//! sidecar 内部 HTTP 客户端与 NDJSON 事件流解析。
//!
//! 只面向回环地址的内部 API：不做 TLS、重定向、连接池与压缩，
//! 用最小的 HTTP/1.1 实现避免为 Tauri 应用引入通用 HTTP 依赖。
//! 鉴权头与端口只存在于 Rust 侧，不向渲染器暴露。
//! 事件流中的空行是 sidecar 的保活心跳，解析时忽略。

use std::collections::HashMap;
use std::fmt;
use std::io::{self, BufRead, BufReader, Read, Write};
use std::net::TcpStream;
use std::time::Duration;

use serde::{Deserialize, Serialize};

/// JSON 请求的读超时。事件流读超时为 10s：sidecar 空闲时每 ≤5s 输出一个空行保活，
/// 连续 10s 收不到任何字节才视为断线，由中继按退避重连。
const JSON_TIMEOUT: Duration = Duration::from_secs(5);
const STREAM_TIMEOUT: Duration = Duration::from_secs(10);
const CONNECT_TIMEOUT: Duration = Duration::from_secs(2);
const WRITE_TIMEOUT: Duration = Duration::from_secs(2);
/// 单行响应头上限，避免异常服务端撑爆内存。
const HEADER_LIMIT: usize = 64 * 1024;

#[derive(Debug)]
pub enum ClientError {
    Io(io::Error),
    Http { status: u16, body: String },
    Protocol(String),
    Json(serde_json::Error),
}

impl fmt::Display for ClientError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io(err) => write!(f, "IO 错误: {err}"),
            Self::Http { status, body } => write!(f, "HTTP {status}: {body}"),
            Self::Protocol(msg) => write!(f, "协议错误: {msg}"),
            Self::Json(err) => write!(f, "JSON 错误: {err}"),
        }
    }
}

impl std::error::Error for ClientError {}

impl From<io::Error> for ClientError {
    fn from(value: io::Error) -> Self {
        Self::Io(value)
    }
}

impl From<serde_json::Error> for ClientError {
    fn from(value: serde_json::Error) -> Self {
        Self::Json(value)
    }
}

/// 回环 HTTP 客户端；一个实例对应一个 sidecar 端口与 token。
#[derive(Clone)]
pub struct SidecarClient {
    port: u16,
    token: String,
}

impl SidecarClient {
    pub fn new(port: u16, token: impl Into<String>) -> Self {
        Self {
            port,
            token: token.into(),
        }
    }

    pub fn port(&self) -> u16 {
        self.port
    }

    pub fn health(&self) -> Result<serde_json::Value, ClientError> {
        self.get_json("/api/health")
    }

    pub fn config(&self) -> Result<serde_json::Value, ClientError> {
        self.get_json("/api/config")
    }

    pub fn queries(&self) -> Result<serde_json::Value, ClientError> {
        self.get_json("/api/queries?limit=10")
    }

    pub fn stats(&self) -> Result<serde_json::Value, ClientError> {
        self.get_json("/api/stats")
    }

    pub fn get_json(&self, path: &str) -> Result<serde_json::Value, ClientError> {
        self.request_json("GET", path)
    }

    /// 无请求体的 POST；用于 `/api/shutdown` 等控制命令。
    pub fn post_json(&self, path: &str) -> Result<serde_json::Value, ClientError> {
        self.request_json("POST", path)
    }

    pub fn delete_json(&self, path: &str) -> Result<serde_json::Value, ClientError> {
        self.request_json("DELETE", path)
    }

    /// 带 JSON 请求体的 POST；用于配置校验、规则测试、上游测试等命令。
    pub fn post_json_body(
        &self,
        path: &str,
        body: &serde_json::Value,
    ) -> Result<serde_json::Value, ClientError> {
        self.request_json_body("POST", path, body)
    }

    pub fn put_json_body(
        &self,
        path: &str,
        body: &serde_json::Value,
    ) -> Result<serde_json::Value, ClientError> {
        self.request_json_body("PUT", path, body)
    }

    fn request_json_body(
        &self,
        method: &str,
        path: &str,
        body: &serde_json::Value,
    ) -> Result<serde_json::Value, ClientError> {
        let encoded = serde_json::to_string(body)?;
        let mut stream = self.connect(JSON_TIMEOUT)?;
        self.write_request(&mut stream, method, path, "application/json", &encoded)?;
        let response = read_response(stream)?;
        let text = response.body.into_string(1024 * 1024)?;
        if !(200..300).contains(&response.status) {
            return Err(ClientError::Http {
                status: response.status,
                body: text,
            });
        }
        Ok(serde_json::from_str(&text)?)
    }

