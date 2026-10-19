//! 渲染进程健康监测：3s 心跳 + 10s 超时重载。
//!
//! WebView2 的 `ProcessFailed` 事件钩子（`with_webview` + `webview2-com`）用于即时感知；
//! 心跳负责覆盖渲染进程挂起、钩子丢失等场景，两者叠加而不是互相替代。
//! 重载最多 5 次，超过后停止自动重载，由前端提示用户手动重启窗口。

use std::sync::atomic::{AtomicU32, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tauri::{AppHandle, Manager};

use crate::ipc::IpcError;
use crate::runtime::Runtime;

const CHECK_INTERVAL: Duration = Duration::from_secs(2);
const STALE_AFTER_MS: u64 = 10_000;
const MAX_RELOADS: u32 = 5;

pub struct RendererGuard {
    last_heartbeat_ms: AtomicU64,
    reloads: AtomicU32,
}

impl RendererGuard {
    pub fn start(app: AppHandle) -> Arc<Self> {
        let guard = Arc::new(Self {
            last_heartbeat_ms: AtomicU64::new(now_ms()),
            reloads: AtomicU32::new(0),
        });
        let monitor = guard.clone();
        std::thread::spawn(move || loop {
            std::thread::sleep(CHECK_INTERVAL);
            if app
                .try_state::<Arc<Runtime>>()
                .is_some_and(|rt| rt.shutting_down.load(Ordering::SeqCst))
            {
                return;
            }

            // 隐藏或最小化的窗口会被 WebView2 节流定时器，心跳变慢不代表渲染进程异常；
            // 真正的崩溃由 ProcessFailed 钩子感知。
            if let Some(window) = app.get_webview_window("main") {
                let hidden = window.is_visible().map(|visible| !visible).unwrap_or(false)
                    || window.is_minimized().unwrap_or(false);
                if hidden {
                    monitor.last_heartbeat_ms.store(now_ms(), Ordering::SeqCst);
                    continue;
                }
            }

            if now_ms().saturating_sub(monitor.last_heartbeat_ms.load(Ordering::SeqCst))
                <= STALE_AFTER_MS
            {
                continue;
            }

            let counts = monitor.reloads.fetch_add(1, Ordering::SeqCst) + 1;
            if counts > MAX_RELOADS {
                monitor.reloads.store(MAX_RELOADS, Ordering::SeqCst);
                log(
                    &app,
                    "渲染进程持续无心跳：已达重载上限，等待用户手动重启窗口",
                );
                return;
            }
            log(
                &app,
                &format!("渲染进程无心跳超过 10s：重载窗口（第 {counts}/{MAX_RELOADS} 次）"),
            );
            monitor.last_heartbeat_ms.store(now_ms(), Ordering::SeqCst);
            if let Some(window) = app.get_webview_window("main") {
                let _ = window.reload();
            }
        });
        guard
    }

    pub fn reloads(&self) -> u32 {
        self.reloads.load(Ordering::SeqCst)
    }

    /// ProcessFailed 钩子触发重载前调用；返回累计次数，超过上限时返回 > MAX_RELOADS。
    pub fn note_reload(&self) -> u32 {
        let count = self.reloads.fetch_add(1, Ordering::SeqCst) + 1;
        if count > MAX_RELOADS {
            self.reloads.store(MAX_RELOADS, Ordering::SeqCst);
        }
        count
    }

    fn heartbeat(&self) -> u32 {
        self.last_heartbeat_ms.store(now_ms(), Ordering::SeqCst);
        self.reloads()
    }
}

/// 前端每 3s 调用一次；返回值用于重载后提示。
#[tauri::command]
pub fn ui_heartbeat(
    guard: tauri::State<'_, Arc<RendererGuard>>,
) -> Result<serde_json::Value, IpcError> {
    let reloads = guard.heartbeat();
    Ok(serde_json::json!({ "ok": true, "renderer_reloads": reloads }))
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis() as u64)
        .unwrap_or(0)
}

