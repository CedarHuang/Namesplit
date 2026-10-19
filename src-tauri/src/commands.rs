//! 转发到 sidecar 内部 API 的 IPC 命令。
//!
//! 渲染器拿不到端口与 token；所有请求在这里构造并附鉴权头。
//! 失败统一转换为 `IpcError`，前端按 `code` 分支处理，`message` 只作诊断补充。

use std::sync::Arc;

use serde::Deserialize;
use serde_json::{json, Value};
use tauri::AppHandle;

use crate::ipc::IpcError;
use crate::runtime::Runtime;
use crate::sidecar_client::{ClientError, SidecarClient};

#[derive(Debug, Default, Deserialize)]
pub struct QueryFilter {
    pub limit: Option<u32>,
    pub cursor: Option<String>,
    pub domain: Option<String>,
    pub qtype: Option<String>,
    pub source: Option<String>,
    pub group: Option<String>,
    pub rule: Option<String>,
    pub upstream: Option<String>,
    pub rcode: Option<i64>,
    pub min_duration_ms: Option<f64>,
    pub max_duration_ms: Option<f64>,
    pub from: Option<String>,
    pub to: Option<String>,
}

fn client(rt: &Runtime) -> Result<SidecarClient, IpcError> {
    let port = rt
        .port()
        .ok_or_else(|| IpcError::new("sidecar_unavailable", "sidecar 未运行"))?;
    Ok(SidecarClient::new(port, rt.token.clone()))
}

fn map_client_error(error: ClientError) -> IpcError {
    match error {
        ClientError::Http { status, body } => {
            let value: Value = serde_json::from_str(&body).unwrap_or(Value::Null);
            let code = value
                .get("error")
                .and_then(|item| item.as_str())
                .map(str::to_string)
                .unwrap_or_else(|| default_code(status).to_string());
            let message = value
                .get("message")
                .and_then(|item| item.as_str())
                .map(str::to_string)
                .unwrap_or_else(|| format!("sidecar 返回 HTTP {status}"));
            let mut mapped = IpcError::new(code, message);
            if let Some(details) = value.get("details") {
                mapped = mapped.with_details(details.clone());
            }
            if let Some(revision) = value.get("current_revision").and_then(|item| item.as_u64()) {
                mapped = mapped.with_current_revision(revision);
            }
            mapped
        }
        ClientError::Io(error) => {
            IpcError::new("sidecar_unavailable", format!("连接 sidecar 失败：{error}"))
        }
        other => IpcError::new("internal", other.to_string()),
    }
}

fn default_code(status: u16) -> &'static str {
    match status {
        400 => "invalid_request",
        401 => "unauthorized",
        404 => "not_found",
        405 => "method_not_allowed",
        409 => "revision_conflict",
        413 => "payload_too_large",
        415 => "unsupported_media_type",
        422 => "invalid_config",
        503 => "not_ready",
        _ => "internal",
    }
}

fn get(rt: tauri::State<'_, Arc<Runtime>>, path: &str) -> Result<Value, IpcError> {
    client(rt.inner())?.get_json(path).map_err(map_client_error)
}

#[tauri::command]
pub fn config_get(rt: tauri::State<'_, Arc<Runtime>>) -> Result<Value, IpcError> {
    get(rt, "/api/config")
}

#[tauri::command]
pub fn config_validate(
    config: Value,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    client(rt.inner())?
        .post_json_body("/api/config/validate", &json!({ "config": config }))
        .map_err(map_client_error)
}

#[tauri::command(rename_all = "snake_case")]
pub fn config_save(
    base_revision: u64,
    config: Value,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    client(rt.inner())?
        .put_json_body(
            "/api/config",
            &json!({ "base_revision": base_revision, "config": config }),
        )
        .map_err(map_client_error)
}

#[tauri::command(rename_all = "snake_case")]
pub fn config_reset(
    base_revision: u64,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    client(rt.inner())?
        .post_json_body(
            "/api/config/reset",
            &json!({ "base_revision": base_revision }),
        )
        .map_err(map_client_error)
}

#[tauri::command]
pub fn rules_test(
    domain: String,
    qtype: Option<String>,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    let mut body = json!({ "domain": domain });
    if let Some(qtype) = qtype {
        body["qtype"] = json!(qtype);
    }
    client(rt.inner())?
        .post_json_body("/api/rules/test", &body)
        .map_err(map_client_error)
}

#[tauri::command(rename_all = "snake_case")]
pub fn queries_list(
    filter: Option<QueryFilter>,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    let filter = filter.unwrap_or_default();
    let mut params: Vec<(&str, String)> = Vec::new();
    if let Some(value) = filter.limit {
        params.push(("limit", value.to_string()));
    }
    if let Some(value) = filter.cursor {
        params.push(("cursor", value));
    }
    if let Some(value) = filter.domain {
        params.push(("domain", value));
    }
    if let Some(value) = filter.qtype {
        params.push(("qtype", value));
    }
    if let Some(value) = filter.source {
        params.push(("source", value));
    }
    if let Some(value) = filter.group {
        params.push(("group", value));
    }
    if let Some(value) = filter.rule {
        params.push(("rule", value));
    }
    if let Some(value) = filter.upstream {
        params.push(("upstream", value));
    }
    if let Some(value) = filter.rcode {
        params.push(("rcode", value.to_string()));
    }
    if let Some(value) = filter.min_duration_ms {
        params.push(("min_duration_ms", value.to_string()));
    }
    if let Some(value) = filter.max_duration_ms {
        params.push(("max_duration_ms", value.to_string()));
    }
    if let Some(value) = filter.from {
        params.push(("from", value));
    }
    if let Some(value) = filter.to {
        params.push(("to", value));
    }
    let path = if params.is_empty() {
        "/api/queries".to_string()
    } else {
        format!(
            "/api/queries?{}",
            params
                .iter()
                .map(|(key, value)| format!("{key}={}", url_encode(value)))
                .collect::<Vec<_>>()
                .join("&")
        )
    };
    get(rt, &path)
}

