// Namesplit 进程内运行时：启动 token、sidecar 状态与日志。
use std::fs::{File, OpenOptions};
use std::io::Write;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use crate::relay::RelaySession;

pub const MAX_RESTARTS_PER_WINDOW: usize = 3;
pub const RESTART_WINDOW_SECS: u64 = 600;
pub const BACKOFF_BASE_MS: u64 = 250;
pub const BACKOFF_MAX_MS: u64 = 2_000;

/// 固定数据目录 `%LOCALAPPDATA%\Namesplit`，与 sidecar 共用。
pub fn data_dir() -> PathBuf {
    match std::env::var_os("LOCALAPPDATA") {
        Some(base) => PathBuf::from(base).join("Namesplit"),
        None => std::env::temp_dir().join("Namesplit"),
    }
}

/// 最近一次上报的 sidecar 状态；渲染器重载后据此恢复状态展示。
#[derive(Debug, Clone)]
pub struct SidecarStatus {
    pub state: String,
    pub reason: Option<String>,
    pub config_error: Option<serde_json::Value>,
}

pub struct Runtime {
    pub token: String,
    pub log_path: PathBuf,
    pub data_dir: PathBuf,
    pub started: Instant,
    pub failed: AtomicBool,
    pub shutting_down: AtomicBool,
    pub port: Mutex<Option<u16>>,
    pub sidecar_pid: AtomicU32,
    /// 滑动窗口内的重启时刻（10 分钟）。
    pub restart_times: Mutex<Vec<Instant>>,
    pub launch_count: AtomicU64,
    /// sidecar stderr 上报的启动配置错误；由监护线程在进程退出后消费。
    config_error: Mutex<Option<serde_json::Value>>,
    log_file: Mutex<File>,
    /// 监护线程启动权：true 表示已有监护线程在运行或正在启动。
    pub supervisor_running: AtomicBool,
    /// UI 是否已订阅事件流；只有为真且 sidecar 就绪时才建立中继。
    events_subscribed: AtomicBool,
    /// 最近一次 sidecar 状态快照；与 `sidecar://state` 事件载荷同源。
    sidecar_state: Mutex<Option<SidecarStatus>>,
    /// 当前事件中继会话；订阅/取消订阅与 sidecar 退出时在此收口。
    relay: Mutex<Option<RelaySession>>,
}

