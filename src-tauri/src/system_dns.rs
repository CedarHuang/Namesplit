//! 系统 DNS 只读状态、变化监听与退出提醒。
//!
//! 不修改网卡设置：通过 GetAdaptersAddresses 读取活动网卡的 DNS 服务器地址，
//! 用 IP Helper 变化通知触发防抖刷新；退出前若仍有网卡指向本机则弹出确认框。

use std::mem::size_of;
use std::sync::{mpsc, Arc};
use std::time::Duration;

use serde::Serialize;
use tauri::{AppHandle, Emitter, Manager};
use windows_sys::Win32::Foundation::{ERROR_BUFFER_OVERFLOW, HANDLE, NO_ERROR};
use windows_sys::Win32::NetworkManagement::IpHelper::{
    CancelMibChangeNotify2, GetAdaptersAddresses, NotifyIpInterfaceChange,
    NotifyUnicastIpAddressChange, GAA_FLAG_SKIP_ANYCAST, GAA_FLAG_SKIP_MULTICAST,
    GAA_FLAG_SKIP_UNICAST, IF_TYPE_SOFTWARE_LOOPBACK, IP_ADAPTER_ADDRESSES_LH, MIB_IPINTERFACE_ROW,
    MIB_NOTIFICATION_TYPE, MIB_UNICASTIPADDRESS_ROW,
};
use windows_sys::Win32::NetworkManagement::Ndis::IfOperStatusUp;
use windows_sys::Win32::Networking::WinSock::{
    AF_INET, AF_INET6, AF_UNSPEC, SOCKADDR_IN, SOCKADDR_IN6, SOCKET_ADDRESS,
};
use windows_sys::Win32::UI::WindowsAndMessaging::{
    MessageBoxW, IDYES, MB_DEFBUTTON2, MB_ICONWARNING, MB_SETFOREGROUND, MB_TOPMOST, MB_YESNO,
};

use crate::ipc::IpcError;

