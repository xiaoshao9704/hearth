// WHIP 回环反代：把 whipclientsink 的 https 请求接到我们自己的信任判定上。
//
// 为什么要有它：whipclientsink 的 signaller 只有 whip-endpoint / auth-token / timeout /
// use-link-headers / manual-sdp-munging 五个属性（gst-plugins-rs 0.15，本机
// `gst-inspect-1.0 whipclientsink` 与上游源码一致），没有任何 TLS/CA 入口，
// 自签服务器的锚没法交给它。于是发布期间在本机开一个只绑 127.0.0.1 的监听，
// 它推明文回环，我们用带锚的客户端（系统根 + 该服务器的锚）转发出去。
//
// 这不是通用代理，边界写死：
//   - 只绑回环、端口随机、路径前缀是随机 secret（同机其它进程猜不到也就用不了）；
//   - 只转发到 start 时给定的那一台服务器，目标主机不可由请求改写；
//   - 只放行 WHIP 用到的 POST / PATCH / DELETE；
//   - 只在一次发布期间存活，stop 即关。
use std::convert::Infallible;
use std::net::TcpListener as StdListener;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use http_body_util::{BodyExt, Full};
use hyper::body::{Bytes, Incoming};
use hyper::header::{HeaderName, HeaderValue};
use hyper::service::service_fn;
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use reqwest::Url;

/// SDP offer 与 answer 都是几十 KB 级别，给足余量即可。
const MAX_BODY: usize = 1 << 20;

/// 不该原样转发的逐跳头（长度与编码由两侧各自决定）。
const SKIP_HEADERS: [&str; 6] = [
    "host",
    "connection",
    "content-length",
    "transfer-encoding",
    "accept-encoding",
    "keep-alive",
];

struct Ctx {
    /// 上游 origin，形如 https://example.com:8443
    origin: String,
    /// 随机路径前缀，形如 /3f2a…
    secret: String,
    /// 回环基址（含 secret 前缀），改写 Location 用
    local_base: String,
    client: reqwest::Client,
    closed: Arc<AtomicBool>,
}

pub struct Proxy {
    /// 交给 whipclientsink 的端点
    pub endpoint: String,
    closed: Arc<AtomicBool>,
    task: tauri::async_runtime::JoinHandle<()>,
}

impl Proxy {
    /// 起监听并返回回环端点。target 是真正的 WHIP 地址（含 /providers/{alias}/w/{频道}）。
    pub fn start(target: &Url, client: reqwest::Client) -> Result<Self, String> {
        let origin = target.origin().ascii_serialization();
        let secret = random_hex(16)?;
        let std_listener =
            StdListener::bind(("127.0.0.1", 0)).map_err(|e| format!("回环监听失败：{e}"))?;
        std_listener
            .set_nonblocking(true)
            .map_err(|e| format!("回环监听失败：{e}"))?;
        let port = std_listener
            .local_addr()
            .map_err(|e| format!("回环监听失败：{e}"))?
            .port();
        let local_base = format!("http://127.0.0.1:{port}/{secret}");
        let mut endpoint = format!("{local_base}{}", target.path());
        if let Some(q) = target.query() {
            endpoint.push('?');
            endpoint.push_str(q);
        }
        let closed = Arc::new(AtomicBool::new(false));
        let ctx = Arc::new(Ctx {
            origin,
            secret: format!("/{secret}"),
            local_base,
            client,
            closed: closed.clone(),
        });

        let task = tauri::async_runtime::spawn(async move {
            let listener = match tokio::net::TcpListener::from_std(std_listener) {
                Ok(l) => l,
                Err(e) => {
                    eprintln!("WHIP 回环反代启动失败：{e}");
                    return;
                }
            };
            loop {
                let Ok((stream, _)) = listener.accept().await else { continue };
                let ctx = ctx.clone();
                tauri::async_runtime::spawn(async move {
                    let svc = service_fn(move |req| forward(req, ctx.clone()));
                    let _ = hyper::server::conn::http1::Builder::new()
                        .serve_connection(TokioIo::new(stream), svc)
                        .await;
                });
            }
        });
        Ok(Self { endpoint, closed, task })
    }

