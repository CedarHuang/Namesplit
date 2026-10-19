//! 事件流中继：读取 sidecar NDJSON 批次，经有界队列转发给消费者。
//!
//! 关键约束：
//! - 读取线程不因消费者变慢而阻塞：队列满时丢弃批次并计数；
//! - 丢弃要向消费者发增量信号，否则 UI 只看到日志停止增长却不知道原因；
//! - 队列有界，慢消费者不会导致内存失控；
//! - 连接断开后按 250/500/1000/2000ms 退避重连，端口变化由监护层重建中继。

use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{mpsc, Arc, Mutex};
use std::thread::{self, JoinHandle};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use serde_json::Value;
use tauri::{AppHandle, Emitter};

use crate::sidecar_client::{Event, SidecarClient};

/// 重连退避；超过末尾后保持 2s。
pub const RECONNECT_BACKOFF_MS: [u64; 4] = [250, 500, 1000, 2000];
/// 中继队列默认容量（批次）。保持小容量以限制慢消费者场景的内存。
pub const DEFAULT_QUEUE_BATCHES: usize = 16;
/// 丢弃通知的最小间隔：慢消费者会持续丢批，逐批通知会把事件风暴放大成通知风暴。
const DROPPED_NOTIFY_INTERVAL: Duration = Duration::from_secs(1);

/// 中继统计；探针与日志共用。
#[derive(Default)]
pub struct RelayStats {
    pub connects: AtomicU64,
    pub disconnects: AtomicU64,
    pub batches: AtomicU64,
    pub events: AtomicU64,
    pub dropped_batches: AtomicU64,
    pub dropped_events: AtomicU64,
    pub first_event_ms: AtomicU64,
    pub last_connect_ms: AtomicU64,
    pub last_event_ms: AtomicU64,
    pub last_error: Mutex<Option<String>>,
}

impl RelayStats {
    pub fn snapshot(&self) -> RelaySnapshot {
        RelaySnapshot {
            connects: self.connects.load(Ordering::SeqCst),
            disconnects: self.disconnects.load(Ordering::SeqCst),
            batches: self.batches.load(Ordering::SeqCst),
            events: self.events.load(Ordering::SeqCst),
            dropped_batches: self.dropped_batches.load(Ordering::SeqCst),
            dropped_events: self.dropped_events.load(Ordering::SeqCst),
            first_event_ms: self.first_event_ms.load(Ordering::SeqCst),
            last_connect_ms: self.last_connect_ms.load(Ordering::SeqCst),
            last_event_ms: self.last_event_ms.load(Ordering::SeqCst),
            last_error: self.last_error.lock().map(|v| v.clone()).unwrap_or(None),
        }
    }

    fn set_error(&self, msg: String) {
        if let Ok(mut slot) = self.last_error.lock() {
            *slot = Some(msg);
        }
    }
}

#[derive(Debug, Clone)]
pub struct RelaySnapshot {
    pub connects: u64,
    pub disconnects: u64,
    pub batches: u64,
    pub events: u64,
    pub dropped_batches: u64,
    pub dropped_events: u64,
    pub first_event_ms: u64,
    pub last_connect_ms: u64,
    pub last_event_ms: u64,
    pub last_error: Option<String>,
}

/// 批次消费者；实现方不得让读取线程等待自己（否则慢消费者策略失效）。
pub trait BatchSink: Send + Sync {
    fn emit(&self, batch: Vec<Event>);

    /// 队列满导致丢弃批次时的增量通知；默认忽略，只做批次转发的实现无需关心。
    fn notify_dropped(&self, _dropped_batches: u64, _dropped_events: u64) {}
}

/// 生产实现：把批次转发为 Tauri 事件 `query://events`。
pub struct TauriBatchSink {
    app: AppHandle,
}

impl TauriBatchSink {
    pub fn new(app: AppHandle) -> Self {
        Self { app }
    }
}

impl BatchSink for TauriBatchSink {
    fn emit(&self, batch: Vec<Event>) {
        if let Ok(payload) = serde_json::to_value(&batch) {
            let _ = self.app.emit("query://events", payload);
        }
    }

    /// 增量上报未被转发的事件数；UI 据此重新订阅，用环形缓冲快照补齐丢失区间。
    fn notify_dropped(&self, dropped_batches: u64, dropped_events: u64) {
        let _ = self.app.emit(
            "query://dropped",
            serde_json::json!({
                "dropped_batches": dropped_batches,
                "dropped_events": dropped_events,
            }),
        );
    }
}

