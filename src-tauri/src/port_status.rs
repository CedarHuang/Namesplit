//! 53 端口占用检测与归因。
//!
//! 只读：用短生命周期的绑定测试判断四个回环目标是否可用，再用 IP Helper 的
//! UDP/TCP 表尽力归因占用进程；不修改系统 DNS 或网卡设置。
//!
//! Windows 对系统保留端口（Hyper-V/WSL/ICS 等）会拒绝绑定并返回权限错误。
//! 这类端口没有可枚举的占用进程，因此状态记为 `unknown` 并标记归因受限，
//! 由上层提示用户可能是保留端口或权限限制。

use std::io;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, TcpListener, UdpSocket};
use std::sync::atomic::Ordering;

use serde::Serialize;
use windows_sys::Win32::Foundation::{CloseHandle, ERROR_INSUFFICIENT_BUFFER, NO_ERROR};
use windows_sys::Win32::NetworkManagement::IpHelper::{
    GetExtendedTcpTable, GetExtendedUdpTable, MIB_TCP6ROW_OWNER_PID, MIB_TCPROW_OWNER_PID,
    MIB_TCP_STATE_LISTEN, MIB_UDP6ROW_OWNER_PID, MIB_UDPROW_OWNER_PID, TCP_TABLE_OWNER_PID_ALL,
    UDP_TABLE_OWNER_PID,
};
use windows_sys::Win32::Networking::WinSock::{AF_INET, AF_INET6};
use windows_sys::Win32::System::Threading::{
    OpenProcess, QueryFullProcessImageNameW, PROCESS_QUERY_LIMITED_INFORMATION,
};

use crate::ipc::IpcError;