    fn request_json(&self, method: &str, path: &str) -> Result<serde_json::Value, ClientError> {
        let mut stream = self.connect(JSON_TIMEOUT)?;
        self.write_request(&mut stream, method, path, "application/json", "")?;
        let response = read_response(stream)?;
        let body = response.body.into_string(1024 * 1024)?;
        if !(200..300).contains(&response.status) {
            return Err(ClientError::Http {
                status: response.status,
                body,
            });
        }
        Ok(serde_json::from_str(&body)?)
    }

    /// 建立事件流连接；返回的流按行读取 `Event[]` 批次。
    pub fn open_events(&self) -> Result<EventStream, ClientError> {
        let mut stream = self.connect(STREAM_TIMEOUT)?;
        self.write_get(&mut stream, "/api/events", true)?;
        let response = read_response(stream)?;
        if response.status != 200 {
            let body = response.body.into_string(4096)?;
            return Err(ClientError::Http {
                status: response.status,
                body,
            });
        }
        Ok(EventStream {
            reader: BufReader::new(response.body),
        })
    }

    fn connect(&self, read_timeout: Duration) -> Result<TcpStream, ClientError> {
        let addr: std::net::SocketAddr = ([127, 0, 0, 1], self.port).into();
        let stream = TcpStream::connect_timeout(&addr, CONNECT_TIMEOUT)?;
        stream.set_nodelay(true)?;
        stream.set_read_timeout(Some(read_timeout))?;
        stream.set_write_timeout(Some(WRITE_TIMEOUT))?;
        Ok(stream)
    }

    fn write_get(
        &self,
        stream: &mut TcpStream,
        path: &str,
        ndjson: bool,
    ) -> Result<(), ClientError> {
        let accept = if ndjson {
            "application/x-ndjson"
        } else {
            "application/json"
        };
        self.write_request(stream, "GET", path, accept, "")
    }

    fn write_request(
        &self,
        stream: &mut TcpStream,
        method: &str,
        path: &str,
        accept: &str,
        body: &str,
    ) -> Result<(), ClientError> {
        write!(
            stream,
            "{method} {path} HTTP/1.1\r\nHost: 127.0.0.1\r\nX-Namesplit-Token: {}\r\nAccept: {accept}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            self.token,
            body.len()
        )?;
        stream.flush()?;
        Ok(())
    }
}

/// NDJSON 事件流；`next_batch` 返回 `None` 表示服务端正常关闭。
pub struct EventStream {
    reader: BufReader<BodyReader>,
}

impl EventStream {
    pub fn next_batch(&mut self) -> Result<Option<Vec<Event>>, ClientError> {
        loop {
            let mut line = String::new();
            let read = self.reader.read_line(&mut line)?;
            if read == 0 {
                return Ok(None);
            }
            let line = line.trim();
            if line.is_empty() {
                continue;
            }
            let batch: Vec<Event> = serde_json::from_str(line)?;
            return Ok(Some(batch));
        }
    }
}

/// 逐查询事件。只声明中继与探针需要的字段；未知字段忽略，
/// 缺失字段用默认值，避免可选字段缺失时 Tauri 应用崩溃。
#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct Event {
    #[serde(default)]
    pub event_id: u64,
    #[serde(default)]
    pub time: String,
    #[serde(default)]
    pub domain: String,
    #[serde(default)]
    pub qtype: String,
    #[serde(default)]
    pub rule: String,
    #[serde(default)]
    pub rule_index: i64,
    #[serde(default)]
    pub rule_kind: String,
    #[serde(default)]
    pub source: String,
    #[serde(default)]
    pub group: String,
    #[serde(default)]
    pub upstream: String,
    #[serde(default)]
    pub upstream_name: String,
    #[serde(default)]
    pub duration_ms: f64,
    #[serde(default)]
    pub rcode: i64,
    #[serde(default)]
    pub rcode_name: String,
    #[serde(default)]
    pub protocol: String,
    #[serde(default)]
    pub answers: Vec<String>,
    #[serde(default)]
    pub authority: Vec<String>,
    #[serde(default)]
    pub additional: Vec<String>,
    #[serde(default)]
    pub ttl: u32,
    #[serde(default)]
    pub cache_hit: bool,
    #[serde(default, rename = "do")]
    pub dnssec_do: bool,
    #[serde(default)]
    pub ad: bool,
    #[serde(default)]
    pub request_flags: RequestFlags,
    #[serde(default)]
    pub response_flags: ResponseFlags,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub attempts: Option<Vec<Attempt>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct RequestFlags {
    #[serde(default)]
    pub rd: bool,
    #[serde(default)]
    pub cd: bool,
}