#[derive(Debug, Clone)]
pub struct RelayConfig {
    pub port: u16,
    pub token: String,
    pub queue_batches: usize,
}

impl RelayConfig {
    pub fn new(port: u16, token: impl Into<String>) -> Self {
        Self {
            port,
            token: token.into(),
            queue_batches: DEFAULT_QUEUE_BATCHES,
        }
    }
}

/// 中继句柄；调用方停止时先置 `stop`，再 join。
pub struct RelayHandle {
    reader: JoinHandle<()>,
    emitter: JoinHandle<()>,
}

impl RelayHandle {
    pub fn join(self) {
        let _ = self.reader.join();
        let _ = self.emitter.join();
    }
}

/// 一次事件流中继会话：停止标志、统计与线程句柄。
///
/// 会话由中继槽拥有，槽内任意时刻最多一个会话；替换时旧会话先置 `stop` 再让位。
/// 停止只是置标志，读线程受读超时约束自行退出，因此不能在持锁时 join。
pub struct RelaySession {
    stop: Arc<AtomicBool>,
    stats: Arc<RelayStats>,
    handle: Option<RelayHandle>,
}

impl RelaySession {
    pub fn new(stop: Arc<AtomicBool>, stats: Arc<RelayStats>, handle: RelayHandle) -> Self {
        Self {
            stop,
            stats,
            handle: Some(handle),
        }
    }

    pub fn stats(&self) -> &RelayStats {
        &self.stats
    }

    /// 仅置停止标志，不等待线程退出；可重复调用。
    pub fn stop(&self) {
        self.stop.store(true, Ordering::SeqCst);
    }

    /// 停止并等待中继线程退出；读超时最多 10s，调用方应放到后台线程执行。
    pub fn stop_and_join(mut self) {
        self.stop();
        if let Some(handle) = self.handle.take() {
            handle.join();
        }
    }
}

impl Drop for RelaySession {
    /// 兜底：会话被丢弃（例如替换后未显式回收）时也不能让线程继续推送。
    fn drop(&mut self) {
        self.stop();
    }
}

/// 启动中继：一个读取线程 + 一个有界队列的转发线程。
pub fn spawn(
    cfg: RelayConfig,
    sink: Arc<dyn BatchSink>,
    stop: Arc<AtomicBool>,
    stats: Arc<RelayStats>,
) -> RelayHandle {
    let (tx, rx) = mpsc::sync_channel::<Vec<Event>>(cfg.queue_batches.max(1));

    let reader_sink = sink.clone();
    let emitter_stop = stop.clone();
    let emitter = thread::spawn(move || loop {
        if emitter_stop.load(Ordering::SeqCst) {
            return;
        }
        match rx.recv_timeout(Duration::from_millis(100)) {
            Ok(batch) => sink.emit(batch),
            Err(mpsc::RecvTimeoutError::Timeout) => continue,
            Err(mpsc::RecvTimeoutError::Disconnected) => return,
        }
    });

    let reader = thread::spawn(move || {
        let mut backoff_index = 0usize;
        let mut pending_batches = 0u64;
        let mut pending_events = 0u64;
        let mut last_notify: Option<Instant> = None;
        while !stop.load(Ordering::SeqCst) {
            let client = SidecarClient::new(cfg.port, cfg.token.clone());
            match client.open_events() {
                Ok(mut stream) => {
                    stats.connects.fetch_add(1, Ordering::SeqCst);
                    stats.last_connect_ms.store(now_ms(), Ordering::SeqCst);
                    backoff_index = 0;
                    loop {
                        if stop.load(Ordering::SeqCst) {
                            break;
                        }
                        match stream.next_batch() {
                            Ok(Some(batch)) => {
                                let count = batch.len() as u64;
                                let now = now_ms();
                                stats.batches.fetch_add(1, Ordering::SeqCst);
                                stats.events.fetch_add(count, Ordering::SeqCst);
                                let _ = stats.first_event_ms.compare_exchange(
                                    0,
                                    now,
                                    Ordering::SeqCst,
                                    Ordering::SeqCst,
                                );
                                stats.last_event_ms.store(now, Ordering::SeqCst);
                                if tx.try_send(batch).is_err() {
                                    stats.dropped_batches.fetch_add(1, Ordering::SeqCst);
                                    stats.dropped_events.fetch_add(count, Ordering::SeqCst);
                                    pending_batches += 1;
                                    pending_events += count;
                                    let notify_now = Instant::now();
                                    if should_notify_dropped(last_notify, notify_now) {
                                        reader_sink.notify_dropped(pending_batches, pending_events);
                                        pending_batches = 0;
                                        pending_events = 0;
                                        last_notify = Some(notify_now);
                                    }
                                }
                            }
                            Ok(None) => break,
                            Err(err) => {
                                stats.set_error(err.to_string());
                                break;
                            }
                        }
                    }
                    // 连接结束前把最后的丢弃量补发出去，避免节流窗口内的丢批没有信号。
                    flush_dropped(
                        reader_sink.as_ref(),
                        &mut pending_batches,
                        &mut pending_events,
                    );
                    stats.disconnects.fetch_add(1, Ordering::SeqCst);
                }
                Err(err) => stats.set_error(err.to_string()),
            }

            if stop.load(Ordering::SeqCst) {
                break;
            }
            let wait_ms = RECONNECT_BACKOFF_MS[backoff_index.min(RECONNECT_BACKOFF_MS.len() - 1)];
            backoff_index = (backoff_index + 1).min(RECONNECT_BACKOFF_MS.len() - 1);
            sleep_until(stop.clone(), Duration::from_millis(wait_ms));
        }
        // 读取线程退出前再补发一次，保证停止时也不丢信号。
        flush_dropped(
            reader_sink.as_ref(),
            &mut pending_batches,
            &mut pending_events,
        );
        // 读取线程退出后丢弃发送端，转发线程随之结束。
    });

    RelayHandle { reader, emitter }
}

