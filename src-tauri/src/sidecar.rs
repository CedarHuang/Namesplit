// sidecar 监护：拉起、发现随机端口、异常退出退避限次重启、Job Object 归组。
use std::io::{BufRead, BufReader};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::AtomicBool;
use std::sync::mpsc;
use std::sync::Arc;
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use serde_json::json;
use tauri::{AppHandle, Emitter};

use crate::job;
use crate::relay::{self, RelayStats, TauriBatchSink};
use crate::runtime::{
    Runtime, BACKOFF_BASE_MS, BACKOFF_MAX_MS, MAX_RESTARTS_PER_WINDOW, RESTART_WINDOW_SECS,
};

const READY_PREFIX: &str = "NAMESPLIT_READY ";
/// sidecar 启动失败时向 stderr 打印的配置错误行；解析后随 failed 状态上报。
const CONFIG_ERROR_PREFIX: &str = "NAMESPLIT_CONFIG_ERROR ";
const READY_TIMEOUT: Duration = Duration::from_secs(5);

/// sidecar 可执行文件位置：优先使用 tauri-build 从 externalBin 复制到当前 exe 同目录的副本，
/// 回退到仓库内构建产物（便于 `cargo run` 直接调试）。
pub fn sidecar_path() -> PathBuf {
    let exe_name = if cfg!(windows) {
        "namesplit-core.exe"
    } else {
        "namesplit-core"
    };
    if let Ok(current) = std::env::current_exe() {
        if let Some(dir) = current.parent() {
            let candidate = dir.join(exe_name);
            if candidate.is_file() {
                return candidate;
            }
        }
    }
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../build")
        .join(exe_name)
}

pub fn supervise(app: AppHandle, rt: Arc<Runtime>) {
    loop {
        if rt.shutting_down.load(std::sync::atomic::Ordering::SeqCst) {
            rt.log("应用正在退出，监护线程结束");
            return;
        }
        let mut relay_stop: Option<Arc<AtomicBool>> = None;
        let mut relay_stats: Option<Arc<RelayStats>> = None;

        let (mut child, stderr_thread) = match spawn_once(&rt) {
            Ok(pair) => pair,
            Err(err) => {
                let reason = format!("拉起 sidecar 失败: {err}");
                mark_failed(&rt, &reason);
                emit_state(&app, &rt, "failed", Some(&reason), None);
                return;
            }
        };
        let spawn_at = Instant::now();
        let ready = wait_ready(&rt, &mut child, READY_TIMEOUT);

        match &ready {
            Some((port, pid)) => {
                rt.set_ready(*port, *pid);
                rt.log(&format!(
                    "sidecar 就绪：127.0.0.1:{port} pid={pid}，spawn→READY {}ms（第 {} 次启动）",
                    spawn_at.elapsed().as_millis(),
                    rt.launch_count.load(std::sync::atomic::Ordering::SeqCst)
                ));
                emit_state(&app, &rt, "running", None, None);

                // READY 之后才建立事件流；端口与 token 只在本进程内使用，不进入 Tauri 事件。
                let stop = Arc::new(AtomicBool::new(false));
                let stats = Arc::new(RelayStats::default());
                let sink = Arc::new(TauriBatchSink::new(app.clone()));
                let _handle = relay::spawn(
                    relay::RelayConfig::new(*port, rt.token.clone()),
                    sink,
                    stop.clone(),
                    stats.clone(),
                );
                relay_stop = Some(stop);
                relay_stats = Some(stats);
            }
            None => {
                rt.log("sidecar 在就绪前退出或就绪超时");
                // 尚未退出才终止；退出状态统一由外层 child.wait() 收口，避免重复 reap。
                if child.try_wait().ok().flatten().is_none() {
                    let _ = child.kill();
                }
            }
        }

        let status = child.wait();
        // 等 stderr 读完，确保 NAMESPLIT_CONFIG_ERROR 行已解析后再判断退出原因。
        if let Some(handle) = stderr_thread {
            let _ = handle.join();
        }
        rt.clear_ready();
        if let Some(stop) = relay_stop.take() {
            stop.store(true, std::sync::atomic::Ordering::SeqCst);
        }
        if let Some(stats) = relay_stats.take() {
            let snapshot = stats.snapshot();
            // 连接失败/401 时 events 为 0，仍需把 last_error 落盘。
            if snapshot.events > 0 || snapshot.dropped_events > 0 || snapshot.last_error.is_some() {
                rt.log(&format!(
                    "事件中继：连接 {} 次、批次 {}、事件 {}、丢弃批次 {}（{} 事件）{}",
                    snapshot.connects,
                    snapshot.batches,
                    snapshot.events,
                    snapshot.dropped_batches,
                    snapshot.dropped_events,
                    snapshot
                        .last_error
                        .map(|err| format!("，最后错误：{err}"))
                        .unwrap_or_default()
                ));
            }
        }
        if rt.shutting_down.load(std::sync::atomic::Ordering::SeqCst) {
            rt.log(&format!("应用退出中，sidecar 已结束：{status:?}"));
            return;
        }

        let exit_reason = format!("sidecar 意外退出：{status:?}");
        rt.log(&exit_reason);

        // 启动配置错误（退出码 3）：不自动重启，进入 failed 并随事件上报 config_error。
        let config_error = rt.take_config_error();
        if status.as_ref().ok().and_then(|status| status.code()) == Some(3)
            || config_error.is_some()
        {
            let reason = config_error
                .as_ref()
                .and_then(|value| value.get("message").and_then(|message| message.as_str()))
                .unwrap_or("sidecar 启动配置错误（退出码 3）")
                .to_string();
            mark_failed(&rt, &reason);
            emit_state(&app, &rt, "failed", Some(&reason), config_error.as_ref());
            return;
        }

        emit_state(&app, &rt, "exited", Some(&exit_reason), None);

        let now = Instant::now();
        let attempt = {
            let mut times = rt.restart_times.lock().unwrap();
            times.retain(|t| now.duration_since(*t) < Duration::from_secs(RESTART_WINDOW_SECS));
            if times.len() >= MAX_RESTARTS_PER_WINDOW {
                None
            } else {
                times.push(now);
                Some(times.len())
            }
        };
        match attempt {
            None => {
                let reason = format!(
                    "10 分钟内已重启 {MAX_RESTARTS_PER_WINDOW} 次，达到上限；进入失败态，不再自动重启（需人工处理）"
                );
                mark_failed(&rt, &reason);
                emit_state(&app, &rt, "failed", Some(&reason), None);
                return;
            }
            Some(n) => {
                let backoff_ms = (BACKOFF_BASE_MS * (1u64 << (n - 1) as u32)).min(BACKOFF_MAX_MS);
                rt.launch_count
                    .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                rt.log(&format!(
                    "第 {n} 次退避重启：{backoff_ms}ms 后重新拉起 sidecar（10 分钟窗口上限 {MAX_RESTARTS_PER_WINDOW} 次）"
                ));
                emit_state(&app, &rt, "restarting", Some(&exit_reason), None);
                std::thread::sleep(Duration::from_millis(backoff_ms));
            }
        }
    }
}