#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct ResponseFlags {
    #[serde(default)]
    pub aa: bool,
    #[serde(default)]
    pub tc: bool,
    #[serde(default)]
    pub ra: bool,
}

#[derive(Debug, Clone, Default, Deserialize, Serialize)]
pub struct Attempt {
    #[serde(default)]
    pub upstream: String,
    #[serde(default)]
    pub duration_ms: f64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

struct Response {
    status: u16,
    body: BodyReader,
}

fn read_response(stream: TcpStream) -> Result<Response, ClientError> {
    let mut reader = BufReader::new(stream);
    let status_line = read_line_limited(&mut reader)?;
    let status = parse_status(&status_line)?;

    let mut headers: HashMap<String, String> = HashMap::new();
    loop {
        let line = read_line_limited(&mut reader)?;
        if line == "\r\n" || line == "\n" {
            break;
        }
        if let Some((name, value)) = line.split_once(':') {
            headers.insert(name.trim().to_ascii_lowercase(), value.trim().to_string());
        }
    }

    let chunked = headers
        .get("transfer-encoding")
        .map(|value| value.to_ascii_lowercase().contains("chunked"))
        .unwrap_or(false);
    let content_length = headers
        .get("content-length")
        .and_then(|value| value.parse::<usize>().ok());

    let mode = if chunked {
        BodyMode::Chunked {
            remaining: 0,
            done: false,
        }
    } else if let Some(len) = content_length {
        BodyMode::Length(len)
    } else {
        BodyMode::Eof
    };

    Ok(Response {
        status,
        body: BodyReader {
            inner: reader,
            mode,
        },
    })
}

fn parse_status(line: &str) -> Result<u16, ClientError> {
    let mut parts = line.split_whitespace();
    let version = parts.next().unwrap_or_default();
    if !version.starts_with("HTTP/") {
        return Err(ClientError::Protocol(format!("非法状态行: {line:?}")));
    }
    parts
        .next()
        .and_then(|code| code.parse::<u16>().ok())
        .ok_or_else(|| ClientError::Protocol(format!("非法状态行: {line:?}")))
}

fn read_line_limited<R: BufRead>(reader: &mut R) -> Result<String, ClientError> {
    let mut line = String::new();
    let read = reader.read_line(&mut line)?;
    if read == 0 {
        return Err(ClientError::Io(io::Error::new(
            io::ErrorKind::UnexpectedEof,
            "连接提前关闭",
        )));
    }
    if line.len() > HEADER_LIMIT {
        return Err(ClientError::Protocol("响应头过长".into()));
    }
    Ok(line)
}

fn client_error_to_io(err: ClientError) -> io::Error {
    match err {
        ClientError::Io(io_err) => io_err,
        other => io::Error::new(io::ErrorKind::InvalidData, other.to_string()),
    }
}

enum BodyMode {
    Chunked { remaining: usize, done: bool },
    Length(usize),
    Eof,
}

/// 按响应头解码响应体：支持 Content-Length、chunked 与连接关闭三种定界。
struct BodyReader {
    inner: BufReader<TcpStream>,
    mode: BodyMode,
}

impl BodyReader {
    fn into_string(mut self, limit: usize) -> Result<String, ClientError> {
        let mut text = String::new();
        self.by_ref().take(limit as u64).read_to_string(&mut text)?;
        Ok(text)
    }
}

impl Read for BodyReader {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        match &mut self.mode {
            BodyMode::Eof => self.inner.read(buf),
            BodyMode::Length(remaining) => {
                if *remaining == 0 || buf.is_empty() {
                    return Ok(0);
                }
                let max = buf.len().min(*remaining);
                let read = self.inner.read(&mut buf[..max])?;
                *remaining -= read;
                Ok(read)
            }
            BodyMode::Chunked { remaining, done } => {
                if *done || buf.is_empty() {
                    return Ok(0);
                }
                if *remaining == 0 {
                    let line = read_line_limited(&mut self.inner).map_err(client_error_to_io)?;
                    let size_text = line.trim().split(';').next().unwrap_or_default();
                    let size = usize::from_str_radix(size_text, 16).map_err(|_| {
                        io::Error::new(io::ErrorKind::InvalidData, "非法 chunk 长度")
                    })?;
                    if size == 0 {
                        // 消费 trailer，直到空行。
                        loop {
                            let trailer =
                                read_line_limited(&mut self.inner).map_err(client_error_to_io)?;
                            if trailer == "\r\n" || trailer == "\n" {
                                break;
                            }
                        }
                        *done = true;
                        return Ok(0);
                    }
                    *remaining = size;
                }

                let max = buf.len().min(*remaining);
                let read = self.inner.read(&mut buf[..max])?;
                if read == 0 {
                    return Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        "chunk 数据提前结束",
                    ));
                }
                *remaining -= read;
                if *remaining == 0 {
                    // 每个 chunk 后有 CRLF；读掉它才能继续解析下一个长度。
                    let mut crlf = String::new();
                    self.inner.read_line(&mut crlf)?;
                }
                Ok(read)
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::net::TcpListener;
    use std::sync::{Arc, Mutex};