fn sleep_until(stop: Arc<AtomicBool>, total: Duration) {
    let deadline = Instant::now() + total;
    while Instant::now() < deadline {
        if stop.load(Ordering::SeqCst) {
            return;
        }
        thread::sleep(Duration::from_millis(20));
    }
}

/// 丢弃通知节流：从未通知过，或距上次通知已满一个间隔。
fn should_notify_dropped(last: Option<Instant>, now: Instant) -> bool {
    match last {
        None => true,
        Some(last) => now.saturating_duration_since(last) >= DROPPED_NOTIFY_INTERVAL,
    }
}

/// 立即上报累计的丢弃量（增量），并清零本地计数。
fn flush_dropped(sink: &dyn BatchSink, pending_batches: &mut u64, pending_events: &mut u64) {
    if *pending_batches == 0 && *pending_events == 0 {
        return;
    }
    sink.notify_dropped(*pending_batches, *pending_events);
    *pending_batches = 0;
    *pending_events = 0;
}

pub fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_millis() as u64)
        .unwrap_or(0)
}

/// 供探针复用的标准化批次值，避免探针自行决定序列化格式。
pub fn batch_to_value(batch: &[Event]) -> Value {
    serde_json::to_value(batch).unwrap_or(Value::Null)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dropped_notification_is_throttled_to_one_per_interval() {
        let start = Instant::now();
        assert!(should_notify_dropped(None, start), "首次丢弃应立即通知");
        assert!(!should_notify_dropped(
            Some(start),
            start + Duration::from_millis(999)
        ));
        assert!(should_notify_dropped(
            Some(start),
            start + DROPPED_NOTIFY_INTERVAL
        ));
    }

    #[test]
    fn flush_dropped_reports_accumulated_delta_once() {
        struct RecordingSink {
            calls: Mutex<Vec<(u64, u64)>>,
        }

        impl BatchSink for RecordingSink {
            fn emit(&self, _batch: Vec<Event>) {}

            fn notify_dropped(&self, dropped_batches: u64, dropped_events: u64) {
                self.calls
                    .lock()
                    .expect("通知记录锁")
                    .push((dropped_batches, dropped_events));
            }
        }

        let sink = RecordingSink {
            calls: Mutex::new(Vec::new()),
        };
        let mut pending_batches = 3u64;
        let mut pending_events = 150u64;

        flush_dropped(&sink, &mut pending_batches, &mut pending_events);
        assert_eq!(*sink.calls.lock().expect("通知记录锁"), vec![(3, 150)]);
        assert_eq!((pending_batches, pending_events), (0, 0));

        flush_dropped(&sink, &mut pending_batches, &mut pending_events);
        assert_eq!(
            sink.calls.lock().expect("通知记录锁").len(),
            1,
            "没有待上报量时不应再发通知"
        );
    }
}
