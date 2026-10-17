// Namesplit 进程内运行时：启动 token、sidecar 状态与日志。
use std::fs::{File, OpenOptions};
use std::io::Write;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU32, AtomicU64, Ordering};
use std::sync::Mutex;
use std::time::{Instant, SystemTime, UNIX_EPOCH};

use serde::Serialize;

pub const MAX_RESTARTS_PER_WINDOW: usize = 3;
pub const RESTART_WINDOW_SECS: u64 = 600;
pub const BACKOFF_BASE_MS: u64 = 250;
pub const BACKOFF_MAX_MS: u64 = 2_000;

pub struct Runtime {
    pub token: String,
    pub log_path: PathBuf,
    pub started: Instant,
    pub failed: AtomicBool,
    pub shutting_down: AtomicBool,
    pub port: Mutex<Option<u16>>,
    pub sidecar_pid: AtomicU32,
    /// 滑动窗口内的重启时刻（10 分钟）。
    pub restart_times: Mutex<Vec<Instant>>,
    pub launch_count: AtomicU64,
    log_file: Mutex<File>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ConnInfo {
    pub port: Option<u16>,
    pub token: String,
    pub sidecar_pid: u32,
    pub restart_count: usize,
    pub failed: bool,
    pub log_path: String,
}

impl Runtime {
    pub fn new() -> std::io::Result<Self> {
        let log_path = std::env::temp_dir().join("namesplit.log");
        let log_file = OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .open(&log_path)?;
        Ok(Self {
            token: generate_token(),
            log_path,
            started: Instant::now(),
            failed: AtomicBool::new(false),
            shutting_down: AtomicBool::new(false),
            port: Mutex::new(None),
            sidecar_pid: AtomicU32::new(0),
            restart_times: Mutex::new(Vec::new()),
            launch_count: AtomicU64::new(1),
            log_file: Mutex::new(log_file),
        })
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

    pub fn restart_count(&self) -> usize {
        self.restart_times.lock().unwrap().len()
    }

    pub fn conn_info(&self) -> ConnInfo {
        ConnInfo {
            port: *self.port.lock().unwrap(),
            token: self.token.clone(),
            sidecar_pid: self.sidecar_pid.load(Ordering::SeqCst),
            restart_count: self.restart_count(),
            failed: self.failed.load(Ordering::SeqCst),
            log_path: self.log_path.display().to_string(),
        }
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
