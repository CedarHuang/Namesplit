//! Tauri 命令的公共错误形状与少量应用侧命令。
//! 失败统一返回 `{code, message}`：`code` 供前端分支处理，`message` 面向用户与日志。

use std::path::Path;
use std::sync::Arc;

use serde::Serialize;
use tauri::Manager;

use crate::runtime::Runtime;
use crate::webview_guard::RendererGuard;

#[derive(Debug, Serialize)]
pub struct IpcError {
    pub code: String,
    pub message: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub details: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub current_revision: Option<u64>,
}

impl IpcError {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            details: None,
            current_revision: None,
        }
    }

    pub fn with_details(mut self, details: serde_json::Value) -> Self {
        self.details = Some(details);
        self
    }

    pub fn with_current_revision(mut self, revision: u64) -> Self {
        self.current_revision = Some(revision);
        self
    }
}

impl std::fmt::Display for IpcError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}: {}", self.code, self.message)
    }
}

impl std::error::Error for IpcError {}

#[derive(Debug, Serialize)]
pub struct AppInfo {
    pub app_version: String,
    pub commit: String,
    /// sidecar 版本；未从 sidecar 读取时为空字符串。
    pub sidecar_version: String,
    pub data_dir: String,
    pub config_path: String,
    pub querylog_path: String,
    pub log_path: String,
    pub sidecar_log_path: String,
    pub renderer_reloads: u32,
}

/// 返回版本、数据目录与日志路径，供设置页与诊断页展示。
#[tauri::command]
pub fn app_info(
    app: tauri::AppHandle,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<AppInfo, IpcError> {
    let rt = rt.inner();
    let renderer_reloads = app
        .try_state::<Arc<RendererGuard>>()
        .map(|guard| guard.reloads())
        .unwrap_or(0);
    Ok(AppInfo {
        app_version: app.package_info().version.to_string(),
        commit: option_env!("NAMESPLIT_COMMIT").unwrap_or("").to_string(),
        sidecar_version: String::new(),
        data_dir: rt.data_dir.display().to_string(),
        config_path: rt.config_path().display().to_string(),
        querylog_path: rt.querylog_path().display().to_string(),
        log_path: rt.log_path.display().to_string(),
        sidecar_log_path: rt.sidecar_log_path().display().to_string(),
        renderer_reloads,
    })
}

/// 打开数据目录；目录由 sidecar 与 Tauri 应用共用。
#[tauri::command]
pub fn open_data_dir(rt: tauri::State<'_, Arc<Runtime>>) -> Result<serde_json::Value, IpcError> {
    open_path(&rt.inner().data_dir).map(|_| serde_json::json!({ "ok": true }))
}

/// 用系统默认编辑器打开配置文件（文件不存在时由用户先启动一次 sidecar）。
#[tauri::command]
pub fn open_config_file(rt: tauri::State<'_, Arc<Runtime>>) -> Result<serde_json::Value, IpcError> {
    open_path(&rt.inner().config_path()).map(|_| serde_json::json!({ "ok": true }))
}

/// 打开 Windows 的“网络和 Internet”设置页。
#[tauri::command]
pub fn open_system_network_settings() -> Result<serde_json::Value, IpcError> {
    open_uri("ms-settings:network").map(|_| serde_json::json!({ "ok": true }))
}

fn open_path(path: &Path) -> Result<(), IpcError> {
    if !path.exists() {
        return Err(IpcError::new(
            "open_failed",
            format!("路径不存在：{}", path.display()),
        ));
    }
    open_uri(&path.display().to_string())
}

fn open_uri(target: &str) -> Result<(), IpcError> {
    use std::os::windows::process::CommandExt;

    // 避免拉起控制台窗口；URI/路径由系统 shell 处理。
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;

    std::process::Command::new("cmd")
        .args(["/C", "start", "", target])
        .creation_flags(CREATE_NO_WINDOW)
        .spawn()
        .map_err(|error| IpcError::new("open_failed", format!("打开失败：{error}")))?;
    Ok(())
}