impl Runtime {
    pub fn new() -> std::io::Result<Self> {
        let data_dir = data_dir();
        std::fs::create_dir_all(&data_dir)?;
        let log_path = data_dir.join("app.log");
        let log_file = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&log_path)?;
        Ok(Self::from_log_file(data_dir, log_path, log_file))
    }

    fn from_log_file(data_dir: PathBuf, log_path: PathBuf, log_file: File) -> Self {
        Self {
            token: generate_token(),
            log_path,
            data_dir,
            started: Instant::now(),
            failed: AtomicBool::new(false),
            shutting_down: AtomicBool::new(false),
            port: Mutex::new(None),
            sidecar_pid: AtomicU32::new(0),
            restart_times: Mutex::new(Vec::new()),
            launch_count: AtomicU64::new(1),
            config_error: Mutex::new(None),
            log_file: Mutex::new(log_file),
            supervisor_running: AtomicBool::new(false),
            events_subscribed: AtomicBool::new(false),
            sidecar_state: Mutex::new(None),
            relay: Mutex::new(None),
        }
    }

    pub fn config_path(&self) -> PathBuf {
        self.data_dir.join("config.toml")
    }

    pub fn querylog_path(&self) -> PathBuf {
        self.data_dir.join("querylog.db")
    }

    pub fn sidecar_log_path(&self) -> PathBuf {
        self.data_dir.join("sidecar.log")
    }

    /// 清除失败态与重启窗口，供手动重启使用。
    pub fn reset_failure(&self) {
        self.failed.store(false, Ordering::SeqCst);
        self.restart_times.lock().unwrap().clear();
        self.launch_count.store(1, Ordering::SeqCst);
    }

    /// 原子抢占监护线程启动权；只有第一个调用者成功，成功方负责启动 supervise。
    ///
    /// 手动重启与启动流程可能并发触发，先读后写会让两方都以为自己是唯一启动者，
    /// 进而拉起两个 sidecar；因此用一次 CAS 把"能否启动"变成单次判定。
    pub fn claim_supervisor(&self) -> bool {
        self.supervisor_running
            .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
            .is_ok()
    }

    /// 当前 sidecar 端口；未就绪时为 None。
    pub fn port(&self) -> Option<u16> {
        self.port.lock().ok().and_then(|slot| *slot)
    }

    /// UI 是否已订阅事件流。
    pub fn events_subscribed(&self) -> bool {
        self.events_subscribed.load(Ordering::SeqCst)
    }

    pub fn set_events_subscribed(&self, subscribed: bool) {
        self.events_subscribed.store(subscribed, Ordering::SeqCst);
    }

    /// 在同一把锁内替换中继槽：旧会话先置 `stop` 再让位，新会话即刻生效。
    ///
    /// 锁内不做 join：旧读线程最长要等 10s 读超时才退出，持锁等待会阻塞订阅与退出路径。
    /// 返回的旧句柄由调用方丢到后台线程回收。
    pub fn replace_relay(&self, next: Option<RelaySession>) -> Option<RelaySession> {
        match self.relay.lock() {
            Ok(mut slot) => {
                let previous = std::mem::replace(&mut *slot, next);
                if let Some(previous) = previous.as_ref() {
                    previous.stop();
                }
                previous
            }
            // 锁中毒时无法安全替换：新会话随本函数返回被丢弃（Drop 会置 stop）。
            Err(_) => None,
        }
    }

    /// 记录 sidecar 状态并返回与 `sidecar://state` 完全一致的载荷。
    pub fn record_sidecar_state(
        &self,
        state: &str,
        reason: Option<&str>,
        config_error: Option<&serde_json::Value>,
    ) -> serde_json::Value {
        let status = SidecarStatus {
            state: state.to_string(),
            reason: reason.map(str::to_string),
            config_error: config_error.cloned(),
        };
        let payload = self.state_payload(&status);
        if let Ok(mut slot) = self.sidecar_state.lock() {
            *slot = Some(status);
        }
        payload
    }

    /// 返回最近一次 sidecar 状态载荷；从未上报过时为 None。
    pub fn sidecar_state_payload(&self) -> Option<serde_json::Value> {
        let status = self.sidecar_state.lock().ok()?.clone()?;
        Some(self.state_payload(&status))
    }

    /// 状态事件与状态查询共用同一序列化逻辑，保证字段一致。
    fn state_payload(&self, status: &SidecarStatus) -> serde_json::Value {
        let mut payload = serde_json::json!({
            "state": status.state,
            "pid": self.sidecar_pid.load(Ordering::SeqCst),
            "restart_count": self.restart_count(),
        });
        if let Some(reason) = &status.reason {
            payload["reason"] = serde_json::json!(reason);
        }
        if let Some(config_error) = &status.config_error {
            payload["config_error"] = config_error.clone();
        }
        payload
    }

    /// 追加一行日志：既有相对启动时间的单调时间戳，也有 epoch 毫秒，便于排查。
    pub fn log(&self, msg: &str) {
        let epoch_ms = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_millis())
            .unwrap_or(0);
        let line = format!(
            "[+{:>7}ms][{}] {}",
            self.started.elapsed().as_millis(),
            epoch_ms,
            msg
        );
        if let Ok(mut f) = self.log_file.lock() {
            let _ = writeln!(f, "{line}");
            let _ = f.flush();
        }
        eprintln!("{line}");
    }

    pub fn set_ready(&self, port: u16, pid: u32) {
        *self.port.lock().unwrap() = Some(port);
        self.sidecar_pid.store(pid, Ordering::SeqCst);
    }

    pub fn clear_ready(&self) {
        *self.port.lock().unwrap() = None;
        self.sidecar_pid.store(0, Ordering::SeqCst);
    }

    /// 10 分钟滑动窗口内的重启次数。
    pub fn restart_count(&self) -> usize {
        let now = Instant::now();
        let window = Duration::from_secs(RESTART_WINDOW_SECS);
        self.restart_times
            .lock()
            .map(|times| {
                times
                    .iter()
                    .filter(|time| now.saturating_duration_since(**time) < window)
                    .count()
            })
            .unwrap_or(0)
    }

    /// 记录 sidecar stderr 上报的启动配置错误。
    pub fn set_config_error(&self, value: serde_json::Value) {
        *self.config_error.lock().unwrap() = Some(value);
    }

    /// 取出并清空启动配置错误；进程退出后只消费一次。
    pub fn take_config_error(&self) -> Option<serde_json::Value> {
        self.config_error.lock().unwrap().take()
    }

    /// 测试用实例：不写真实数据目录与 app.log；每次调用使用独立目录，避免并行用例互相覆盖。
    #[cfg(test)]
    pub(crate) fn for_test() -> Self {
        static COUNTER: AtomicU64 = AtomicU64::new(0);
        let dir = std::env::temp_dir().join(format!(
            "namesplit-runtime-test-{}-{}",
            std::process::id(),
            COUNTER.fetch_add(1, Ordering::Relaxed)
        ));
        std::fs::create_dir_all(&dir).expect("创建测试数据目录");
        let log_path = dir.join("app.log");
        let log_file = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&log_path)
            .expect("打开测试日志文件");
        Self::from_log_file(dir, log_path, log_file)
    }
}

