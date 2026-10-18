//! 事件流中继：读取 sidecar NDJSON 批次，经有界队列转发给消费者。
//!
//! 关键约束：
//! - 读取线程不因消费者变慢而阻塞：队列满时丢弃批次并计数；
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

/// 启动中继：一个读取线程 + 一个有界队列的转发线程。
pub fn spawn(
    cfg: RelayConfig,
    sink: Arc<dyn BatchSink>,
    stop: Arc<AtomicBool>,
    stats: Arc<RelayStats>,
) -> RelayHandle {
    let (tx, rx) = mpsc::sync_channel::<Vec<Event>>(cfg.queue_batches.max(1));

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
                                }
                            }
                            Ok(None) => break,
                            Err(err) => {
                                stats.set_error(err.to_string());
                                break;
                            }
                        }
                    }
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
