//! 事件流的唯一所有权出口。
//!
//! 是否保持对 sidecar 的事件流连接只由两个事实派生：UI 的订阅意图与 sidecar
//! 是否已就绪。监护线程与 IPC 命令都只调用这里，避免两处各自建流、替换会话时
//! 不停止旧连接，以及窗口隐藏（已取消订阅）后 sidecar 重启又悄悄恢复推流。
//!
//! 所有入口在同一把控制锁下串行执行，保证意图与中继槽不会出现相互矛盾的中间态。

use std::sync::atomic::AtomicBool;
use std::sync::{Arc, Mutex, MutexGuard};

use tauri::AppHandle;

use crate::ipc::IpcError;
use crate::relay::{self, RelaySession, RelayStats, TauriBatchSink};
use crate::runtime::Runtime;

/// 串行化订阅意图与中继槽的变更；锁内只做状态切换与线程启动，不做 join。
static CONTROL: Mutex<()> = Mutex::new(());

fn control_lock() -> MutexGuard<'static, ()> {
    CONTROL
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

/// 记录订阅意图，并在 sidecar 就绪时建立事件流。
///
/// sidecar 未就绪时只记录意图：此时没有可用端口，起一个连接必然失败的重连循环
/// 没有意义；READY 后由 `sidecar_ready` 按同一意图建立。
pub fn subscribe(app: &AppHandle, rt: &Arc<Runtime>) -> Result<(), IpcError> {
    let _control = control_lock();
    rt.set_events_subscribed(true);
    if let Some(port) = rt.port() {
        replace_session(app, rt, port);
    }
    Ok(())
}

/// 清除订阅意图并停止事件流；sidecar 侧环形缓冲继续累积，重新订阅时补发快照。
pub fn unsubscribe(rt: &Arc<Runtime>) {
    let _control = control_lock();
    rt.set_events_subscribed(false);
    stop_in_background(rt.replace_relay(None));
}

/// sidecar 就绪后按新端口重建事件流；未订阅时不建流。
pub fn sidecar_ready(app: &AppHandle, rt: &Arc<Runtime>, port: u16) {
    let _control = control_lock();
    if rt.events_subscribed() {
        replace_session(app, rt, port);
    }
}

/// sidecar 退出后停止事件流，但保留订阅意图，供重启后自动恢复。
///
/// 返回停止前的统计，调用方据此记录最后一条连接的结果（含错误）。
pub fn sidecar_exited(rt: &Arc<Runtime>) -> Option<relay::RelaySnapshot> {
    let _control = control_lock();
    let session = rt.replace_relay(None)?;
    let snapshot = session.stats().snapshot();
    stop_in_background(Some(session));
    Some(snapshot)
}

/// 建立新会话并原子替换旧会话；旧会话在锁内被置 stop，句柄丢后台回收。
fn replace_session(app: &AppHandle, rt: &Arc<Runtime>, port: u16) {
    let stop = Arc::new(AtomicBool::new(false));
    let stats = Arc::new(RelayStats::default());
    let sink = Arc::new(TauriBatchSink::new(app.clone()));
    let handle = relay::spawn(
        relay::RelayConfig::new(port, rt.token.clone()),
        sink,
        stop.clone(),
        stats.clone(),
    );
    let session = RelaySession::new(stop, stats, handle);
    // 读线程受 10s 读超时约束，join 可能阻塞很久，因此不能放在锁内执行。
    stop_in_background(rt.replace_relay(Some(session)));
}

fn stop_in_background(session: Option<RelaySession>) {
    let Some(session) = session else {
        return;
    };
    std::thread::spawn(move || session.stop_and_join());
}
