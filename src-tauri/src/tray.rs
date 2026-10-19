//! 托盘三态图标：运行中 / 未指向本机 / 解析服务异常。
//!
//! 图标由应用图标在内存中叠加角标生成，不引入额外图片资源；状态刷新只在
//! sidecar 状态或系统 DNS 变化时触发，避免定时轮询。

use std::sync::atomic::Ordering;
use std::sync::Arc;

use tauri::image::Image;
use tauri::AppHandle;
use tauri::Manager;

use crate::runtime::Runtime;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Status {
    Ok,
    Warning,
    Error,
}

#[derive(Clone)]
pub struct TrayIcons {
    ok: Image<'static>,
    warning: Image<'static>,
    error: Image<'static>,
}

impl TrayIcons {
    pub fn create(base: &Image<'_>) -> Self {
        Self {
            ok: with_dot(base, [34, 197, 94, 255]),
            warning: with_dot(base, [245, 158, 11, 255]),
            error: with_dot(base, [239, 68, 68, 255]),
        }
    }

    pub fn image(&self, status: Status) -> Image<'static> {
        match status {
            Status::Ok => self.ok.clone(),
            Status::Warning => self.warning.clone(),
            Status::Error => self.error.clone(),
        }
    }
}

/// 根据 sidecar 与系统 DNS 的当前状态刷新托盘图标和提示。
pub fn refresh_status(app: &AppHandle) {
    let Some(icons) = app.try_state::<TrayIcons>() else {
        return;
    };
    let Some(rt) = app.try_state::<Arc<Runtime>>() else {
        return;
    };

    let status = if rt.failed.load(Ordering::SeqCst) {
        Status::Error
    } else if rt.sidecar_pid.load(Ordering::SeqCst) == 0 {
        Status::Warning
    } else {
        match crate::system_dns::snapshot() {
            Ok(snapshot)
                if !snapshot.interfaces.is_empty()
                    && snapshot
                        .interfaces
                        .iter()
                        .all(|item| !item.points_to_loopback) =>
            {
                Status::Warning
            }
            Ok(_) => Status::Ok,
            Err(_) => Status::Warning,
        }
    };

    let tooltip = match status {
        Status::Ok => "Namesplit：运行中",
        Status::Warning => "Namesplit：网卡 DNS 未指向本机",
        Status::Error => "Namesplit：解析服务异常",
    };

    if let Some(tray) = app.tray_by_id("main-tray") {
        let _ = tray.set_icon(Some(icons.image(status)));
        let _ = tray.set_tooltip(Some(tooltip));
    }
}

fn with_dot(base: &Image<'_>, color: [u8; 4]) -> Image<'static> {
    let width = base.width();
    let height = base.height();
    let mut rgba = base.rgba().to_vec();
    let radius = (width.min(height) as f32 * 0.22).max(3.0);
    let center_x = width as f32 - radius - 1.0;
    let center_y = height as f32 - radius - 1.0;

    for y in 0..height {
        for x in 0..width {
            let dx = x as f32 - center_x;
            let dy = y as f32 - center_y;
            if dx * dx + dy * dy <= radius * radius {
                let offset = ((y * width + x) * 4) as usize;
                rgba[offset..offset + 4].copy_from_slice(&color);
            }
        }
    }

    Image::new_owned(rgba, width, height)
}
