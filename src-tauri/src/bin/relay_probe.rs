//! Tauri 应用代理路径的中继探针。
//!
//! 拉起 devstub、复用 Tauri 应用侧的中继模块，测量固定速率下的接收速率、端到端延迟、
//! 主动断流后的重连时间与慢消费者丢弃行为，并输出 JSON 结果。

use std::io::{BufRead, BufReader};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use serde_json::{json, Value};

use namesplit_lib::relay::{self, BatchSink, RelayConfig, RelayStats, DEFAULT_QUEUE_BATCHES};
use namesplit_lib::runtime::generate_token;
use namesplit_lib::sidecar_client::{ClientError, Event, SidecarClient};

const READY_PREFIX: &str = "NAMESPLIT_READY ";

fn main() {
    if let Err(err) = run() {
        eprintln!("relay_probe 失败: {err}");
        std::process::exit(1);
    }
}

fn run() -> Result<(), String> {
    let args = Args::parse()?;
    let token = generate_token();
    let mut guard = ChildGuard::default();

    let (child, port) = spawn_devstub(&args.devstub, &token, args.rate)?;
    guard.child = Some(child);
    eprintln!("devstub 就绪：127.0.0.1:{port}");

    let client = SidecarClient::new(port, token.clone());
    wait_health(&client)?;
    eprintln!("health 校验通过");

    // 用错误 token 调一次 health，确认鉴权并非仅靠客户端自觉。
    let wrong_token_status = match SidecarClient::new(port, "0".repeat(64)).health() {
        Err(ClientError::Http { status, .. }) => status,
        Err(_) => 0,
        Ok(_) => 200,
    };
    eprintln!("错误 token 请求 health 状态码：{wrong_token_status}");

    let throughput = run_throughput(port, &token, args.duration);
    eprintln!(
        "吞吐场景：{} 事件 / {} ms，稳态 {:.1} 事件/秒，p95 {:.2} ms",
        throughput.events, throughput.elapsed_ms, throughput.events_per_sec, throughput.p95_ms
    );

    let reconnect = run_reconnect(port, &token);
    eprintln!(
        "重连场景：关闭 {} 条流，重连耗时 {:?} ms，重连后事件 {}",
        reconnect.closed_streams, reconnect.reconnect_ms, reconnect.events_after_trigger
    );

    let slow = run_slow_consumer(port, &token, args.slow_duration);
    eprintln!(
        "慢消费者场景：接收 {} 事件，丢弃 {} 事件 / {} 批次",
        slow.events, slow.dropped_events, slow.dropped_batches
    );

    let evidence = json!({
        "probe": "relay-probe",
        "generated_at_unix_ms": now_ms(),
        "devstub": args.devstub.display().to_string(),
        "target_rate_per_sec": args.rate,
        "path": "devstub → Tauri 应用 sidecar_client → relay 有界队列 → sink",
        "auth": {
            "wrong_token_status": wrong_token_status,
            "expected": 401,
        },
        "throughput": {
            "duration_ms": throughput.elapsed_ms,
            "steady_window_ms": throughput.steady_window_ms,
            "events": throughput.events,
            "batches": throughput.batches,
            "events_per_sec": round1(throughput.events_per_sec),
            "events_per_sec_wall": round1(throughput.events_per_sec_wall),
            "batches_per_sec": round1(throughput.batches_per_sec),
            "avg_batch_size": round1(throughput.avg_batch_size),
            "connects": throughput.connects,
            "disconnects": throughput.disconnects,
        },
        "latency_ms": {
            "definition": "devstub 生成事件 time → relay sink 收到批次（含服务端 50ms 批量与 relay 转发）",
            "samples": throughput.latency_samples,
            "p50": round2(throughput.p50_ms),
            "p95": round2(throughput.p95_ms),
            "max": round2(throughput.max_ms),
        },
        "reconnect": {
            "trigger": "GET /api/dev/disconnect（devstub 主动关闭当前事件流）",
            "triggered_at_unix_ms": reconnect.triggered_at_ms,
            "closed_streams": reconnect.closed_streams,
            "reconnected_after_ms": reconnect.reconnect_ms,
            "events_after_trigger": reconnect.events_after_trigger,
            "connects": reconnect.connects,
            "disconnects": reconnect.disconnects,
        },
        "slow_consumer": {
            "receiver_sleep_ms": 400,
            "queue_capacity_batches": DEFAULT_QUEUE_BATCHES,
            "duration_ms": slow.elapsed_ms,
            "events_received_from_stream": slow.events,
            "batches_received_from_stream": slow.batches,
            "events_delivered_to_sink": slow.delivered_events,
            "dropped_events": slow.dropped_events,
            "dropped_batches": slow.dropped_batches,
            "reader_events_per_sec": round1(slow.reader_events_per_sec),
            "reader_blocked": slow.events == 0,
            "note": "读取线程持续读取；队列满时丢弃批次并计数，内存受队列容量约束",
        },
        "thresholds": {
            "rate_target": 1000,
            "rate_met": throughput.events_per_sec >= 1000.0,
            "reconnect_target_ms": 3000,
            "reconnect_met": reconnect.reconnect_ms.map(|ms| ms <= 3000).unwrap_or(false),
            "latency_p95_budget_ms": 200,
            "latency_met": throughput.p95_ms <= 200.0,
        },
        "limitations": [
            "devstub 是假 sidecar：不解析 DNS、不写 SQLite，事件为固定内容",
            "事件 time 在 devstub 生成批次时写入，延迟含服务端 50ms 批量等待",
            "慢消费者场景只验证 Tauri 应用侧中继，不覆盖 Tauri WebView 渲染开销",
            "真实内核 + 持久化下的事件速率需另行复测"
        ]
    });

    let text = serde_json::to_string_pretty(&evidence).map_err(|err| err.to_string())?;
    if let Some(parent) = args.out.parent() {
        std::fs::create_dir_all(parent).map_err(|err| format!("创建输出目录失败: {err}"))?;
    }
    std::fs::write(&args.out, &text).map_err(|err| format!("写输出文件失败: {err}"))?;
    println!("{text}");
    eprintln!("结果已写入 {}", args.out.display());

    // 优雅退出 devstub；失败则强制结束。devstub 的 shutdown 只接受 POST。
    let _ = client.post_json("/api/shutdown");
    std::thread::sleep(Duration::from_millis(300));
    if let Some(mut child) = guard.child.take() {
        if let Ok(None) = child.try_wait() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
    Ok(())
}

struct Args {
    devstub: PathBuf,
    rate: f64,
    duration: Duration,
    slow_duration: Duration,
    out: PathBuf,
}

impl Args {
    fn parse() -> Result<Self, String> {
        let mut devstub = PathBuf::from("build/devstub.exe");
        let mut rate = 1000.0_f64;
        let mut duration = Duration::from_secs(8);
        let mut slow_duration = Duration::from_secs(3);
        let mut out = None;

        let mut iter = std::env::args().skip(1);
        while let Some(arg) = iter.next() {
            let mut value = || iter.next().ok_or_else(|| format!("参数 {arg} 缺少数值"));
            match arg.as_str() {
                "--devstub" => devstub = PathBuf::from(value()?),
                "--rate" => rate = value()?.parse().map_err(|_| "--rate 非法")?,
                "--duration-sec" => {
                    duration =
                        Duration::from_secs(value()?.parse().map_err(|_| "--duration-sec 非法")?)
                }
                "--slow-sec" => {
                    slow_duration =
                        Duration::from_secs(value()?.parse().map_err(|_| "--slow-sec 非法")?)
                }
                "--out" => out = Some(PathBuf::from(value()?)),
                other => return Err(format!("未知参数: {other}")),
            }
        }

        let out =
            out.unwrap_or_else(|| PathBuf::from(format!("build/evidence/relay-{}.json", now_ms())));
        Ok(Self {
            devstub,
            rate,
            duration,
            slow_duration,
            out,
        })
    }
}

fn spawn_devstub(path: &Path, token: &str, rate: f64) -> Result<(Child, u16), String> {
    let mut command = Command::new(path);
    command
        .env("NAMESPLIT_TOKEN", token)
        .env("DEVSERVER_EVENTS_PER_SEC", rate.to_string())
        .env("DEVSERVER_CLOSE_STREAM_AFTER_MS", "0")
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        command.creation_flags(CREATE_NO_WINDOW);
    }

    let mut child = command
        .spawn()
        .map_err(|err| format!("启动 devstub 失败: {err}"))?;

    if let Some(stderr) = child.stderr.take() {
        std::thread::spawn(move || {
            for line in BufReader::new(stderr).lines().map_while(Result::ok) {
                eprintln!("[devstub stderr] {line}");
            }
        });
    }

    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| "无法读取 devstub stdout".to_string())?;
    let (tx, rx) = std::sync::mpsc::channel::<String>();
    std::thread::spawn(move || {
        for line in BufReader::new(stdout).lines().map_while(Result::ok) {
            match line.strip_prefix(READY_PREFIX) {
                Some(rest) => {
                    let _ = tx.send(rest.to_string());
                }
                None => eprintln!("[devstub] {line}"),
            }
        }
    });

    let ready = rx
        .recv_timeout(Duration::from_secs(5))
        .map_err(|_| "等待 devstub READY 超时".to_string())?;
    let value: Value =
        serde_json::from_str(&ready).map_err(|err| format!("READY 行不是 JSON: {err}"))?;
    let port = value
        .get("port")
        .and_then(Value::as_u64)
        .ok_or_else(|| "READY 缺少 port".to_string())? as u16;
    Ok((child, port))
}