#[tauri::command]
pub fn queries_clear(rt: tauri::State<'_, Arc<Runtime>>) -> Result<Value, IpcError> {
    client(rt.inner())?
        .delete_json("/api/queries")
        .map_err(map_client_error)
}

#[tauri::command]
pub fn stats_get(rt: tauri::State<'_, Arc<Runtime>>) -> Result<Value, IpcError> {
    get(rt, "/api/stats")
}

#[tauri::command]
pub fn stats_minute(
    from: Option<String>,
    to: Option<String>,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    let mut params: Vec<(&str, String)> = Vec::new();
    if let Some(value) = from {
        params.push(("from", value));
    }
    if let Some(value) = to {
        params.push(("to", value));
    }
    let path = if params.is_empty() {
        "/api/stats/minute".to_string()
    } else {
        format!(
            "/api/stats/minute?{}",
            params
                .iter()
                .map(|(key, value)| format!("{key}={}", url_encode(value)))
                .collect::<Vec<_>>()
                .join("&")
        )
    };
    get(rt, &path)
}

#[tauri::command]
pub fn stats_top(
    by: String,
    limit: Option<u32>,
    since: Option<String>,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    let mut params: Vec<(&str, String)> = vec![("by", by)];
    if let Some(value) = limit {
        params.push(("limit", value.to_string()));
    }
    if let Some(value) = since {
        params.push(("since", value));
    }
    let path = format!(
        "/api/stats/top?{}",
        params
            .iter()
            .map(|(key, value)| format!("{key}={}", url_encode(value)))
            .collect::<Vec<_>>()
            .join("&")
    );
    get(rt, &path)
}

#[tauri::command]
pub fn status_get(rt: tauri::State<'_, Arc<Runtime>>) -> Result<Value, IpcError> {
    get(rt, "/api/status")
}

#[tauri::command]
pub fn upstream_test(
    request: Value,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    client(rt.inner())?
        .post_json_body("/api/upstreams/test", &request)
        .map_err(map_client_error)
}

/// 建立事件流：新连接会先收到环形缓冲快照，再由中继转发实时批次。
///
/// sidecar 尚未就绪时只记录订阅意图，READY 后由监护线程按同一意图建立，
/// 因此该命令在 sidecar 退避重启期间也可安全调用（不返回 sidecar_unavailable）。
#[tauri::command]
pub fn events_subscribe(
    app: AppHandle,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    crate::relay_control::subscribe(&app, rt.inner())?;
    Ok(json!({ "ok": true }))
}

#[tauri::command]
pub fn events_unsubscribe(rt: tauri::State<'_, Arc<Runtime>>) -> Result<Value, IpcError> {
    crate::relay_control::unsubscribe(rt.inner());
    Ok(json!({ "ok": true }))
}

/// 返回最近一次 sidecar 状态；用于渲染器重载后恢复 failed/exited 展示。
#[tauri::command]
pub fn sidecar_state(rt: tauri::State<'_, Arc<Runtime>>) -> Result<Option<Value>, IpcError> {
    Ok(rt.inner().sidecar_state_payload())
}

/// 清除失败态并重新拉起 sidecar。
///
/// 启动权由一次 CAS 抢占：与自动重启或另一次手动重启并发时只有一个调用方真正拉起
/// 监护线程，避免出现两个 supervisor 与两个 sidecar。
#[tauri::command]
pub fn sidecar_restart(
    app: AppHandle,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Value, IpcError> {
    let rt = rt.inner().clone();
    if rt.sidecar_pid.load(std::sync::atomic::Ordering::SeqCst) != 0 {
        return Ok(json!({ "ok": true }));
    }
    // 先抢启动权再动状态：抢占失败说明已有监护线程在启动或退避重启中，
    // 此时清空失败态与重启窗口会替对方重置限次预算，必须原样保留。
    if !rt.claim_supervisor() {
        rt.log("解析服务已在启动或运行中，忽略手动重启");
        return Ok(json!({ "ok": true }));
    }
    rt.reset_failure();
    rt.log("手动重启 sidecar：清除失败态与重启窗口");
    // 先上报 restarting，UI 在 READY 之前就能看到"正在重启"而不是停在上一个失败态。
    crate::sidecar::emit_state(&app, &rt, "restarting", Some("手动重启 sidecar"), None);
    std::thread::spawn(move || crate::sidecar::supervise(app, rt));
    Ok(json!({ "ok": true }))
}

fn url_encode(value: &str) -> String {
    let mut encoded = String::with_capacity(value.len());
    for byte in value.bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                encoded.push(byte as char);
            }
            _ => {
                use std::fmt::Write as _;
                let _ = write!(encoded, "%{byte:02X}");
            }
        }
    }
    encoded
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encodes_query_values() {
        assert_eq!(url_encode("a b/c"), "a%20b%2Fc");
        assert_eq!(url_encode("example.com."), "example.com.");
    }

    #[test]
    fn maps_http_error_body() {
        let error = map_client_error(ClientError::Http {
            status: 409,
            body: r#"{"error":"revision_conflict","current_revision":8}"#.to_string(),
        });
        assert_eq!(error.code, "revision_conflict");
        assert_eq!(error.current_revision, Some(8));
    }
}