/// 系统 DNS 变化事件名；前端按此订阅，改名会让已构建的前端收不到事件。
const DNS_CHANGED_EVENT: &str = "system_dns://changed";
/// 换网时同一变化会连续触发多条通知，合并到安静期后再读取。
const DEBOUNCE: Duration = Duration::from_millis(300);

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct InterfaceDns {
    pub guid: String,
    pub name: String,
    pub ipv4: Vec<String>,
    pub ipv6: Vec<String>,
    pub points_to_loopback: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct SystemDnsStatus {
    pub checked_at: String,
    pub interfaces: Vec<InterfaceDns>,
}

/// 只读读取当前活动网卡的 DNS 配置。
pub fn snapshot() -> Result<SystemDnsStatus, IpcError> {
    let interfaces = unsafe { read_interfaces()? };
    Ok(SystemDnsStatus {
        checked_at: crate::timefmt::now_rfc3339(),
        interfaces,
    })
}

/// Tauri 命令：只读读取网卡 DNS，失败时 reject `system_dns_unavailable`。
/// 读取是阻塞型系统调用，放到 blocking 线程池执行，避免占用主线程。
#[tauri::command]
pub async fn system_dns_get(
    rt: tauri::State<'_, Arc<crate::runtime::Runtime>>,
) -> Result<SystemDnsStatus, IpcError> {
    let rt = rt.inner().clone();
    let status = tauri::async_runtime::spawn_blocking(snapshot)
        .await
        .map_err(|error| {
            IpcError::new(
                "system_dns_unavailable",
                format!("读取网卡 DNS 的任务异常：{error}"),
            )
        })??;
    rt.log(&format!(
        "系统 DNS 状态读取：{} 个活动网卡",
        status.interfaces.len()
    ));
    Ok(status)
}

unsafe fn read_interfaces() -> Result<Vec<InterfaceDns>, IpcError> {
    let flags = GAA_FLAG_SKIP_ANYCAST | GAA_FLAG_SKIP_MULTICAST | GAA_FLAG_SKIP_UNICAST;
    let mut size: u32 = 16 * 1024;

    // 首次按估计大小申请；地址表增长导致溢出时用 API 回填的大小重试。
    for _ in 0..3 {
        let mut buffer = vec![0u64; (size as usize).div_ceil(8)];
        let mut buffer_size = (buffer.len() * 8) as u32;
        let result = unsafe {
            GetAdaptersAddresses(
                AF_UNSPEC as u32,
                flags,
                std::ptr::null(),
                buffer.as_mut_ptr().cast::<IP_ADAPTER_ADDRESSES_LH>(),
                &mut buffer_size,
            )
        };

        if result == ERROR_BUFFER_OVERFLOW {
            size = buffer_size;
            continue;
        }
        if result != NO_ERROR {
            return Err(IpcError::new(
                "system_dns_unavailable",
                format!("读取网卡 DNS 配置失败（错误码 {result}）"),
            ));
        }

        let mut interfaces = Vec::new();
        let mut current = buffer.as_ptr().cast::<IP_ADAPTER_ADDRESSES_LH>();
        while !current.is_null() {
            let adapter = unsafe { &*current };
            if adapter.OperStatus == IfOperStatusUp && adapter.IfType != IF_TYPE_SOFTWARE_LOOPBACK {
                interfaces.push(unsafe { interface_from_adapter(adapter) });
            }
            current = adapter.Next;
        }
        interfaces.sort_by(|left, right| left.name.cmp(&right.name));

        return Ok(interfaces);
    }

    Err(IpcError::new(
        "system_dns_unavailable",
        "网卡地址表大小持续变化，无法读取 DNS 配置",
    ))
}

unsafe fn interface_from_adapter(adapter: &IP_ADAPTER_ADDRESSES_LH) -> InterfaceDns {
    let guid = unsafe { ansi_string(adapter.AdapterName) };
    let name = unsafe { wide_string(adapter.FriendlyName) };

    let mut ipv4 = Vec::new();
    let mut ipv6 = Vec::new();
    let mut dns = adapter.FirstDnsServerAddress;
    while !dns.is_null() {
        let entry = unsafe { &*dns };
        if let Some((is_ipv6, address)) = unsafe { socket_address(entry.Address) } {
            if is_ipv6 {
                ipv6.push(address);
            } else {
                ipv4.push(address);
            }
        }
        dns = entry.Next;
    }
    ipv4.sort();
    ipv4.dedup();
    ipv6.sort();
    ipv6.dedup();

    let points_to_loopback = points_to_loopback(&ipv4, &ipv6);

    InterfaceDns {
        guid,
        name,
        ipv4,
        ipv6,
        points_to_loopback,
    }
}

fn points_to_loopback(ipv4: &[String], ipv6: &[String]) -> bool {
    ipv4.iter().any(|address| address == "127.0.0.1") || ipv6.iter().any(|address| address == "::1")
}

unsafe fn socket_address(address: SOCKET_ADDRESS) -> Option<(bool, String)> {
    if address.lpSockaddr.is_null() {
        return None;
    }

    let family = unsafe { (*address.lpSockaddr).sa_family };
    match family {
        AF_INET => {
            if address.iSockaddrLength < size_of::<SOCKADDR_IN>() as i32 {
                return None;
            }
            let socket = unsafe { &*(address.lpSockaddr.cast::<SOCKADDR_IN>()) };
            let octets = socket.sin_addr.S_un.S_addr.to_ne_bytes();
            Some((false, std::net::Ipv4Addr::from(octets).to_string()))
        }
        AF_INET6 => {
            if address.iSockaddrLength < size_of::<SOCKADDR_IN6>() as i32 {
                return None;
            }
            let socket = unsafe { &*(address.lpSockaddr.cast::<SOCKADDR_IN6>()) };
            Some((
                true,
                std::net::Ipv6Addr::from(socket.sin6_addr.u.Byte).to_string(),
            ))
        }
        _ => None,
    }
}

unsafe fn ansi_string(pointer: *const u8) -> String {
    if pointer.is_null() {
        return String::new();
    }
    unsafe {
        std::ffi::CStr::from_ptr(pointer.cast::<std::ffi::c_char>())
            .to_string_lossy()
            .into_owned()
    }
}

unsafe fn wide_string(pointer: *const u16) -> String {
    if pointer.is_null() {
        return String::new();
    }
    let mut length = 0;
    while length < 512 && unsafe { *pointer.add(length) } != 0 {
        length += 1;
    }
    String::from_utf16_lossy(unsafe { std::slice::from_raw_parts(pointer, length) })
}

/// 监听网卡/IP 变化并推送 `system_dns://changed`。
pub struct DnsMonitor {
    tx: mpsc::Sender<WatchMessage>,
}

/// 系统通知、窗口聚焦、关停都汇入这一通道，由监听线程串行处理，
/// 保证快照只被一个线程读写。
enum WatchMessage {
    Refresh,
    Stop,
}

impl DnsMonitor {
    /// 建立监听；快照读取与事件推送都在监听线程内完成，不阻塞调用线程。
    pub fn start(app: AppHandle) -> Arc<Self> {
        let (tx, receiver) = mpsc::channel::<WatchMessage>();
        let callback_sender = tx.clone();
        let monitor = Arc::new(Self { tx });

        let thread_app = app.clone();
        if let Err(error) = std::thread::Builder::new()
            .name("namesplit-dns-watch".into())
            .spawn(move || watch(thread_app, receiver, callback_sender))
        {
            log(&app, &format!("启动系统 DNS 监听线程失败：{error}"));
        }

        monitor
    }

    /// 请求一次防抖刷新；窗口聚焦等事件只投递消息，不在调用线程读系统状态。
    pub fn request_refresh(&self) {
        let _ = self.tx.send(WatchMessage::Refresh);
    }
}

impl Drop for DnsMonitor {
    fn drop(&mut self) {
        let _ = self.tx.send(WatchMessage::Stop);
    }
}

fn watch(
    app: AppHandle,
    receiver: mpsc::Receiver<WatchMessage>,
    callback_sender: mpsc::Sender<WatchMessage>,
) {
    // 回调 context 指向本线程栈上的 Sender：注册成功后系统线程通过它投递刷新请求，
    // CancelMibChangeNotify2 返回后不会再回调，因此栈上生命周期是安全的。
    let context = &callback_sender as *const mpsc::Sender<WatchMessage> as *const core::ffi::c_void;
    let mut interface_handle: HANDLE = std::ptr::null_mut();
    let mut address_handle: HANDLE = std::ptr::null_mut();

    let result = unsafe {
        NotifyIpInterfaceChange(
            AF_UNSPEC,
            Some(on_interface_change),
            context,
            false,
            &mut interface_handle,
        )
    };
    if result != NO_ERROR {
        log(&app, &format!("注册网卡变化通知失败：错误码 {result}"));
    }

    let result = unsafe {
        NotifyUnicastIpAddressChange(
            AF_UNSPEC,
            Some(on_address_change),
            context,
            false,
            &mut address_handle,
        )
    };
    if result != NO_ERROR {
        log(&app, &format!("注册 IP 变化通知失败：错误码 {result}"));
    }

    let mut last = snapshot().ok();
    let count = last
        .as_ref()
        .map(|status| status.interfaces.len())
        .unwrap_or(0);
    log(&app, &format!("系统 DNS 监听已启动：{count} 个活动网卡"));

    // 线程以任何方式退出时都先取消通知注册，再释放回调 context。
    let _registrations = NotificationRegistrations {
        interface: interface_handle,
        address: address_handle,
    };

    while let Ok(WatchMessage::Refresh) = receiver.recv() {
        loop {
            match receiver.recv_timeout(DEBOUNCE) {
                Ok(WatchMessage::Refresh) => continue,
                Ok(WatchMessage::Stop) | Err(mpsc::RecvTimeoutError::Disconnected) => return,
                Err(mpsc::RecvTimeoutError::Timeout) => break,
            }
        }

        refresh_and_emit(&app, &mut last);
    }
}

/// 持有变化通知句柄，线程退出时取消注册，避免把清理逻辑散落在各条返回路径。
struct NotificationRegistrations {
    interface: HANDLE,
    address: HANDLE,
}

impl Drop for NotificationRegistrations {
    fn drop(&mut self) {
        unsafe {
            if !self.interface.is_null() {
                CancelMibChangeNotify2(self.interface);
            }
            if !self.address.is_null() {
                CancelMibChangeNotify2(self.address);
            }
        }
    }
}

fn refresh_and_emit(app: &AppHandle, last: &mut Option<SystemDnsStatus>) {
    let Ok(status) = snapshot() else {
        return;
    };
    if !interfaces_changed(last.as_ref(), &status) {
        return;
    }

    *last = Some(status.clone());
    let _ = app.emit(DNS_CHANGED_EVENT, &status);
    log(app, "系统 DNS 状态已变化，已推送 system_dns://changed");
    crate::tray::refresh_status(app);
}

fn interfaces_changed(previous: Option<&SystemDnsStatus>, current: &SystemDnsStatus) -> bool {
    previous
        .map(|previous| previous.interfaces != current.interfaces)
        .unwrap_or(true)
}

unsafe extern "system" fn on_interface_change(
    context: *const core::ffi::c_void,
    _row: *const MIB_IPINTERFACE_ROW,
    _notification: MIB_NOTIFICATION_TYPE,
) {
    if context.is_null() {
        return;
    }
    let sender = unsafe { &*(context.cast::<mpsc::Sender<WatchMessage>>()) };
    let _ = sender.send(WatchMessage::Refresh);
}

unsafe extern "system" fn on_address_change(
    context: *const core::ffi::c_void,
    _row: *const MIB_UNICASTIPADDRESS_ROW,
    _notification: MIB_NOTIFICATION_TYPE,
) {
    if context.is_null() {
        return;
    }
    let sender = unsafe { &*(context.cast::<mpsc::Sender<WatchMessage>>()) };
    let _ = sender.send(WatchMessage::Refresh);
}

/// 退出提醒需要覆盖的网卡名称。
fn loopback_interface_names(status: &SystemDnsStatus) -> Vec<&str> {
    status
        .interfaces
        .iter()
        .filter(|interface| interface.points_to_loopback)
        .map(|interface| interface.name.as_str())
        .collect()
}

/// 退出前检查：仍有网卡 DNS 指向本机时让用户确认。
/// 返回 true 表示可以退出；读取失败不阻塞退出。
pub fn confirm_exit_if_needed(app: &AppHandle) -> bool {
    let status = match snapshot() {
        Ok(status) => status,
        Err(error) => {
            log(app, &format!("退出前读取系统 DNS 失败，跳过提醒：{error}"));
            return true;
        }
    };

    let affected = loopback_interface_names(&status);
    if affected.is_empty() {
        return true;
    }

    let names = affected.join("、");
    log(app, &format!("退出提醒：以下网卡 DNS 仍指向本机：{names}"));

    let text = format!(
        "以下网卡的 DNS 仍指向本机：\r\n{names}\r\n\r\n退出 Namesplit 后，这些网卡将无法解析域名。请先把 DNS 改回自动获取或原来的服务器地址。\r\n\r\n仍要退出吗？"
    );
    let text_wide: Vec<u16> = text.encode_utf16().chain(Some(0)).collect();
    let title_wide: Vec<u16> = "Namesplit".encode_utf16().chain(Some(0)).collect();

    // 主窗口可见时作为 owner；隐藏到托盘时退化为无 owner + TOPMOST/SETFOREGROUND，
    // 避免对话框因父窗口不可见而出现在其他窗口后面，让用户误以为"点退出没反应"。
    let owner = app
        .get_webview_window("main")
        .filter(|window| window.is_visible().unwrap_or(false))
        .and_then(|window| window.hwnd().ok())
        .map(|window| window.0)
        .unwrap_or(std::ptr::null_mut());

    let choice = unsafe {
        MessageBoxW(
            owner,
            text_wide.as_ptr(),
            title_wide.as_ptr(),
            MB_YESNO | MB_ICONWARNING | MB_DEFBUTTON2 | MB_SETFOREGROUND | MB_TOPMOST,
        )
    };

    let allowed = match choice {
        IDYES => true,
        // 0 表示消息框无法显示（极度异常），退出守卫必须 fail-open，
        // 不能因为无法询问用户就把应用锁死。
        0 => {
            log(app, "退出提醒消息框显示失败，按允许退出处理");
            true
        }
        _ => false,
    };
    log(
        app,
        if allowed {
            "用户确认在 DNS 仍指向本机时退出"
        } else {
            "用户取消退出"
        },
    );
    allowed
}

fn log(app: &AppHandle, message: &str) {
    if let Some(runtime) = app.try_state::<Arc<crate::runtime::Runtime>>() {
        runtime.log(message);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn detects_loopback_dns_servers() {
        assert!(points_to_loopback(&["127.0.0.1".into()], &[]));
        assert!(points_to_loopback(&[], &["::1".into()]));
        assert!(!points_to_loopback(
            &["8.8.8.8".into()],
            &["2001:4860:4860::8888".into()]
        ));
    }

    #[test]
    fn detects_interface_list_changes() {
        let status = |name: &str| SystemDnsStatus {
            checked_at: "2026-10-19T00:00:00Z".into(),
            interfaces: vec![InterfaceDns {
                guid: "{1}".into(),
                name: name.into(),
                ipv4: vec!["192.168.1.1".into()],
                ipv6: vec![],
                points_to_loopback: false,
            }],
        };

        assert!(interfaces_changed(None, &status("WLAN")));
        assert!(!interfaces_changed(Some(&status("WLAN")), &status("WLAN")));
        assert!(interfaces_changed(
            Some(&status("WLAN")),
            &status("Ethernet")
        ));
    }

    #[test]
    fn snapshot_reads_interfaces() {
        let status = snapshot().expect("读取活动网卡 DNS");
        assert!(status.checked_at.ends_with('Z'));
        for interface in &status.interfaces {
            let expected = interface.ipv4.iter().any(|address| address == "127.0.0.1")
                || interface.ipv6.iter().any(|address| address == "::1");
            assert_eq!(
                interface.points_to_loopback, expected,
                "points_to_loopback 与地址列表不一致：{interface:?}"
            );
        }
    }

    #[test]
    fn exit_guard_lists_only_loopback_interfaces() {
        let status = SystemDnsStatus {
            checked_at: "2026-10-19T00:00:00Z".into(),
            interfaces: vec![
                InterfaceDns {
                    guid: "{1}".into(),
                    name: "WLAN".into(),
                    ipv4: vec!["127.0.0.1".into()],
                    ipv6: vec![],
                    points_to_loopback: true,
                },
                InterfaceDns {
                    guid: "{2}".into(),
                    name: "Ethernet".into(),
                    ipv4: vec!["192.168.1.1".into()],
                    ipv6: vec![],
                    points_to_loopback: false,
                },
            ],
        };

        assert_eq!(loopback_interface_names(&status), vec!["WLAN"]);
    }
}
