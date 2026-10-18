//! Tauri 应用代理路径的中继探针。
//!
//! 拉起 devstub 或真实 namesplit-core，复用 Tauri 应用侧的中继模块，测量固定速率下的
//! 接收速率、端到端延迟、主动断流后的重连时间与慢消费者丢弃行为，并输出 JSON 结果。
//!
//! 真实内核模式由 `--core` + `--loadgen` 触发：loadgen 向 127.0.0.1:53 持续发查询，
//! core 产生真实事件；该模式不重测重连（缺少可主动断流的开发端点）。
//!
//! 证据 JSON 总是先写盘；任一阈值、鉴权或子进程约束不满足时，main 以非零退出码结束，
//! 避免脚本只看到证据文件就当作通过。

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
    let real_core = args.core.is_some();

    let (child, port) = if let Some(core) = &args.core {
        spawn_core(core, &token, args.local_app_data.as_deref())?
    } else {
        spawn_devstub(&args.devstub, &token, args.rate)?
    };
    guard.child = Some(child);
    eprintln!(
        "{} 就绪：127.0.0.1:{port}",
        if real_core { "core" } else { "devstub" }
    );

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

    // core 模式需要 loadgen 在整轮测量期间持续供压；其正常时长是
    // duration + slow + 2s，留出的 2s 让慢消费者场景也能读到事件。
    let loadgen_expected = args.duration + args.slow_duration + Duration::from_secs(2);
    let mut loadgen_guard = ChildGuard::default();
    let mut loadgen_started = None;
    let loadgen_stdout = Arc::new(Mutex::new(String::new()));
    let mut loadgen_reader = None;
    if real_core {
        let loadgen = args
            .loadgen
            .as_ref()
            .ok_or_else(|| "真实内核模式需要 --loadgen".to_string())?;
        let started = Instant::now();
        let mut child = spawn_loadgen(loadgen, args.rate, loadgen_expected)?;
        forward_stderr(&mut child, "loadgen");
        // loadgen 的 stdout 是最终统计 JSON；单独收流，退出后再解析。
        if let Some(stdout) = child.stdout.take() {
            let buffer = loadgen_stdout.clone();
            loadgen_reader = Some(std::thread::spawn(move || {
                for line in BufReader::new(stdout).lines().map_while(Result::ok) {
                    if let Ok(mut text) = buffer.lock() {
                        text.push_str(&line);
                        text.push('\n');
                    }
                }
            }));
        }
        loadgen_guard.child = Some(child);
        loadgen_started = Some(started);
        std::thread::sleep(Duration::from_millis(300));
    }

    let throughput = run_throughput(port, &token, args.duration);
    eprintln!(
        "吞吐场景：{} 事件 / {} ms，稳态 {:.1} 事件/秒，p95 {:.2} ms",
        throughput.events, throughput.elapsed_ms, throughput.events_per_sec, throughput.p95_ms
    );

    let reconnect = if real_core {
        None
    } else {
        Some(run_reconnect(port, &token))
    };
    if let Some(reconnect) = &reconnect {
        eprintln!(
            "重连场景：关闭 {} 条流，重连耗时 {:?} ms，重连后事件 {}",
            reconnect.closed_streams, reconnect.reconnect_ms, reconnect.events_after_trigger
        );
    }

    let slow = run_slow_consumer(port, &token, args.slow_duration);
    eprintln!(
        "慢消费者场景：接收 {} 事件，丢弃 {} 事件 / {} 批次",
        slow.events, slow.dropped_events, slow.dropped_batches
    );

    // loadgen 正常在 duration + slow + 2s 附近退出；这里等它收尾并取回统计，
    // 早退、超时、非零退出或存在失败请求都说明 core 侧证据不可信。
    let mut loadgen_run: Option<LoadgenRun> = None;
    if real_core {
        if let Some(mut child) = loadgen_guard.child.take() {
            let started = loadgen_started.unwrap_or_else(Instant::now);
            let exit = wait_loadgen(&mut child, started, loadgen_expected);
            if let Some(reader) = loadgen_reader.take() {
                let _ = reader.join();
            }
            let text = loadgen_stdout
                .lock()
                .map(|guard| guard.clone())
                .unwrap_or_default();
            let result = serde_json::from_str::<Value>(text.trim()).ok();
            loadgen_run = Some(LoadgenRun { exit, result });
        }
    }

    let reconnect_evidence = match &reconnect {
        Some(reconnect) => json!({
            "trigger": "GET /api/dev/disconnect（devstub 主动关闭当前事件流）",
            "triggered_at_unix_ms": reconnect.triggered_at_ms,
            "closed_streams": reconnect.closed_streams,
            "reconnected_after_ms": reconnect.reconnect_ms,
            "events_after_trigger": reconnect.events_after_trigger,
            "connects": reconnect.connects,
            "disconnects": reconnect.disconnects,
        }),
        None => Value::Null,
    };

    let reconnect_met = reconnect
        .as_ref()
        .and_then(|item| item.reconnect_ms)
        .map(|ms| ms <= 3000);
    let rate_met = throughput.events_per_sec >= 1000.0;
    let latency_met = throughput.p95_ms <= 200.0;
    let reader_blocked = slow.events == 0;
    // core 的 /api/health 顶层 ready 在 DNS 内核启动前就为 true，必须读 dns.ready
    // 才能证明 53 端口真的在监听；测量结束后再读，避开启动时序竞态。
    let core_dns_ready = if real_core {
        match client.health() {
            Ok(value) => value.pointer("/dns/ready").and_then(Value::as_bool),
            Err(err) => {
                eprintln!("读取 core health 失败: {err}");
                None
            }
        }
    } else {
        None
    };

    let mut failures: Vec<String> = Vec::new();
    if wrong_token_status != 401 {
        failures.push(format!("错误 token 应返回 401，实际 {wrong_token_status}"));
    }
    if !rate_met {
        failures.push(format!(
            "稳态速率未达标：{:.1} 事件/秒 < 1000",
            throughput.events_per_sec
        ));
    }
    if !latency_met {
        failures.push(format!(
            "p95 延迟超出预算：{:.2} ms > 200",
            throughput.p95_ms
        ));
    }
    if reconnect_met == Some(false) {
        failures.push("重连耗时超过 3000ms".to_string());
    }
    if reader_blocked {
        failures.push("慢消费者读取线程被阻塞（未读到事件）".to_string());
    }
    if real_core && core_dns_ready != Some(true) {
        failures.push(format!(
            "core /api/health 的 dns.ready 不是 true：{core_dns_ready:?}"
        ));
    }

    let loadgen_evidence = match &loadgen_run {
        Some(run) => json!({
            "exit_code": run.exit.exit_code,
            "timed_out": run.exit.timed_out,
            "early_exit": run.exit.early_exit,
            "elapsed_ms": run.exit.elapsed_ms,
            "result": run.result.clone().unwrap_or(Value::Null),
        }),
        None => Value::Null,
    };
    if real_core {
        match &loadgen_run {
            Some(run) => {
                if run.exit.timed_out {
                    failures.push("loadgen 未在预期时间内退出".to_string());
                }
                if run.exit.early_exit {
                    failures.push(format!(
                        "loadgen 异常早退：{} ms，预期约 {} ms",
                        run.exit.elapsed_ms,
                        loadgen_expected.as_millis()
                    ));
                }
                if run.exit.exit_code != Some(0) {
                    failures.push(format!("loadgen 退出码异常：{:?}", run.exit.exit_code));
                }
                match run.result.as_ref() {
                    Some(result) => {
                        let failed = result.get("failed").and_then(Value::as_u64).unwrap_or(0);
                        let sent = result.get("sent").and_then(Value::as_u64).unwrap_or(0);
                        if failed != 0 {
                            failures.push(format!("loadgen 失败请求 {failed} 条"));
                        }
                        if sent == 0 {
                            failures.push("loadgen 未发送任何查询".to_string());
                        }
                    }
                    None => failures.push("loadgen 未输出可解析的统计 JSON".to_string()),
                }
            }
            None => failures.push("core 模式缺少 loadgen 子进程".to_string()),
        }
    }

    let evidence = json!({
        "probe": "relay-probe",
        "generated_at_unix_ms": now_ms(),
        "mode": if real_core { "core" } else { "devstub" },
        "core": args.core.as_ref().map(|path| path.display().to_string()),
        "loadgen": args.loadgen.as_ref().map(|path| path.display().to_string()),
        "data_dir": args.local_app_data.as_ref().map(|path| path.display().to_string()),
        "devstub": if real_core { Value::Null } else { json!(args.devstub.display().to_string()) },
        "target_rate_per_sec": args.rate,
        "path": if real_core {
            "namesplit-core → Tauri 应用 sidecar_client → relay 有界队列 → sink"
        } else {
            "devstub → Tauri 应用 sidecar_client → relay 有界队列 → sink"
        },
        "auth": {
            "wrong_token_status": wrong_token_status,
            "expected": 401,
        },
        "core_health": if real_core {
            json!({ "dns_ready": core_dns_ready })
        } else {
            Value::Null
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
            "definition": if real_core {
                "core 收到查询时间 → relay sink 收到批次（含服务端 50ms 批量与 relay 转发）"
            } else {
                "devstub 生成事件 time → relay sink 收到批次（含服务端 50ms 批量与 relay 转发）"
            },
            "samples": throughput.latency_samples,
            "p50": round2(throughput.p50_ms),
            "p95": round2(throughput.p95_ms),
            "max": round2(throughput.max_ms),
        },
        "reconnect": reconnect_evidence,
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
            "reader_blocked": reader_blocked,
            "note": "读取线程持续读取；队列满时丢弃批次并计数，内存受队列容量约束",
        },
        "loadgen_run": loadgen_evidence,
        "thresholds": {
            "rate_target": 1000,
            "rate_met": rate_met,
            "reconnect_target_ms": 3000,
            "reconnect_met": reconnect_met,
            "latency_p95_budget_ms": 200,
            "latency_met": latency_met,
        },
        "limitations": if real_core {
            json!([
                "真实内核模式使用本地 mock 上游，不含公网往返延迟",
                "真实内核模式未重测重连：缺少可主动断流的开发端点，重连路径沿用 devstub 证据",
                "慢消费者场景只验证 Tauri 应用侧中继，不覆盖 Tauri WebView 渲染开销"
            ])
        } else {
            json!([
                "devstub 是假 sidecar：不解析 DNS、不写 SQLite，事件为固定内容",
                "事件 time 在 devstub 生成批次时写入，延迟含服务端 50ms 批量等待",
                "慢消费者场景只验证 Tauri 应用侧中继，不覆盖 Tauri WebView 渲染开销",
                "真实内核 + 持久化下的事件速率需另行复测"
            ])
        },
        "failures": failures.clone(),
    });

    let text = serde_json::to_string_pretty(&evidence).map_err(|err| err.to_string())?;
    if let Some(parent) = args.out.parent() {
        std::fs::create_dir_all(parent).map_err(|err| format!("创建输出目录失败: {err}"))?;
    }
    std::fs::write(&args.out, &text).map_err(|err| format!("写输出文件失败: {err}"))?;
    println!("{text}");
    eprintln!("结果已写入 {}", args.out.display());

    // 优雅退出 sidecar；失败则强制结束。
    let _ = client.post_json("/api/shutdown");
    std::thread::sleep(Duration::from_millis(300));
    if let Some(mut child) = guard.child.take() {
        if let Ok(None) = child.try_wait() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }

    // 证据已落盘；阈值或子进程约束不满足时必须让调用方看到非零退出码。
    if !failures.is_empty() {
        return Err(format!("阈值未达标: {}", failures.join("；")));
    }
    Ok(())
}

