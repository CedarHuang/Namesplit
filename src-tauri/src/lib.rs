// Namesplit 应用侧：窗口/托盘/单实例/sidecar 监护。
mod job;
mod runtime;
mod sidecar;

use std::sync::Arc;

use serde_json::json;
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Emitter, Manager, State, WindowEvent};

use runtime::{ConnInfo, Runtime};

#[tauri::command]
fn get_connection(rt: State<'_, Arc<Runtime>>) -> ConnInfo {
    rt.conn_info()
}

fn show_main(app: &AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
    }
}

fn setup_tray(app: &tauri::App) -> tauri::Result<()> {
    let open = MenuItem::with_id(app, "open", "打开主窗口", true, None::<&str>)?;
    let clear = MenuItem::with_id(app, "clear_logs", "清空日志视图", true, None::<&str>)?;
    let separator = PredefinedMenuItem::separator(app)?;
    let quit = MenuItem::with_id(app, "quit", "退出", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&open, &clear, &separator, &quit])?;

    let icon = app
        .default_window_icon()
        .cloned()
        .ok_or_else(|| tauri::Error::AssetNotFound("default window icon".into()))?;

    TrayIconBuilder::with_id("main-tray")
        .icon(icon)
        .tooltip("Namesplit")
        .menu(&menu)
        .show_menu_on_left_click(false)
        .on_menu_event(|app, event| match event.id.as_ref() {
            "open" => show_main(app),
            "clear_logs" => {
                let _ = app.emit("logs://clear", json!({}));
            }
            "quit" => {
                if let Some(rt) = app.try_state::<Arc<Runtime>>() {
                    rt.shutting_down
                        .store(true, std::sync::atomic::Ordering::SeqCst);
                    rt.log("托盘菜单退出：应用退出（sidecar 由 Job Object 一并终止）");
                }
                app.exit(0);
            }
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                show_main(tray.app_handle());
            }
        })
        .build(app)?;
    Ok(())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        // 单实例：第二个实例启动时只唤起已有主窗口，不再拉起第二份 sidecar。
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            show_main(app);
        }))
        .invoke_handler(tauri::generate_handler![get_connection])
        .setup(|app| {
            let rt = Arc::new(Runtime::new()?);
            rt.log(&format!(
                "Namesplit 启动 version={}",
                app.package_info().version
            ));
            let sidecar_path = sidecar::sidecar_path();
            rt.log(&format!("sidecar 路径：{}", sidecar_path.display()));
            if !sidecar_path.is_file() {
                rt.log("警告：sidecar 可执行文件不存在，监护将以失败态结束");
            }
            app.manage(rt.clone());
            setup_tray(app)?;

            let handle = app.handle().clone();
            std::thread::spawn(move || sidecar::supervise(handle, rt));
            Ok(())
        })
        .on_window_event(|window, event| {
            if let WindowEvent::CloseRequested { api, .. } = event {
                // 关闭主窗口仅隐藏到托盘，解析继续运行。
                api.prevent_close();
                let _ = window.hide();
                if let Some(rt) = window.app_handle().try_state::<Arc<Runtime>>() {
                    rt.log("主窗口关闭请求：已隐藏到托盘，Go sidecar 继续运行");
                }
            }
        })
        .run(tauri::generate_context!())
        .expect("Namesplit 启动失败");
}