pub fn generate_token() -> String {
    let mut buf = [0u8; 32];
    getrandom::fill(&mut buf).expect("获取系统随机数失败");
    hex(&buf)
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(s, "{b:02x}");
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sidecar_state_payload_is_none_before_first_record() {
        let rt = Runtime::for_test();
        assert!(rt.sidecar_state_payload().is_none());
    }

    #[test]
    fn record_sidecar_state_returns_event_shaped_payload() {
        let rt = Runtime::for_test();
        rt.set_ready(1234, 4321);

        let payload = rt.record_sidecar_state("running", None, None);
        assert_eq!(payload["state"], "running");
        assert_eq!(payload["pid"], 4321);
        assert_eq!(payload["restart_count"], 0);
        assert!(payload.get("reason").is_none());
        assert!(payload.get("config_error").is_none());

        let config_error = serde_json::json!({"code": "invalid_toml", "message": "第 3 行"});
        let payload = rt.record_sidecar_state("failed", Some("启动配置错误"), Some(&config_error));
        assert_eq!(payload["state"], "failed");
        assert_eq!(payload["reason"], "启动配置错误");
        assert_eq!(payload["config_error"], config_error);

        // 查询得到的载荷必须与最近一次事件同形。
        assert_eq!(rt.sidecar_state_payload(), Some(payload));
    }

    #[test]
    fn restart_count_only_counts_entries_inside_window() {
        let rt = Runtime::for_test();
        {
            let mut times = rt.restart_times.lock().expect("重启窗口锁");
            times.push(Instant::now() - Duration::from_secs(RESTART_WINDOW_SECS + 1));
            times.push(Instant::now());
        }
        assert_eq!(rt.restart_count(), 1);
    }

    #[test]
    fn concurrent_claims_have_exactly_one_winner() {
        use std::sync::atomic::AtomicUsize;
        use std::sync::Arc;

        const THREADS: usize = 8;
        let rt = Arc::new(Runtime::for_test());
        let winners = Arc::new(AtomicUsize::new(0));
        let mut handles = Vec::new();
        for _ in 0..THREADS {
            let rt = rt.clone();
            let winners = winners.clone();
            handles.push(std::thread::spawn(move || {
                if rt.claim_supervisor() {
                    winners.fetch_add(1, Ordering::SeqCst);
                }
            }));
        }
        for handle in handles {
            handle.join().expect("抢占线程应正常结束");
        }
        assert_eq!(
            winners.load(Ordering::SeqCst),
            1,
            "只能有一个线程抢到启动权"
        );

        // 监护线程退出（SupervisorFlag 释放）后必须可以再次抢占。
        rt.supervisor_running.store(false, Ordering::SeqCst);
        assert!(rt.claim_supervisor());
        assert!(!rt.claim_supervisor());
    }
}
