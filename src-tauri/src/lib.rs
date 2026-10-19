// Namesplit 应用侧：窗口/托盘/单实例/sidecar 监护。
mod autostart;
mod ipc;
mod job;
pub mod port_status;
pub mod relay;
pub mod runtime;
mod sidecar;
pub mod sidecar_client;
pub mod system_dns;
mod timefmt;

use std::sync::Arc;

use serde_json::json;
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Emitter, Manager, WindowEvent};

use runtime::Runtime;

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
            "quit" => app.exit(0),
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

/// 应用退出的唯一入口：先经过退出提醒，再设置退出标志。
/// 所有退出路径最终都通过 `RunEvent::ExitRequested` 进入这里，
/// 避免每个调用方各自实现一套确认逻辑。
fn request_exit(app: &AppHandle) {
    let Some(rt) = app.try_state::<Arc<Runtime>>() else {
        // Runtime 尚未建立时没有可提醒的状态，直接退出。
        app.exit(0);
        return;
    };

    if rt.shutting_down.load(std::sync::atomic::Ordering::SeqCst) {
        return;
    }

    if !system_dns::confirm_exit_if_needed(app) {
        rt.log("用户取消退出：应用继续运行");
        return;
    }

    rt.shutting_down
        .store(true, std::sync::atomic::Ordering::SeqCst);
    rt.log("退出确认完成：应用退出（sidecar 由 Job Object 一并终止）");
    app.exit(0);
}

/// 拦截所有退出请求：未确认过退出时阻止退出并走统一确认流程。
fn on_exit_requested(app: &AppHandle, api: &tauri::ExitRequestApi) {
    let Some(rt) = app.try_state::<Arc<Runtime>>() else {
        return;
    };
    if rt.shutting_down.load(std::sync::atomic::Ordering::SeqCst) {
        // 已经确认过退出，放行本次请求。
        return;
    }

    api.prevent_exit();
    request_exit(app);
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        // 单实例：第二个实例启动时只唤起已有主窗口，不再拉起第二份 sidecar。
        .plugin(tauri_plugin_single_instance::init(|app, _argv, _cwd| {
            show_main(app);
        }))
        .invoke_handler(tauri::generate_handler![
            port_status::port_status,
            system_dns::system_dns_get,
            autostart::autostart_get,
            autostart::autostart_set,
            ipc::open_system_network_settings
        ])
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

            let monitor = system_dns::DnsMonitor::start(app.handle().clone());
            app.manage(monitor);

            let handle = app.handle().clone();
            std::thread::spawn(move || sidecar::supervise(handle, rt));
            Ok(())
        })
        .on_window_event(|window, event| match event {
            WindowEvent::Focused(true) => {
                // 窗口重新获得焦点时兜底刷新：手工改 DNS 不一定产生网卡变化通知。
                // 只投递刷新请求，系统调用由监听线程执行，避免阻塞事件循环。
                if let Some(monitor) = window
                    .app_handle()
                    .try_state::<Arc<system_dns::DnsMonitor>>()
                {
                    monitor.request_refresh();
                }
            }
            WindowEvent::CloseRequested { api, .. } => {
                // 关闭主窗口仅隐藏到托盘，解析继续运行。
                api.prevent_close();
                let _ = window.hide();
                if let Some(rt) = window.app_handle().try_state::<Arc<Runtime>>() {
                    rt.log("主窗口关闭请求：已隐藏到托盘，Go sidecar 继续运行");
                }
            }
            _ => {}
        })
        .build(tauri::generate_context!())
        .expect("Namesplit 启动失败")
        .run(|app, event| {
            if let tauri::RunEvent::ExitRequested { api, .. } = event {
                on_exit_requested(app, &api);
            }
            // 覆盖范围说明：Windows 关机/注销由 tao 直接销毁事件循环
            // （tao 尚未处理 WM_QUERYENDSESSION），不会进入 ExitRequested，
            // 因此无法弹确认框；该路径不属于应用主动退出流程。
        });
}