/// 注册 WebView2 `ProcessFailed` 事件钩子：即时记录失败类型并触发一次重载，
/// 心跳仍作为挂起/钩子失效时的兜底。
#[cfg(windows)]
pub fn install_process_failed_hook(app: &AppHandle) {
    use webview2_com::Microsoft::Web::WebView2::Win32::{
        ICoreWebView2Controller, COREWEBVIEW2_PROCESS_FAILED_KIND,
        COREWEBVIEW2_PROCESS_FAILED_KIND_BROWSER_PROCESS_EXITED,
        COREWEBVIEW2_PROCESS_FAILED_KIND_FRAME_RENDER_PROCESS_EXITED,
        COREWEBVIEW2_PROCESS_FAILED_KIND_RENDER_PROCESS_EXITED,
        COREWEBVIEW2_PROCESS_FAILED_KIND_RENDER_PROCESS_UNRESPONSIVE,
    };
    use webview2_com::ProcessFailedEventHandler;

    let Some(window) = app.get_webview_window("main") else {
        return;
    };
    let app_handle = app.clone();
    let result = window.with_webview(move |webview| {
        let controller: ICoreWebView2Controller = webview.controller();
        let core = match unsafe { controller.CoreWebView2() } {
            Ok(core) => core,
            Err(error) => {
                log(
                    &app_handle,
                    &format!("注册 WebView2 ProcessFailed 失败：{error}"),
                );
                return;
            }
        };

        let handler_app = app_handle.clone();
        let handler = ProcessFailedEventHandler::create(Box::new(move |_sender, args| {
            let mut kind = COREWEBVIEW2_PROCESS_FAILED_KIND(0);
            if let Some(args) = args {
                unsafe {
                    let _ = args.ProcessFailedKind(&mut kind);
                }
            }
            let relevant = kind.0 == COREWEBVIEW2_PROCESS_FAILED_KIND_BROWSER_PROCESS_EXITED.0
                || kind.0 == COREWEBVIEW2_PROCESS_FAILED_KIND_RENDER_PROCESS_EXITED.0
                || kind.0 == COREWEBVIEW2_PROCESS_FAILED_KIND_RENDER_PROCESS_UNRESPONSIVE.0
                || kind.0 == COREWEBVIEW2_PROCESS_FAILED_KIND_FRAME_RENDER_PROCESS_EXITED.0;
            log(
                &handler_app,
                &format!("WebView2 进程失败事件：kind={kind:?} relevant={relevant}"),
            );
            if !relevant {
                return Ok(());
            }
            let reloads = handler_app
                .try_state::<Arc<RendererGuard>>()
                .map(|guard| guard.note_reload())
                .unwrap_or(0);
            if reloads > MAX_RELOADS {
                log(
                    &handler_app,
                    "渲染进程持续失败：已达重载上限，等待用户手动重启窗口",
                );
                return Ok(());
            }
            log(
                &handler_app,
                &format!("渲染进程失败：重载窗口（第 {reloads}/{MAX_RELOADS} 次）"),
            );
            let window = handler_app.get_webview_window("main");
            // 事件回调内不直接重载，避免 WebView2 在回调栈中重入。
            std::thread::spawn(move || {
                std::thread::sleep(Duration::from_millis(100));
                if let Some(window) = window {
                    let _ = window.reload();
                }
            });
            Ok(())
        }));

        let mut token = 0i64;
        unsafe {
            if let Err(error) = core.add_ProcessFailed(&handler, &mut token) {
                log(
                    &app_handle,
                    &format!("注册 WebView2 ProcessFailed 失败：{error}"),
                );
            }
        }
    });

    if let Err(error) = result {
        log(app, &format!("with_webview 调用失败：{error}"));
    }
}

#[cfg(not(windows))]
pub fn install_process_failed_hook(_app: &AppHandle) {}

fn log(app: &AppHandle, message: &str) {
    if let Some(rt) = app.try_state::<Arc<Runtime>>() {
        rt.log(message);
    } else {
        eprintln!("{message}");
    }
}
