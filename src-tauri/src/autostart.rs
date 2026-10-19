//! 用户级登录自启（HKCU Run）与启动路径有效性检查。
//!
//! 只写当前用户的 Run 键，不需要管理员权限；不创建服务、计划任务或驱动。

use std::path::Path;

use serde::Serialize;
use windows_sys::Win32::Foundation::{ERROR_FILE_NOT_FOUND, NO_ERROR};
use windows_sys::Win32::System::Registry::{
    RegCloseKey, RegDeleteValueW, RegOpenKeyExW, RegQueryValueExW, RegSetValueExW, HKEY,
    HKEY_CURRENT_USER, KEY_READ, KEY_SET_VALUE, REG_SZ,
};

use crate::ipc::IpcError;

const RUN_KEY: &str = r"Software\Microsoft\Windows\CurrentVersion\Run";
const VALUE_NAME: &str = "Namesplit";

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct AutostartStatus {
    pub enabled: bool,
    pub path: String,
    pub path_valid: bool,
}

/// Tauri 命令：读取登录自启状态。
#[tauri::command]
pub fn autostart_get(
    rt: tauri::State<'_, std::sync::Arc<crate::runtime::Runtime>>,
) -> Result<AutostartStatus, IpcError> {
    let status = read_status().map_err(autostart_error)?;
    rt.log(&format!(
        "读取登录自启：enabled={} path_valid={}",
        status.enabled, status.path_valid
    ));
    Ok(status)
}

/// Tauri 命令：开启或关闭登录自启。
#[tauri::command]
pub fn autostart_set(
    enabled: bool,
    rt: tauri::State<'_, std::sync::Arc<crate::runtime::Runtime>>,
) -> Result<AutostartStatus, IpcError> {
    set_enabled(enabled)?;
    let status = read_status().map_err(autostart_error)?;
    rt.log(&format!(
        "设置登录自启：enabled={} path={}",
        status.enabled, status.path
    ));
    Ok(status)
}

fn read_status() -> Result<AutostartStatus, u32> {
    let Some(raw) = read_run_value()? else {
        return Ok(AutostartStatus {
            enabled: false,
            path: String::new(),
            path_valid: false,
        });
    };

    let path = executable_from_run_value(&raw).to_string();
    let path_valid = !path.is_empty() && Path::new(&path).is_file();

    Ok(AutostartStatus {
        enabled: true,
        path,
        path_valid,
    })
}

fn read_run_value() -> Result<Option<String>, u32> {
    let name = to_wide_null(VALUE_NAME);
    let mut key: HKEY = std::ptr::null_mut();
    let result = unsafe {
        RegOpenKeyExW(
            HKEY_CURRENT_USER,
            to_wide_null(RUN_KEY).as_ptr(),
            0,
            KEY_READ,
            &mut key,
        )
    };
    if result == ERROR_FILE_NOT_FOUND {
        return Ok(None);
    }
    if result != NO_ERROR {
        return Err(result);
    }

    let mut value_type = 0u32;
    let mut size = 0u32;
    let mut result = unsafe {
        RegQueryValueExW(
            key,
            name.as_ptr(),
            std::ptr::null(),
            &mut value_type,
            std::ptr::null_mut(),
            &mut size,
        )
    };
    if result == ERROR_FILE_NOT_FOUND {
        unsafe { RegCloseKey(key) };
        return Ok(None);
    }
    if result != NO_ERROR {
        unsafe { RegCloseKey(key) };
        return Err(result);
    }
    if size == 0 {
        unsafe { RegCloseKey(key) };
        return Ok(Some(String::new()));
    }

    let mut buffer = vec![0u16; (size as usize).div_ceil(2)];
    result = unsafe {
        RegQueryValueExW(
            key,
            name.as_ptr(),
            std::ptr::null(),
            &mut value_type,
            buffer.as_mut_ptr().cast(),
            &mut size,
        )
    };
    unsafe { RegCloseKey(key) };
    if result != NO_ERROR {
        return Err(result);
    }

    let length = (size as usize).div_ceil(2);
    let text = String::from_utf16_lossy(&buffer[..length]);
    Ok(Some(text.trim_end_matches('\0').to_string()))
}

fn set_enabled(enabled: bool) -> Result<(), IpcError> {
    let name = to_wide_null(VALUE_NAME);
    let mut key: HKEY = std::ptr::null_mut();
    let result = unsafe {
        RegOpenKeyExW(
            HKEY_CURRENT_USER,
            to_wide_null(RUN_KEY).as_ptr(),
            0,
            KEY_SET_VALUE,
            &mut key,
        )
    };
    if result != NO_ERROR {
        return Err(autostart_error(result));
    }

    let result = if enabled {
        let executable = std::env::current_exe()
            .map_err(|error| {
                IpcError::new("autostart_failed", format!("读取应用路径失败：{error}"))
            })?
            .to_string_lossy()
            .into_owned();
        let command = format!("\"{executable}\"");
        let data = to_wide_null(&command);
        let bytes =
            unsafe { std::slice::from_raw_parts(data.as_ptr().cast::<u8>(), data.len() * 2) };
        unsafe {
            RegSetValueExW(
                key,
                name.as_ptr(),
                0,
                REG_SZ,
                bytes.as_ptr(),
                bytes.len() as u32,
            )
        }
    } else {
        let result = unsafe { RegDeleteValueW(key, name.as_ptr()) };
        if result == ERROR_FILE_NOT_FOUND {
            NO_ERROR
        } else {
            result
        }
    };

    unsafe { RegCloseKey(key) };
    if result == NO_ERROR {
        Ok(())
    } else {
        Err(autostart_error(result))
    }
}

/// Run 值可能是 `"C:\path\app.exe"` 或带参数的命令行；只取可执行文件路径。
fn executable_from_run_value(value: &str) -> &str {
    let value = value.trim();
    if let Some(rest) = value.strip_prefix('"') {
        if let Some(end) = rest.find('"') {
            return &rest[..end];
        }
    }
    value.split_whitespace().next().unwrap_or(value)
}

fn to_wide_null(value: &str) -> Vec<u16> {
    value.encode_utf16().chain(Some(0)).collect()
}

fn autostart_error(code: u32) -> IpcError {
    IpcError::new(
        "autostart_failed",
        format!("操作登录自启失败（Windows 错误码 {code}）"),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_quoted_executable_from_run_value() {
        assert_eq!(
            executable_from_run_value(r#""C:\Program Files\Namesplit\Namesplit.exe""#),
            r"C:\Program Files\Namesplit\Namesplit.exe"
        );
        assert_eq!(
            executable_from_run_value(r#""C:\Apps\Namesplit.exe" --minimized"#),
            r"C:\Apps\Namesplit.exe"
        );
        assert_eq!(
            executable_from_run_value(r"C:\Apps\Namesplit.exe --minimized"),
            r"C:\Apps\Namesplit.exe"
        );
    }

    #[test]
    fn reads_autostart_status_without_failing() {
        let status = read_status().expect("读取 HKCU Run 不应失败");
        if status.enabled {
            assert!(!status.path.is_empty());
        } else {
            assert!(status.path.is_empty());
            assert!(!status.path_valid);
        }
    }
}