/// 固定检测的端口。
const PORT: u16 = 53;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum PortState {
    /// 可以绑定。
    Free,
    /// 已被占用，holders 为尽力归因的结果。
    InUse,
    /// 本机不存在该目标地址（典型：IPv6 未启用）；不适用，不是冲突。
    Unavailable,
    /// 绑定失败但无法归类（典型：保留端口或权限限制），也无法归因占用者。
    Unknown,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct Holder {
    pub pid: u32,
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub path: Option<String>,
    /// 占用者是否为当前应用正在监护的 sidecar；只有非自身占用才构成端口冲突。
    pub is_self: bool,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct TargetStatus {
    /// 形如 `127.0.0.1:53` 或 `[::1]:53`。
    pub address: String,
    pub protocol: &'static str,
    pub status: PortState,
    pub holders: Vec<Holder>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct PortStatus {
    pub checked_at: String,
    pub port: u16,
    pub targets: Vec<TargetStatus>,
    /// 有目标的占用进程未能完整归因（权限受限、保留端口或系统表查询失败）。
    pub attribution_limited: bool,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Protocol {
    Udp,
    Tcp,
}

impl Protocol {
    fn as_str(self) -> &'static str {
        match self {
            Self::Udp => "udp",
            Self::Tcp => "tcp",
        }
    }
}

#[derive(Debug, Clone, Copy)]
struct Endpoint {
    protocol: Protocol,
    address: IpAddr,
    pid: u32,
}

/// Tauri 命令：返回 53 端口占用与归因结果。
/// 检测包含系统表枚举与绑定探测，放到 blocking 线程池执行，避免占用主线程。
#[tauri::command]
pub async fn port_status(
    rt: tauri::State<'_, std::sync::Arc<crate::runtime::Runtime>>,
) -> Result<PortStatus, IpcError> {
    let rt = rt.inner().clone();
    // pid 是判断"自身占用"的唯一可靠依据：进程名可以重名，且上一实例残留的
    // sidecar 与本实例进程名相同但 pid 不同，必须按外部占用处理。
    let self_pid = match rt.sidecar_pid.load(Ordering::SeqCst) {
        0 => None,
        pid => Some(pid),
    };
    let status = tauri::async_runtime::spawn_blocking(move || status_for_port(PORT, self_pid))
        .await
        .map_err(|error| IpcError::new("internal", format!("端口检测任务异常：{error}")))?;
    let busy = status
        .targets
        .iter()
        .filter(|target| matches!(target.status, PortState::InUse | PortState::Unknown))
        .count();
    let unavailable = status
        .targets
        .iter()
        .filter(|target| target.status == PortState::Unavailable)
        .count();
    rt.log(&format!(
        "端口检测：{busy} 个回环目标被占用或无法绑定，{unavailable} 个目标地址在本机不存在，归因受限={}",
        status.attribution_limited
    ));
    Ok(status)
}

fn status_for_port(port: u16, self_pid: Option<u32>) -> PortStatus {
    let mut endpoints = Vec::new();
    let mut table_limited = false;

    // 四张表分别查询；任一张失败只影响归因完整度，不改变绑定测试结果。
    unsafe {
        table_limited |= !collect_tcp4(port, &mut endpoints);
        table_limited |= !collect_tcp6(port, &mut endpoints);
        table_limited |= !collect_udp4(port, &mut endpoints);
        table_limited |= !collect_udp6(port, &mut endpoints);
    }

    let mut attribution_limited = table_limited;
    let mut targets = Vec::with_capacity(4);
    for (target, protocol, host) in [
        (IpAddr::V4(Ipv4Addr::LOCALHOST), Protocol::Udp, "127.0.0.1"),
        (IpAddr::V4(Ipv4Addr::LOCALHOST), Protocol::Tcp, "127.0.0.1"),
        (IpAddr::V6(Ipv6Addr::LOCALHOST), Protocol::Udp, "::1"),
        (IpAddr::V6(Ipv6Addr::LOCALHOST), Protocol::Tcp, "::1"),
    ] {
        // 系统表已能证明被占用时不再做绑定测试：避免与正在启动的 sidecar
        // 争抢端口，也避免把自身 sidecar 的监听误判为异常。
        let (status, limited) = if has_endpoint(&endpoints, target, protocol) {
            (PortState::InUse, false)
        } else {
            probe(host, protocol, port)
        };
        attribution_limited |= limited;

        let holders = match status {
            PortState::Free | PortState::Unavailable => Vec::new(),
            PortState::InUse | PortState::Unknown => collect_holders(
                &endpoints,
                target,
                protocol,
                self_pid,
                &mut attribution_limited,
            ),
        };

        targets.push(TargetStatus {
            address: format_address(host, port),
            protocol: protocol.as_str(),
            status,
            holders,
        });
    }

    PortStatus {
        checked_at: crate::timefmt::now_rfc3339(),
        port,
        targets,
        attribution_limited,
    }
}

/// 用一次绑定尝试判断目标是否可用；socket 在函数返回前关闭。
fn probe(host: &str, protocol: Protocol, port: u16) -> (PortState, bool) {
    let address = format_address(host, port);
    let result = match protocol {
        Protocol::Udp => UdpSocket::bind(address).map(|_| ()),
        Protocol::Tcp => TcpListener::bind(address).map(|_| ()),
    };

    match result {
        Ok(()) => (PortState::Free, false),
        Err(error) => classify_bind_error(&error),
    }
}

fn classify_bind_error(error: &io::Error) -> (PortState, bool) {
    match error.kind() {
        io::ErrorKind::AddrInUse => (PortState::InUse, false),
        // 地址/地址族在本机不存在（典型：IPv6 未启用），目标不适用，不是冲突。
        io::ErrorKind::AddrNotAvailable | io::ErrorKind::Unsupported => {
            (PortState::Unavailable, false)
        }
        // 其余失败通常来自系统保留端口或权限限制；真正原因不可枚举，只能标记归因受限。
        _ => (PortState::Unknown, true),
    }
}

fn format_address(host: &str, port: u16) -> String {
    if host.contains(':') {
        format!("[{host}]:{port}")
    } else {
        format!("{host}:{port}")
    }
}

fn collect_holders(
    endpoints: &[Endpoint],
    target: IpAddr,
    protocol: Protocol,
    self_pid: Option<u32>,
    attribution_limited: &mut bool,
) -> Vec<Holder> {
    let mut pids = Vec::new();
    for endpoint in endpoints {
        if endpoint.protocol == protocol
            && endpoint_matches(endpoint.address, target)
            && !pids.contains(&endpoint.pid)
        {
            pids.push(endpoint.pid);
        }
    }
    pids.sort_unstable();

    pids.into_iter()
        .map(|pid| {
            let is_self = self_pid == Some(pid);
            match process_info(pid) {
                Some((name, path)) => Holder {
                    pid,
                    name,
                    path,
                    is_self,
                },
                None => {
                    *attribution_limited = true;
                    Holder {
                        pid,
                        name: String::new(),
                        path: None,
                        is_self,
                    }
                }
            }
        })
        .collect()
}

/// 判断系统表中是否已有端点占用给定回环目标。
fn has_endpoint(endpoints: &[Endpoint], target: IpAddr, protocol: Protocol) -> bool {
    endpoints
        .iter()
        .any(|endpoint| endpoint.protocol == protocol && endpoint_matches(endpoint.address, target))
}

/// 判断系统表里的本地地址是否可能占用给定回环目标：
/// 同地址族且本地地址为回环或通配地址。
fn endpoint_matches(endpoint: IpAddr, target: IpAddr) -> bool {
    match (endpoint, target) {
        (IpAddr::V4(address), IpAddr::V4(_)) => address.is_unspecified() || address.is_loopback(),
        (IpAddr::V6(address), IpAddr::V6(_)) => address.is_unspecified() || address.is_loopback(),
        _ => false,
    }
}

fn process_info(pid: u32) -> Option<(String, Option<String>)> {
    // 系统进程由内核持有，无法 OpenProcess；按固定 PID 给出可读名称。
    match pid {
        0 => return Some(("System Idle Process".into(), None)),
        4 => return Some(("System".into(), None)),
        _ => {}
    }

    unsafe {
        let handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
        if handle.is_null() {
            return None;
        }

        let mut buffer = vec![0u16; 32_768];
        let mut size = buffer.len() as u32;
        let ok = QueryFullProcessImageNameW(handle, 0, buffer.as_mut_ptr(), &mut size);
        let _ = CloseHandle(handle);

        if ok == 0 {
            return None;
        }

        let path = String::from_utf16_lossy(&buffer[..size as usize]);
        let name = path.rsplit('\\').next().unwrap_or(&path).to_string();
        if name.is_empty() {
            None
        } else {
            Some((name, Some(path)))
        }
    }
}

/// 端口在 MIB 表中以网络字节序存放在低 16 位。
fn port_from_network(value: u32) -> u16 {
    u16::from_be((value & 0xffff) as u16)
}

unsafe fn collect_tcp4(port: u16, endpoints: &mut Vec<Endpoint>) -> bool {
    let Some(buffer) = query_table(|table, size| unsafe {
        GetExtendedTcpTable(table, size, 0, AF_INET as u32, TCP_TABLE_OWNER_PID_ALL, 0)
    }) else {
        return false;
    };

    let count = buffer[0] as usize;
    let capacity = row_capacity::<MIB_TCPROW_OWNER_PID>(&buffer);
    let rows = (buffer.as_ptr() as *const u8).add(4) as *const MIB_TCPROW_OWNER_PID;

    for index in 0..count.min(capacity) {
        let row = unsafe { &*rows.add(index) };
        if row.dwState != MIB_TCP_STATE_LISTEN as u32 || port_from_network(row.dwLocalPort) != port
        {
            continue;
        }
        endpoints.push(Endpoint {
            protocol: Protocol::Tcp,
            address: IpAddr::V4(Ipv4Addr::from(row.dwLocalAddr.to_ne_bytes())),
            pid: row.dwOwningPid,
        });
    }

    true
}

unsafe fn collect_tcp6(port: u16, endpoints: &mut Vec<Endpoint>) -> bool {
    let Some(buffer) = query_table(|table, size| unsafe {
        GetExtendedTcpTable(table, size, 0, AF_INET6 as u32, TCP_TABLE_OWNER_PID_ALL, 0)
    }) else {
        return false;
    };

    let count = buffer[0] as usize;
    let capacity = row_capacity::<MIB_TCP6ROW_OWNER_PID>(&buffer);
    let rows = (buffer.as_ptr() as *const u8).add(4) as *const MIB_TCP6ROW_OWNER_PID;

    for index in 0..count.min(capacity) {
        let row = unsafe { &*rows.add(index) };
        if row.dwState != MIB_TCP_STATE_LISTEN as u32 || port_from_network(row.dwLocalPort) != port
        {
            continue;
        }
        endpoints.push(Endpoint {
            protocol: Protocol::Tcp,
            address: IpAddr::V6(Ipv6Addr::from(row.ucLocalAddr)),
            pid: row.dwOwningPid,
        });
    }

    true
}

unsafe fn collect_udp4(port: u16, endpoints: &mut Vec<Endpoint>) -> bool {
    let Some(buffer) = query_table(|table, size| unsafe {
        GetExtendedUdpTable(table, size, 0, AF_INET as u32, UDP_TABLE_OWNER_PID, 0)
    }) else {
        return false;
    };

    let count = buffer[0] as usize;
    let capacity = row_capacity::<MIB_UDPROW_OWNER_PID>(&buffer);
    let rows = (buffer.as_ptr() as *const u8).add(4) as *const MIB_UDPROW_OWNER_PID;

    for index in 0..count.min(capacity) {
        let row = unsafe { &*rows.add(index) };
        if port_from_network(row.dwLocalPort) != port {
            continue;
        }
        endpoints.push(Endpoint {
            protocol: Protocol::Udp,
            address: IpAddr::V4(Ipv4Addr::from(row.dwLocalAddr.to_ne_bytes())),
            pid: row.dwOwningPid,
        });
    }

    true
}

unsafe fn collect_udp6(port: u16, endpoints: &mut Vec<Endpoint>) -> bool {
    let Some(buffer) = query_table(|table, size| unsafe {
        GetExtendedUdpTable(table, size, 0, AF_INET6 as u32, UDP_TABLE_OWNER_PID, 0)
    }) else {
        return false;
    };

    let count = buffer[0] as usize;
    let capacity = row_capacity::<MIB_UDP6ROW_OWNER_PID>(&buffer);
    let rows = (buffer.as_ptr() as *const u8).add(4) as *const MIB_UDP6ROW_OWNER_PID;

    for index in 0..count.min(capacity) {
        let row = unsafe { &*rows.add(index) };
        if port_from_network(row.dwLocalPort) != port {
            continue;
        }
        endpoints.push(Endpoint {
            protocol: Protocol::Udp,
            address: IpAddr::V6(Ipv6Addr::from(row.ucLocalAddr)),
            pid: row.dwOwningPid,
        });
    }

    true
}

/// 调用两次 IP Helper 表 API：先取所需字节数，再用对齐的缓冲区取表内容。
unsafe fn query_table(
    mut fill: impl FnMut(*mut core::ffi::c_void, *mut u32) -> u32,
) -> Option<Vec<u32>> {
    let mut size = 0u32;
    let first = fill(std::ptr::null_mut(), &mut size);
    if first != NO_ERROR && first != ERROR_INSUFFICIENT_BUFFER {
        return None;
    }
    if size == 0 {
        return None;
    }

    // Vec<u32> 保证 4 字节对齐，避免把 IP Helper 返回的缓冲区按未对齐指针读取。
    let mut buffer = vec![0u32; (size as usize).div_ceil(4)];
    let status = fill(buffer.as_mut_ptr().cast(), &mut size);
    if status != NO_ERROR {
        return None;
    }

    Some(buffer)
}

fn row_capacity<T>(buffer: &[u32]) -> usize {
    (buffer.len() * 4).saturating_sub(4) / std::mem::size_of::<T>()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn network_port_byte_order() {
        // 53 在网络字节序中为 00 35，按小端 u32 读出是 0x0000_3500。
        assert_eq!(port_from_network(0x0000_3500), 53);
        // 8080 为 1F 90。
        assert_eq!(port_from_network(0x0000_901f), 8080);
    }

    #[test]
    fn process_info_reads_current_process() {
        let (name, path) = process_info(std::process::id()).expect("应能读取当前进程");
        assert!(!name.is_empty());
        assert!(path.is_some());
    }

    #[test]
    fn detects_udp_listener_with_owner() {
        // 借一个临时端口模拟外部占用；检测函数只依赖传入的端口。
        let socket = UdpSocket::bind("127.0.0.1:0").expect("绑定临时 UDP 端口");
        let port = socket.local_addr().expect("读取临时端口").port();
        let status = status_for_port(port, None);

        let target = status
            .targets
            .iter()
            .find(|target| {
                target.protocol == "udp" && target.address == format!("127.0.0.1:{port}")
            })
            .expect("应包含 IPv4 UDP 目标");

        assert_eq!(target.status, PortState::InUse);
        let holder = target
            .holders
            .iter()
            .find(|holder| holder.pid == std::process::id())
            .expect("应归因到当前测试进程");
        assert!(!holder.is_self, "未传入 sidecar pid 时不得标记为自身");
    }

    #[test]
    fn self_pid_marks_holders_of_current_sidecar() {
        let pid = std::process::id();
        let endpoints = [Endpoint {
            protocol: Protocol::Udp,
            address: IpAddr::V4(Ipv4Addr::LOCALHOST),
            pid,
        }];
        let mut attribution_limited = false;

        let holders = collect_holders(
            &endpoints,
            IpAddr::V4(Ipv4Addr::LOCALHOST),
            Protocol::Udp,
            Some(pid),
            &mut attribution_limited,
        );
        assert_eq!(holders.len(), 1);
        assert!(holders[0].is_self);
        assert!(!attribution_limited);

        // 传入其他 pid（例如上一实例残留的 sidecar）时必须视为外部占用。
        let holders = collect_holders(
            &endpoints,
            IpAddr::V4(Ipv4Addr::LOCALHOST),
            Protocol::Udp,
            Some(pid.wrapping_add(1)),
            &mut attribution_limited,
        );
        assert!(!holders[0].is_self);
    }

    #[test]
    fn bind_permission_error_is_unknown_and_limited() {
        let error = io::Error::from(io::ErrorKind::PermissionDenied);
        let (state, limited) = classify_bind_error(&error);
        assert_eq!(state, PortState::Unknown);
        assert!(limited);
    }

    #[test]
    fn bind_addr_not_available_is_unavailable() {
        for kind in [io::ErrorKind::AddrNotAvailable, io::ErrorKind::Unsupported] {
            let error = io::Error::from(kind);
            let (state, limited) = classify_bind_error(&error);
            assert_eq!(state, PortState::Unavailable, "{kind:?}");
            assert!(!limited, "{kind:?}");
        }
    }

    #[test]
    fn bind_addr_in_use_is_in_use() {
        let error = io::Error::from(io::ErrorKind::AddrInUse);
        let (state, limited) = classify_bind_error(&error);
        assert_eq!(state, PortState::InUse);
        assert!(!limited);
    }

    #[test]
    fn wildcard_endpoint_matches_loopback_target() {
        assert!(endpoint_matches(
            IpAddr::V4(Ipv4Addr::UNSPECIFIED),
            IpAddr::V4(Ipv4Addr::LOCALHOST)
        ));
        assert!(endpoint_matches(
            IpAddr::V6(Ipv6Addr::UNSPECIFIED),
            IpAddr::V6(Ipv6Addr::LOCALHOST)
        ));
        assert!(!endpoint_matches(
            IpAddr::V4(Ipv4Addr::new(192, 168, 1, 10)),
            IpAddr::V4(Ipv4Addr::LOCALHOST)
        ));
        assert!(!endpoint_matches(
            IpAddr::V6(Ipv6Addr::LOCALHOST),
            IpAddr::V4(Ipv4Addr::LOCALHOST)
        ));
    }
}