fn wait_health(client: &SidecarClient) -> Result<(), String> {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        match client.health() {
            Ok(value) => {
                if value.get("ready").and_then(Value::as_bool) == Some(true) {
                    return Ok(());
                }
            }
            Err(err) => {
                if Instant::now() >= deadline {
                    return Err(format!("health 不可用: {err}"));
                }
            }
        }
        if Instant::now() >= deadline {
            return Err("health 就绪超时".into());
        }
        std::thread::sleep(Duration::from_millis(50));
    }
}

struct ThroughputResult {
    elapsed_ms: u64,
    steady_window_ms: u64,
    events: u64,
    batches: u64,
    events_per_sec: f64,
    events_per_sec_wall: f64,
    batches_per_sec: f64,
    avg_batch_size: f64,
    connects: u64,
    disconnects: u64,
    p50_ms: f64,
    p95_ms: f64,
    max_ms: f64,
    latency_samples: usize,
}

fn run_throughput(port: u16, token: &str, duration: Duration) -> ThroughputResult {
    let stats = Arc::new(RelayStats::default());
    let sink = Arc::new(LatencySink::default());
    let stop = Arc::new(AtomicBool::new(false));
    let handle = relay::spawn(
        RelayConfig::new(port, token.to_string()),
        sink.clone(),
        stop.clone(),
        stats.clone(),
    );

    let started = Instant::now();
    std::thread::sleep(duration);
    stop.store(true, Ordering::SeqCst);
    handle.join();
    let elapsed_ms = started.elapsed().as_millis() as u64;

    let snapshot = stats.snapshot();
    let steady_window_ms = snapshot
        .last_event_ms
        .saturating_sub(snapshot.first_event_ms)
        .max(1);

    let mut samples = sink.take_samples();
    let latency_samples = samples.len();
    samples.sort_by(|a, b| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal));
    let p50 = percentile(&samples, 0.50);
    let p95 = percentile(&samples, 0.95);
    let max = samples.last().copied().unwrap_or(0.0);

    ThroughputResult {
        elapsed_ms,
        steady_window_ms,
        events: snapshot.events,
        batches: snapshot.batches,
        events_per_sec: snapshot.events as f64 * 1000.0 / steady_window_ms as f64,
        events_per_sec_wall: snapshot.events as f64 * 1000.0 / elapsed_ms.max(1) as f64,
        batches_per_sec: snapshot.batches as f64 * 1000.0 / elapsed_ms.max(1) as f64,
        avg_batch_size: snapshot.events as f64 / snapshot.batches.max(1) as f64,
        connects: snapshot.connects,
        disconnects: snapshot.disconnects,
        p50_ms: p50,
        p95_ms: p95,
        max_ms: max,
        latency_samples,
    }
}