    pub fn stop(self) {
        self.closed.store(true, Ordering::Relaxed);
        self.task.abort();
    }
}

async fn forward(req: Request<Incoming>, ctx: Arc<Ctx>) -> Result<Response<Full<Bytes>>, Infallible> {
    if ctx.closed.load(Ordering::Relaxed) {
        return Ok(status(StatusCode::SERVICE_UNAVAILABLE));
    }
    if !matches!(req.method(), &Method::POST | &Method::PATCH | &Method::DELETE) {
        return Ok(status(StatusCode::METHOD_NOT_ALLOWED));
    }
    let path_q = req.uri().path_and_query().map(|p| p.as_str()).unwrap_or("/");
    let Some(rest) = path_q.strip_prefix(&ctx.secret) else {
        return Ok(status(StatusCode::NOT_FOUND));
    };
    if !rest.is_empty() && !rest.starts_with('/') {
        return Ok(status(StatusCode::NOT_FOUND));
    }
    let url = format!("{}{}", ctx.origin, rest);

    let method = reqwest::Method::from_bytes(req.method().as_str().as_bytes())
        .unwrap_or(reqwest::Method::POST);
    let mut out = ctx.client.request(method, &url);
    for (name, value) in req.headers() {
        if SKIP_HEADERS.contains(&name.as_str()) {
            continue;
        }
        out = out.header(name.as_str(), value.as_bytes());
    }
    let body = match req.into_body().collect().await {
        Ok(b) => b.to_bytes(),
        Err(_) => return Ok(status(StatusCode::BAD_REQUEST)),
    };
    if body.len() > MAX_BODY {
        return Ok(status(StatusCode::PAYLOAD_TOO_LARGE));
    }
    let resp = match out.body(body.to_vec()).send().await {
        Ok(r) => r,
        Err(e) => {
            eprintln!("WHIP 回环反代转发失败：{e}");
            return Ok(status(StatusCode::BAD_GATEWAY));
        }
    };

    let mut builder = Response::builder().status(resp.status().as_u16());
    for (name, value) in resp.headers() {
        if SKIP_HEADERS.contains(&name.as_str()) {
            continue;
        }
        let val = if name.as_str() == "location" {
            match rewrite_location(value.to_str().unwrap_or(""), &ctx) {
                Some(v) => HeaderValue::from_str(&v).unwrap_or_else(|_| value.clone()),
                None => value.clone(),
            }
        } else {
            value.clone()
        };
        if let Ok(name) = HeaderName::from_bytes(name.as_str().as_bytes()) {
            builder = builder.header(name, val);
        }
    }
    let body = resp.bytes().await.unwrap_or_default();
    Ok(builder
        .body(Full::new(Bytes::from(body.to_vec())))
        .unwrap_or_else(|_| status(StatusCode::BAD_GATEWAY)))
}

/// 会话资源地址要回指回环，否则 whipclientsink 的 PATCH/DELETE 会绕过我们直连上游
/// （那条连接没有信任锚）。规则与服务端 rewriteWHIPLocation 对称：绝对 URL 与根相对
/// 都只取路径接到回环基址上，纯相对本就相对请求路径解析，原样不动。
fn rewrite_location(loc: &str, ctx: &Ctx) -> Option<String> {
    if loc.is_empty() {
        return None;
    }
    if let Ok(u) = Url::parse(loc) {
        let mut out = format!("{}{}", ctx.local_base, u.path());
        if let Some(q) = u.query() {
            out.push('?');
            out.push_str(q);
        }
        return Some(out);
    }
    if loc.starts_with('/') {
        return Some(format!("{}{}", ctx.local_base, loc));
    }
    None
}

fn status(code: StatusCode) -> Response<Full<Bytes>> {
    Response::builder()
        .status(code)
        .body(Full::new(Bytes::new()))
        .expect("构造空响应不会失败")
}

fn random_hex(bytes: usize) -> Result<String, String> {
    let mut buf = vec![0u8; bytes];
    getrandom::fill(&mut buf).map_err(|e| format!("取随机数失败：{e}"))?;
    Ok(buf.iter().map(|b| format!("{b:02x}")).collect())
}