    /// 起一个一次性回环 HTTP 服务：读完请求头后把请求行交给 `responder` 写响应。
    fn serve_once(responder: impl FnOnce(&mut TcpStream, &str) + Send + 'static) -> u16 {
        let listener = TcpListener::bind(("127.0.0.1", 0)).expect("监听回环端口");
        let port = listener.local_addr().expect("读取监听端口").port();
        std::thread::spawn(move || {
            let (mut stream, _) = listener.accept().expect("接受连接");
            let mut reader = BufReader::new(stream.try_clone().expect("克隆连接"));
            let mut request_line = String::new();
            let _ = reader.read_line(&mut request_line);
            loop {
                let mut line = String::new();
                if reader.read_line(&mut line).unwrap_or(0) == 0 || line == "\r\n" || line == "\n" {
                    break;
                }
            }
            responder(&mut stream, request_line.trim_end());
            let _ = stream.flush();
        });
        port
    }

    fn test_client(port: u16) -> SidecarClient {
        SidecarClient::new(port, "0".repeat(64))
    }

    #[test]
    fn parse_status_rejects_bad_lines() {
        assert_eq!(parse_status("HTTP/1.1 200 OK").unwrap(), 200);
        assert!(parse_status("garbage").is_err());
        assert!(parse_status("HTTP/1.1 nope").is_err());
    }

    #[test]
    fn decodes_chunked_body() {
        let port = serve_once(|stream, _request_line| {
            stream
                .write_all(
                    b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n\
                      5\r\nhello\r\n6\r\n world\r\n0\r\n\r\n",
                )
                .unwrap();
        });
        let client = test_client(port);
        let mut stream = client.connect(JSON_TIMEOUT).unwrap();
        client.write_get(&mut stream, "/api/events", false).unwrap();
        let response = read_response(stream).unwrap();
        assert_eq!(response.status, 200);
        assert_eq!(response.body.into_string(1024).unwrap(), "hello world");
    }

    #[test]
    fn get_json_reads_content_length_body() {
        let port = serve_once(|stream, _request_line| {
            stream
                .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 14\r\n\r\n{\"ready\":true}")
                .unwrap();
        });
        let value = test_client(port).get_json("/api/health").unwrap();
        assert_eq!(value["ready"], serde_json::json!(true));
    }

    #[test]
    fn get_json_returns_http_error() {
        let port = serve_once(|stream, _request_line| {
            stream
                .write_all(
                    b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 25\r\n\r\n\
                      {\"error\":\"unauthorized\"}",
                )
                .unwrap();
        });
        match test_client(port).get_json("/api/health") {
            Err(ClientError::Http { status, .. }) => assert_eq!(status, 401),
            other => panic!("期望 401，实际 {other:?}"),
        }
    }

    #[test]
    fn event_stream_skips_heartbeats() {
        let port = serve_once(|stream, _request_line| {
            stream
                .write_all(
                    b"HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\n\r\n\
                      \n\n[{\"event_id\":7,\"domain\":\"a.test.\"}]\n\n",
                )
                .unwrap();
        });
        let mut stream = test_client(port).open_events().unwrap();
        let batch = stream.next_batch().unwrap().expect("应读到事件批次");
        assert_eq!(batch.len(), 1);
        assert_eq!(batch[0].event_id, 7);
        assert_eq!(batch[0].domain, "a.test.");
        assert!(stream.next_batch().unwrap().is_none(), "EOF 应返回 None");
    }

    #[test]
    fn post_json_uses_post_method() {
        let seen = Arc::new(Mutex::new(String::new()));
        let seen_in_server = seen.clone();
        let port = serve_once(move |stream, request_line| {
            *seen_in_server.lock().unwrap() = request_line.to_string();
            stream
                .write_all(
                    b"HTTP/1.1 202 Accepted\r\nContent-Length: 26\r\n\r\n\
                      {\"status\":\"shutting_down\"}",
                )
                .unwrap();
        });
        let value = test_client(port).post_json("/api/shutdown").unwrap();
        assert_eq!(value["status"], "shutting_down");
        assert!(seen
            .lock()
            .unwrap()
            .starts_with("POST /api/shutdown HTTP/1.1"));
    }
}