struct ReconnectResult {
    triggered_at_ms: u64,
    closed_streams: u64,
    reconnect_ms: Option<u64>,
    events_after_trigger: u64,
    connects: u64,
    disconnects: u64,
}

#[derive(Default)]
struct NullSink;

impl BatchSink for NullSink {
    fn emit(&self, _batch: Vec<Event>) {}
}

fn run_reconnect(port: u16, token: &str) -> ReconnectResult {
    let stats = Arc::new(RelayStats::default());
    let stop = Arc::new(AtomicBool::new(false));
    let handle = relay::spawn(
        RelayConfig::new(port, token.to_string()),
        Arc::new(NullSink),
        stop.clone(),
        stats.clone(),
    );

    // 先稳定接收 2s，再主动断流。
    std::thread::sleep(Duration::from_secs(2));
    let events_before_trigger = stats.events.load(Ordering::SeqCst);
    let triggered_at_ms = now_ms();
    let client = SidecarClient::new(port, token.to_string());
    let closed_streams = client
        .get_json("/api/dev/disconnect")
        .ok()
        .and_then(|value| value.get("closed").and_then(Value::as_u64))
        .unwrap_or(0);

    let deadline = Instant::now() + Duration::from_secs(5);
    let mut reconnect_ms = None;
    while Instant::now() < deadline {
        let snapshot = stats.snapshot();
        if snapshot.connects >= 2 && snapshot.last_connect_ms > triggered_at_ms {
            reconnect_ms = Some(snapshot.last_connect_ms - triggered_at_ms);
            break;
        }
        std::thread::sleep(Duration::from_millis(20));
    }

    // 重连后继续接收 1.5s，确认事件流恢复。
    std::thread::sleep(Duration::from_millis(1500));
    stop.store(true, Ordering::SeqCst);
    handle.join();

    let snapshot = stats.snapshot();
    ReconnectResult {
        triggered_at_ms,
        closed_streams,
        reconnect_ms,
        events_after_trigger: snapshot.events.saturating_sub(events_before_trigger),
        connects: snapshot.connects,
        disconnects: snapshot.disconnects,
    }
}

