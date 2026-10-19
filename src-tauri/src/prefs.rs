//! UI 偏好：语言与主题。
//!
//! 偏好由 Tauri 应用保存（sidecar 只管 DNS 配置），固定落在数据目录的
//! `ui-prefs.json`；窗口状态由 window-state 插件独立保存，不混入本文件。
//! 偏好文件是可直接阅读/手改的普通 JSON，因此读取必须容忍缺失、损坏与非法取值：
//! 任何一项异常都不能让设置页失去读写能力。

use std::path::PathBuf;
use std::sync::Arc;

use serde::{Deserialize, Serialize};

use crate::ipc::IpcError;
use crate::runtime::Runtime;

const LANGUAGES: [&str; 2] = ["zh-CN", "en-US"];
const THEMES: [&str; 3] = ["system", "light", "dark"];

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Prefs {
    pub language: String,
    pub theme: String,
}

impl Default for Prefs {
    fn default() -> Self {
        Self {
            language: "zh-CN".to_string(),
            theme: "system".to_string(),
        }
    }
}

fn prefs_path(rt: &Runtime) -> PathBuf {
    rt.data_dir.join("ui-prefs.json")
}

/// 读取偏好，永不失败：文件缺失/不可读/JSON 非法时整体回退默认值；
/// 单个字段取值不在白名单时只回退该字段，避免一个坏值连累另一项。
fn read_prefs(rt: &Runtime) -> Prefs {
    let path = prefs_path(rt);
    let raw = match std::fs::read_to_string(&path) {
        Ok(raw) => raw,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Prefs::default(),
        Err(error) => {
            rt.log(&format!("读取 UI 偏好失败，使用默认值：{error}"));
            return Prefs::default();
        }
    };
    let mut prefs: Prefs = match serde_json::from_str(&raw) {
        Ok(prefs) => prefs,
        Err(error) => {
            rt.log(&format!("UI 偏好文件非法，使用默认值：{error}"));
            return Prefs::default();
        }
    };
    let defaults = Prefs::default();
    if !LANGUAGES.contains(&prefs.language.as_str()) {
        rt.log(&format!(
            "UI 偏好语言取值非法，回退默认：{}",
            prefs.language
        ));
        prefs.language = defaults.language;
    }
    if !THEMES.contains(&prefs.theme.as_str()) {
        rt.log(&format!("UI 偏好主题取值非法，回退默认：{}", prefs.theme));
        prefs.theme = defaults.theme;
    }
    prefs
}

fn write_prefs(rt: &Runtime, prefs: &Prefs) -> Result<(), IpcError> {
    let path = prefs_path(rt);
    let data = serde_json::to_vec_pretty(prefs)
        .map_err(|error| IpcError::new("internal", format!("序列化偏好失败：{error}")))?;
    let tmp = path.with_extension("json.tmp");
    std::fs::write(&tmp, data)
        .and_then(|_| std::fs::rename(&tmp, &path))
        .map_err(|error| IpcError::new("internal", format!("写入偏好失败：{error}")))
}

/// 读取 UI 偏好；文件损坏或字段非法时按字段回退，不 reject。
#[tauri::command]
pub fn prefs_get(rt: tauri::State<'_, Arc<Runtime>>) -> Prefs {
    read_prefs(rt.inner())
}

/// 更新 UI 偏好；未提供的字段保持不变。
#[tauri::command]
pub fn prefs_set(
    language: Option<String>,
    theme: Option<String>,
    rt: tauri::State<'_, Arc<Runtime>>,
) -> Result<Prefs, IpcError> {
    let mut prefs = read_prefs(rt.inner());
    if let Some(language) = language {
        if !LANGUAGES.contains(&language.as_str()) {
            return Err(IpcError::new(
                "invalid_prefs_value",
                format!("不支持的语言：{language}"),
            ));
        }
        prefs.language = language;
    }
    if let Some(theme) = theme {
        if !THEMES.contains(&theme.as_str()) {
            return Err(IpcError::new(
                "invalid_prefs_value",
                format!("不支持的主题：{theme}"),
            ));
        }
        prefs.theme = theme;
    }
    write_prefs(rt.inner(), &prefs)?;
    rt.log(&format!(
        "UI 偏好已保存：language={} theme={}",
        prefs.language, prefs.theme
    ));
    Ok(prefs)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn write_raw(rt: &Runtime, raw: &str) {
        std::fs::write(prefs_path(rt), raw).expect("写入测试偏好文件");
    }

    #[test]
    fn missing_file_falls_back_to_defaults() {
        let rt = Runtime::for_test();
        let prefs = read_prefs(&rt);
        assert_eq!(prefs.language, "zh-CN");
        assert_eq!(prefs.theme, "system");
    }

    #[test]
    fn corrupt_file_falls_back_to_defaults() {
        let rt = Runtime::for_test();
        write_raw(&rt, "{ 这不是 JSON");
        let prefs = read_prefs(&rt);
        assert_eq!(prefs.language, "zh-CN");
        assert_eq!(prefs.theme, "system");
    }

    #[test]
    fn illegal_value_falls_back_per_field() {
        let rt = Runtime::for_test();
        write_raw(&rt, r#"{"language":"fr","theme":"dark"}"#);
        let prefs = read_prefs(&rt);
        assert_eq!(prefs.language, "zh-CN", "非法语言应回退默认");
        assert_eq!(prefs.theme, "dark", "合法主题应保留");

        write_raw(&rt, r#"{"language":"en-US","theme":"neon"}"#);
        let prefs = read_prefs(&rt);
        assert_eq!(prefs.language, "en-US", "合法语言应保留");
        assert_eq!(prefs.theme, "system", "非法主题应回退默认");
    }

    #[test]
    fn valid_file_round_trips() {
        let rt = Runtime::for_test();
        let prefs = Prefs {
            language: "en-US".to_string(),
            theme: "dark".to_string(),
        };
        write_prefs(&rt, &prefs).expect("写入合法偏好");
        let read = read_prefs(&rt);
        assert_eq!(read.language, "en-US");
        assert_eq!(read.theme, "dark");
    }
}