fn spawn_once(rt: &Arc<Runtime>) -> std::io::Result<(Child, Option<JoinHandle<()>>)> {
    let path = sidecar_path();
    rt.log(&format!("拉起 sidecar：{}", path.display()));
    let mut cmd = Command::new(&path);
    cmd.env("NAMESPLIT_TOKEN", &rt.token)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        cmd.creation_flags(CREATE_NO_WINDOW);
    }
    let mut child = cmd.spawn()?;
    match job::global_job() {
        Ok(job) => {
            if let Err(err) = job.assign(child.id()) {
                rt.log(&format!("警告：sidecar 加入 Job Object 失败：{err}"));
            }
        }
        Err(err) => rt.log(&format!("警告：创建 Job Object 失败：{err}")),
    }
    let stderr_thread = child.stderr.take().map(|stderr| {
        let rt = rt.clone();
        std::thread::spawn(move || {
            for line in BufReader::new(stderr).lines().map_while(Result::ok) {
                if let Some(rest) = line.strip_prefix(CONFIG_ERROR_PREFIX) {
                    match serde_json::from_str::<serde_json::Value>(rest) {
                        Ok(value) => rt.set_config_error(value),
                        Err(err) => rt.log(&format!("[sidecar] 配置错误行不是合法 JSON：{err}")),
                    }
                }
                rt.log(&format!("[sidecar] {line}"));
            }
        })
    });
    Ok((child, stderr_thread))
}

/// 读取 stdout 直到 NAMESPLIT_READY 行；同时监视子进程是否提前退出。
fn wait_ready(rt: &Arc<Runtime>, child: &mut Child, timeout: Duration) -> Option<(u16, u32)> {
    let stdout = child.stdout.take()?;
    let (tx, rx) = mpsc::channel::<String>();
    let reader_rt = rt.clone();
    std::thread::spawn(move || {
        for line in BufReader::new(stdout).lines().map_while(Result::ok) {
            if let Some(rest) = line.strip_prefix(READY_PREFIX) {
                let _ = tx.send(rest.to_string());
            } else {
                reader_rt.log(&format!("[sidecar stdout] {line}"));
            }
        }
    });
    let start = Instant::now();
    loop {
        match rx.recv_timeout(Duration::from_millis(50)) {
            Ok(json_line) => {
                let value: serde_json::Value = serde_json::from_str(&json_line).ok()?;
                let port = value.get("port")?.as_u64()? as u16;
                let pid = value.get("pid")?.as_u64()? as u32;
                return Some((port, pid));
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {
                if child.try_wait().ok().flatten().is_some() {
                    return None;
                }
                if start.elapsed() > timeout {
                    return None;
                }
            }
            Err(mpsc::RecvTimeoutError::Disconnected) => return None,
        }
    }
}

fn emit_state(
    app: &AppHandle,
    rt: &Runtime,
    state: &str,
    reason: Option<&str>,
    config_error: Option<&serde_json::Value>,
) {
    // 只上报生命周期状态；端口与 token 留在 Rust 侧，不进入渲染器可见事件。
    let mut payload = json!({
        "state": state,
        "pid": rt.sidecar_pid.load(std::sync::atomic::Ordering::SeqCst),
        "restart_count": rt.restart_count(),
    });
    if let Some(reason) = reason {
        payload["reason"] = json!(reason);
    }
    if let Some(config_error) = config_error {
        payload["config_error"] = config_error.clone();
    }
    let _ = app.emit("sidecar://state", payload);
}

fn mark_failed(rt: &Runtime, reason: &str) {
    rt.failed.store(true, std::sync::atomic::Ordering::SeqCst);
    rt.log(&format!("sidecar 监护失败：{reason}"));
}