struct SlowResult {
    elapsed_ms: u64,
    events: u64,
    batches: u64,
    delivered_events: u64,
    dropped_events: u64,
    dropped_batches: u64,
    reader_events_per_sec: f64,
}

fn run_slow_consumer(port: u16, token: &str, duration: Duration) -> SlowResult {
    let stats = Arc::new(RelayStats::default());
    let sink = Arc::new(SlowSink::default());
    let stop = Arc::new(AtomicBool::new(false));
    let handle = relay::spawn(
        RelayConfig::new(port, token.to_string()),
        sink.clone(),
        stop.clone(),
        stats.clone(),
    );

    let started = Instant::now();
    std::thread::sleep(duration);
    stop.store(true, Ordering::SeqCst);
    handle.join();

    let snapshot = stats.snapshot();
    let elapsed_ms = started.elapsed().as_millis() as u64;
    SlowResult {
        elapsed_ms,
        events: snapshot.events,
        batches: snapshot.batches,
        delivered_events: sink.delivered.load(Ordering::SeqCst),
        dropped_events: snapshot.dropped_events,
        dropped_batches: snapshot.dropped_batches,
        reader_events_per_sec: snapshot.events as f64 * 1000.0 / elapsed_ms.max(1) as f64,
    }
}

#[derive(Default)]
struct LatencySink {
    samples: Mutex<Vec<f64>>,
}

