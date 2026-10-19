//! Tauri 命令的公共错误形状与少量应用侧命令。
//! 失败统一返回 `{code, message}`：`code` 供前端分支处理，`message` 面向用户与日志。

use serde::Serialize;

#[derive(Debug, Serialize)]
pub struct IpcError {
    pub code: &'static str,
    pub message: String,
}

impl IpcError {
    pub fn new(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
        }
    }
}

impl std::fmt::Display for IpcError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for IpcError {}

/// 打开 Windows 的“网络和 Internet”设置页。
#[tauri::command]
pub fn open_system_network_settings() -> Result<serde_json::Value, IpcError> {
    use std::os::windows::process::CommandExt;

    // 避免拉起控制台窗口；URI 由系统 shell 处理。
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;

    std::process::Command::new("cmd")
        .args(["/C", "start", "", "ms-settings:network"])
        .creation_flags(CREATE_NO_WINDOW)
        .spawn()
        .map_err(|error| IpcError::new("open_failed", format!("打开系统网络设置失败：{error}")))?;

    Ok(serde_json::json!({ "ok": true }))
}