struct Args {
    devstub: PathBuf,
    core: Option<PathBuf>,
    loadgen: Option<PathBuf>,
    local_app_data: Option<PathBuf>,
    rate: f64,
    duration: Duration,
    slow_duration: Duration,
    out: PathBuf,
}

impl Args {
    fn parse() -> Result<Self, String> {
        let mut devstub = PathBuf::from("build/devstub.exe");
        let mut core = None;
        let mut loadgen = None;
        let mut local_app_data = None;
        let mut rate = 1000.0_f64;
        let mut duration = Duration::from_secs(8);
        let mut slow_duration = Duration::from_secs(3);
        let mut out = None;

        let mut iter = std::env::args().skip(1);
        while let Some(arg) = iter.next() {
            let mut value = || iter.next().ok_or_else(|| format!("参数 {arg} 缺少数值"));
            match arg.as_str() {
                "--devstub" => devstub = PathBuf::from(value()?),
                "--core" => core = Some(PathBuf::from(value()?)),
                "--loadgen" => loadgen = Some(PathBuf::from(value()?)),
                "--local-app-data" => local_app_data = Some(PathBuf::from(value()?)),
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

        if core.is_some() && loadgen.is_none() {
            return Err("真实内核模式需要 --loadgen".to_string());
        }

        let out =
            out.unwrap_or_else(|| PathBuf::from(format!("build/evidence/relay-{}.json", now_ms())));
        Ok(Self {
            devstub,
            core,
            loadgen,
            local_app_data,
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
    apply_no_window(&mut command);

    let mut child = command
        .spawn()
        .map_err(|err| format!("启动 devstub 失败: {err}"))?;

    forward_stderr(&mut child, "devstub");
    let port = wait_ready(&mut child, "devstub")?;

    Ok((child, port))
}

fn spawn_core(
    path: &Path,
    token: &str,
    local_app_data: Option<&Path>,
) -> Result<(Child, u16), String> {
    let mut command = Command::new(path);
    command
        .env("NAMESPLIT_TOKEN", token)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if let Some(dir) = local_app_data {
        command.env("LOCALAPPDATA", dir);
    }
    apply_no_window(&mut command);

    let mut child = command
        .spawn()
        .map_err(|err| format!("启动 namesplit-core 失败: {err}"))?;

    forward_stderr(&mut child, "core");
    let port = wait_ready(&mut child, "core")?;

    Ok((child, port))
}

fn spawn_loadgen(path: &Path, qps: f64, duration: Duration) -> Result<Child, String> {
    let mut command = Command::new(path);
    command
        .args([
            "-addr",
            "127.0.0.1:53",
            "-qps",
            &format!("{:.0}", qps),
            "-duration",
            &format!("{}s", duration.as_secs()),
            "-concurrency",
            "64",
        ])
        .stdin(Stdio::null())
        // stdout 是最终统计 JSON，必须收流解析；stderr 单独转发日志。
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    apply_no_window(&mut command);

    command
        .spawn()
        .map_err(|err| format!("启动 loadgen 失败: {err}"))
}

struct LoadgenExit {
    exit_code: Option<i32>,
    timed_out: bool,
    early_exit: bool,
    elapsed_ms: u64,
}

struct LoadgenRun {
    exit: LoadgenExit,
    result: Option<Value>,
}

fn wait_loadgen(child: &mut Child, started: Instant, expected: Duration) -> LoadgenExit {
    // 正常退出点由 loadgen 自己的 duration 决定；额外留 3s 收尾余量，
    // 超时则强杀，避免探针永久挂起。
    let deadline = started + expected + Duration::from_secs(3);
    let mut timed_out = false;
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break Some(status),
            Ok(None) => {}
            Err(err) => {
                eprintln!("等待 loadgen 退出失败: {err}");
                break None;
            }
        }
        if Instant::now() >= deadline {
            timed_out = true;
            let _ = child.kill();
            break child.wait().ok();
        }
        std::thread::sleep(Duration::from_millis(50));
    };

    let elapsed = started.elapsed();
    LoadgenExit {
        exit_code: status.and_then(|status| status.code()),
        timed_out,
        // 远早于预期时长退出说明 loadgen 没跑完既定负载（例如 core 未监听 53）。
        early_exit: !timed_out && elapsed + Duration::from_millis(1000) < expected,
        elapsed_ms: elapsed.as_millis() as u64,
    }
}

fn apply_no_window(command: &mut Command) {
    #[cfg(windows)]
    {
        use std::os::windows::process::CommandExt;
        const CREATE_NO_WINDOW: u32 = 0x0800_0000;
        command.creation_flags(CREATE_NO_WINDOW);
    }
}

fn forward_stderr(child: &mut Child, label: &'static str) {
    if let Some(stderr) = child.stderr.take() {
        std::thread::spawn(move || {
            for line in BufReader::new(stderr).lines().map_while(Result::ok) {
                eprintln!("[{label} stderr] {line}");
            }
        });
    }
}

fn wait_ready(child: &mut Child, label: &'static str) -> Result<u16, String> {
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| format!("无法读取 {label} stdout"))?;
    let (tx, rx) = std::sync::mpsc::channel::<String>();
    std::thread::spawn(move || {
        for line in BufReader::new(stdout).lines().map_while(Result::ok) {
            match line.strip_prefix(READY_PREFIX) {
                Some(rest) => {
                    let _ = tx.send(rest.to_string());
                }
                None => eprintln!("[{label}] {line}"),
            }
        }
    });

    let ready = rx
        .recv_timeout(Duration::from_secs(5))
        .map_err(|_| format!("等待 {label} READY 超时"))?;
    let value: Value =
        serde_json::from_str(&ready).map_err(|err| format!("READY 行不是 JSON: {err}"))?;
    let port = value
        .get("port")
        .and_then(Value::as_u64)
        .ok_or_else(|| "READY 缺少 port".to_string())? as u16;

    Ok(port)
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