impl LatencySink {
    fn take_samples(&self) -> Vec<f64> {
        self.samples
            .lock()
            .map(|mut guard| std::mem::take(&mut *guard))
            .unwrap_or_default()
    }
}

impl BatchSink for LatencySink {
    fn emit(&self, batch: Vec<Event>) {
        let received = now_ns();
        if let Ok(mut samples) = self.samples.lock() {
            samples.reserve(batch.len());
            for event in &batch {
                if let Some(sent) = parse_rfc3339_ns(&event.time) {
                    let delta_ms = (received - sent) as f64 / 1_000_000.0;
                    if delta_ms >= 0.0 {
                        samples.push(delta_ms);
                    }
                }
            }
        }
    }
}

#[derive(Default)]
struct SlowSink {
    delivered: AtomicU64,
}

impl BatchSink for SlowSink {
    fn emit(&self, batch: Vec<Event>) {
        // 人为阻塞消费者，验证读取线程不随之阻塞。
        std::thread::sleep(Duration::from_millis(400));
        self.delivered
            .fetch_add(batch.len() as u64, Ordering::SeqCst);
    }
}

fn percentile(sorted: &[f64], q: f64) -> f64 {
    if sorted.is_empty() {
        return 0.0;
    }
    let index = ((sorted.len() - 1) as f64 * q).round() as usize;
    sorted[index]
}

/// 解析 devstub 的 RFC3339Nano UTC 时间（如 2026-10-18T20:16:32.123456789Z）。
fn parse_rfc3339_ns(text: &str) -> Option<i128> {
    let text = text.trim().trim_end_matches('Z');
    let (date, time) = text.split_once('T')?;
    let mut date_parts = date.split('-');
    let year: i64 = date_parts.next()?.parse().ok()?;
    let month: i64 = date_parts.next()?.parse().ok()?;
    let day: i64 = date_parts.next()?.parse().ok()?;

    let (clock, fraction) = match time.split_once('.') {
        Some((clock, fraction)) => (clock, fraction),
        None => (time, ""),
    };
    let mut clock_parts = clock.split(':');
    let hour: i64 = clock_parts.next()?.parse().ok()?;
    let minute: i64 = clock_parts.next()?.parse().ok()?;
    let second: i64 = clock_parts.next()?.parse().ok()?;

    // 公历日期 → Unix 天数（Howard Hinnant 的 civil_from_days 反算）。
    let adjusted_year = year - if month <= 2 { 1 } else { 0 };
    let era = if adjusted_year >= 0 {
        adjusted_year
    } else {
        adjusted_year - 399
    } / 400;
    let year_of_era = adjusted_year - era * 400;
    let month_index = (month + 9) % 12;
    let day_of_year = (153 * month_index + 2) / 5 + day - 1;
    let day_of_era = year_of_era * 365 + year_of_era / 4 - year_of_era / 100 + day_of_year;
    let days = era * 146_097 + day_of_era - 719_468;

    let mut padded = fraction.to_string();
    while padded.len() < 9 {
        padded.push('0');
    }
    let nanos: i128 = padded.get(..9)?.parse().ok()?;

    let seconds = days * 86_400 + hour * 3_600 + minute * 60 + second;
    Some(seconds as i128 * 1_000_000_000 + nanos)
}

fn now_ns() -> i128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_nanos() as i128)
        .unwrap_or(0)
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|duration| duration.as_millis() as u64)
        .unwrap_or(0)
}

fn round1(value: f64) -> f64 {
    (value * 10.0).round() / 10.0
}

fn round2(value: f64) -> f64 {
    (value * 100.0).round() / 100.0
}

#[derive(Default)]
struct ChildGuard {
    child: Option<Child>,
}

impl Drop for ChildGuard {
    fn drop(&mut self) {
        if let Some(mut child) = self.child.take() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}
