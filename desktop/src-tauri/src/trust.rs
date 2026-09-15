// 应用内信任：给用户明确配对过的那一台服务器加一条信任锚，不装系统 CA、不全局关校验。
//
// 三条边界（docs/plan-desktop.md「应用内信任」）：
//   - 锚按 host:port 隔离，只在该服务器的连接上生效；链、SAN、有效期照常由系统判。
//   - 配对前不向该服务器发任何凭据：只下载公开的 /ca.crt，指纹对上才落盘。
//   - 跨网络的明文 http 一律拒绝（回环除外），免得会话与推流令牌裸奔。
//
// 落盘就一个 json：host:port → {url, root_b64}。系统已信任的服务器也记一条（没有锚），
// 供 WHIP 回环反代确认「只转发到配置的那一台」。
use std::collections::BTreeMap;
use std::fs;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use base64::Engine as _;
use reqwest::Url;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

#[cfg(target_os = "macos")]
pub mod macos;
#[cfg(target_os = "windows")]
pub mod windows;

/// 换根或删根后清掉 WebView 里缓存的放行决定。
/// 只有 WebView2 有这层缓存（按 host+证书存到 session 结束）；WKWebView 每条连接现问现答，无需清。
fn clear_webview_decisions(app: &tauri::AppHandle) {
    #[cfg(target_os = "windows")]
    windows::clear_decisions(app);
    #[cfg(not(target_os = "windows"))]
    let _ = app;
}

/// 探测与下载的上限：本机网络内应当秒回，卡住不如快失败。
const HTTP_TIMEOUT: Duration = Duration::from_secs(10);
/// /ca.crt 的体积上限：根证书 PEM 只有几 KB。
const CA_MAX_BYTES: usize = 64 * 1024;

#[derive(Clone, Serialize, Deserialize)]
pub struct ServerEntry {
    pub url: String,
    /// 配对时存下的根证书 DER（base64）。系统已信任的服务器没有这项。
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub root_b64: Option<String>,
}

#[derive(Default, Serialize, Deserialize)]
struct Config {
    #[serde(default)]
    servers: BTreeMap<String, ServerEntry>,
}

pub struct Trust {
    path: PathBuf,
    cfg: Mutex<Config>,
}

impl Trust {
    /// 读配置；文件不存在或坏了都按空配置起（配对一次即可重建，不值得打断启动）。
    pub fn load(dir: PathBuf) -> Arc<Self> {
        let path = dir.join("servers.json");
        let cfg = fs::read(&path)
            .ok()
            .and_then(|b| serde_json::from_slice::<Config>(&b).ok())
            .unwrap_or_default();
        Arc::new(Self { path, cfg: Mutex::new(cfg) })
    }

    fn save(&self, cfg: &Config) -> Result<(), String> {
        if let Some(dir) = self.path.parent() {
            fs::create_dir_all(dir).map_err(|e| format!("创建配置目录失败：{e}"))?;
        }
        let body = serde_json::to_vec_pretty(cfg).map_err(|e| format!("序列化配置失败：{e}"))?;
        fs::write(&self.path, body).map_err(|e| format!("写入配置失败：{e}"))
    }

    /// 按 host:port 取信任锚（根证书 DER）。WebView 的证书回调与 Rust 侧的 https 都用它。
    pub fn anchor(&self, host: &str, port: u16) -> Option<Vec<u8>> {
        let key = format!("{}:{}", host.to_ascii_lowercase(), port);
        let cfg = self.cfg.lock().unwrap();
        let b64 = cfg.servers.get(&key)?.root_b64.as_ref()?;
        base64::engine::general_purpose::STANDARD.decode(b64).ok()
    }

    fn put(&self, key: String, entry: ServerEntry) -> Result<(), String> {
        let mut cfg = self.cfg.lock().unwrap();
        cfg.servers.insert(key, entry);
        self.save(&cfg)
    }

    fn get(&self, key: &str) -> Option<ServerEntry> {
        self.cfg.lock().unwrap().servers.get(key).cloned()
    }

    fn forget(&self, key: &str) -> Result<(), String> {
        let mut cfg = self.cfg.lock().unwrap();
        cfg.servers.remove(key);
        self.save(&cfg)
    }
}

/// host:port（端口按 scheme 补默认值）。信任锚的隔离粒度就是这个键。
fn key_of(url: &Url) -> Result<String, String> {
    let host = url.host_str().ok_or("地址里没有主机名")?;
    let port = url.port_or_known_default().ok_or("地址里没有端口")?;
    Ok(format!("{}:{}", host.to_ascii_lowercase(), port))
}

/// 解析并规范化用户填的地址：只认 http/https，去掉路径只留 origin。
fn parse(url: &str) -> Result<Url, String> {
    let u = Url::parse(url.trim()).map_err(|_| "地址格式不对".to_string())?;
    if u.scheme() != "http" && u.scheme() != "https" {
        return Err("地址必须是 http/https".to_string());
    }
    if u.host_str().is_none() {
        return Err("地址里没有主机名".to_string());
    }
    Ok(u)
}

/// 回环主机：本机的 http 不出网卡，不算明文跨网络。
fn is_loopback(url: &Url) -> bool {
    let Some(host) = url.host_str() else { return false };
    let host = host.trim_start_matches('[').trim_end_matches(']');
    if host.eq_ignore_ascii_case("localhost") {
        return true;
    }
    host.parse::<std::net::IpAddr>().is_ok_and(|ip| ip.is_loopback())
}

/// 建 http 客户端。anchor 非空时在**系统根之外**再加一条锚，两个平台都不会关掉内置根：
/// macOS 的 native-tls 走 SecTrustSetAnchorCertificates；Windows 走 schannel，加的证书进
/// 链构建库，只有它真的出现在最终链里才补 CERT_CHAIN_POLICY_ALLOW_UNKNOWN_CA_FLAG，
/// 有效期、serverAuth、主机名仍由 CertVerifyCertificateChainPolicy 照判
/// （native-tls 0.2 `src/imp/schannel.rs` + schannel 0.1 `tls_stream.rs` 的 validate）。
/// insecure 只给 /ca.crt 下载用。
fn client(anchor: Option<&[u8]>, insecure: bool) -> Result<reqwest::Client, String> {
    let mut b = reqwest::Client::builder()
        .timeout(HTTP_TIMEOUT)
        .user_agent("hearth-desktop");
    if let Some(der) = anchor {
        let cert = reqwest::Certificate::from_der(der).map_err(|e| format!("根证书不可用：{e}"))?;
        b = b.add_root_certificate(cert);
    }
    if insecure {
        // 仅用于下载公开材料 /ca.crt：拿到的字节要按指纹核对，核不上就丢掉。
        b = b.danger_accept_invalid_certs(true).danger_accept_invalid_hostnames(true);
    }
    b.build().map_err(|e| format!("创建 http 客户端失败：{e}"))
}

/// 对外的 http 客户端：按 host:port 取锚，没有锚就是纯系统根。
/// Rust 侧一切出站 https（探测、WHIP 回环反代）都从这里拿客户端。
pub fn client_for(trust: &Trust, url: &Url) -> Result<reqwest::Client, String> {
    let host = url.host_str().ok_or("地址里没有主机名")?;
    let port = url.port_or_known_default().ok_or("地址里没有端口")?;
    client(trust.anchor(host, port).as_deref(), false)
}

#[derive(Serialize)]
pub struct CheckResult {
    pub ok: bool,
    /// ok=false 时的原因：untrusted / unreachable / not_https / not_hearth / bad_url
    pub reason: &'static str,
    /// 给界面直接展示的中文说明（ok=true 时为空）
    pub detail: String,
}

impl CheckResult {
    fn ok() -> Self {
        Self { ok: true, reason: "", detail: String::new() }
    }
    fn bad(reason: &'static str, detail: impl Into<String>) -> Self {
        Self { ok: false, reason, detail: detail.into() }
    }
}

/// 证书不被信任，还是压根连不上？reqwest 把两者都归进 is_connect，只能顺着
/// 错误链认 TLS 的字样（native-tls 在 macOS 上会带上 Security.framework 的原文与 OSStatus）。
fn looks_like_tls(err: &reqwest::Error) -> bool {
    let mut text = String::new();
    let mut cur: Option<&(dyn std::error::Error + 'static)> = Some(err);
    while let Some(e) = cur {
        text.push_str(&e.to_string());
        text.push(' ');
        cur = e.source();
    }
    let text = text.to_ascii_lowercase();
    [
        "certificate",
        "cert verify",
        "tls",
        "ssl",
        "trust",
        "-9807",
        "-9808",
        "-9813",
        "-9814",
        "-25280",
    ]
    .iter()
    .any(|m| text.contains(m))
}

/// 真实校验一次 /api/site：连得上、TLS 过、回的是 hearth 才算通。
async fn probe(client: &reqwest::Client, base: &Url) -> CheckResult {
    let url = match base.join("/api/site") {
        Ok(u) => u,
        Err(e) => return CheckResult::bad("bad_url", format!("地址拼接失败：{e}")),
    };
    let resp = match client.get(url).send().await {
        Ok(r) => r,
        Err(e) if looks_like_tls(&e) => {
            return CheckResult::bad("untrusted", "这台服务器的证书不被系统信任（多半是自签）")
        }
        Err(e) => return CheckResult::bad("unreachable", format!("连不上这台服务器：{e}")),
    };
    if !resp.status().is_success() {
        return CheckResult::bad("not_hearth", format!("服务器返回了 {}", resp.status().as_u16()));
    }
    let body = match resp.bytes().await {
        Ok(b) => b,
        Err(e) => return CheckResult::bad("unreachable", format!("读取响应失败：{e}")),
    };
    match serde_json::from_slice::<serde_json::Value>(&body) {
        Ok(v) if v.get("policy").is_some() => CheckResult::ok(),
        _ => CheckResult::bad("not_hearth", "这个地址回的不是 hearth 服务器"),
    }
}

/// 探测服务器：能直连（系统根 + 已配对的锚）就 ok，否则给出可判的原因。
#[tauri::command(async)]
pub async fn check_server(
    trust: tauri::State<'_, Arc<Trust>>,
    url: String,
) -> Result<CheckResult, String> {
    check(&trust, &url).await
}

pub async fn check(trust: &Trust, url: &str) -> Result<CheckResult, String> {
    let u = match parse(url) {
        Ok(u) => u,
        Err(e) => return Ok(CheckResult::bad("bad_url", e)),
    };
    if u.scheme() == "http" && !is_loopback(&u) {
        return Ok(CheckResult::bad(
            "not_https",
            "跨网络的明文 http 不传凭据，请给服务器配上 https（自签也行）",
        ));
    }
    let key = key_of(&u)?;
    let client = client_for(trust, &u)?;
    let res = probe(&client, &u).await;
    if res.ok {
        // 记一条（保留已有的锚）：WHIP 回环反代据此确认转发目标是配置里的那一台。
        let root_b64 = trust.get(&key).and_then(|e| e.root_b64);
        trust.put(key, ServerEntry { url: origin_of(&u), root_b64 })?;
    }
    Ok(res)
}

/// 配对：下载公开的 /ca.crt，指纹对上才作为该服务器的信任锚落盘，落盘后再真实校验一次。
#[tauri::command(async, rename_all = "snake_case")]
pub async fn pair_server(
    app: tauri::AppHandle,
    trust: tauri::State<'_, Arc<Trust>>,
    url: String,
    fingerprint_sha256: String,
) -> Result<(), String> {
    let result = pair(&trust, &url, &fingerprint_sha256).await;
    if result.is_ok() {
        // 同一台服务器换了根：WebView 里按旧根做过的放行决定必须作废。
        clear_webview_decisions(&app);
    }
    result
}

pub async fn pair(trust: &Trust, url: &str, fingerprint_sha256: &str) -> Result<(), String> {
    let u = parse(url)?;
    if u.scheme() != "https" {
        return Err("只有 https 需要配对".to_string());
    }
    let want = normalize_fingerprint(fingerprint_sha256)?;
    let key = key_of(&u)?;

    // 只有「连得上但不被信任」才允许配对：别的失败配了也没用，反而会掩盖真实原因。
    let pre = probe(&client_for(trust, &u)?, &u).await;
    if pre.ok {
        return Err("这台服务器的证书已经被信任，不需要配对".to_string());
    }
    if pre.reason != "untrusted" {
        return Err(pre.detail);
    }

    let ca_url = u.join("/ca.crt").map_err(|e| format!("地址拼接失败：{e}"))?;
    let resp = client(None, true)?
        .get(ca_url)
        .send()
        .await
        .map_err(|e| format!("下载根证书失败：{e}"))?;
    if !resp.status().is_success() {
        return Err(format!(
            "这台服务器没有提供根证书下载（/ca.crt 返回 {}）",
            resp.status().as_u16()
        ));
    }
    let body = resp.bytes().await.map_err(|e| format!("读取根证书失败：{e}"))?;
    if body.len() > CA_MAX_BYTES {
        return Err("根证书文件异常地大，已丢弃".to_string());
    }
    let der = first_cert_der(&body)?;
    let got = hex_lower(&Sha256::digest(&der));
    if got != want {
        // 指纹对不上：什么都不落盘，也不给出「正确指纹」，免得把校验退化成复读。
        return Err("指纹不一致，已拒绝。请向管理员再确认一次根证书指纹".to_string());
    }

    // 拿这条锚重新做一次**真实**校验（链、SAN、有效期都要过），过了才算配对完成。
    let verified = probe(&client(Some(&der), false)?, &u).await;
    if !verified.ok {
        return Err(format!("根证书指纹对上了，但校验仍未通过：{}", verified.detail));
    }
    trust.put(
        key,
        ServerEntry {
            url: origin_of(&u),
            root_b64: Some(base64::engine::general_purpose::STANDARD.encode(&der)),
        },
    )
}

/// 忘记服务器：删掉本机这条配置（含信任锚）。网页那边的登录态自己清。
#[tauri::command(async)]
pub async fn forget_server(
    app: tauri::AppHandle,
    trust: tauri::State<'_, Arc<Trust>>,
    url: String,
) -> Result<(), String> {
    let result = forget(&trust, &url);
    if result.is_ok() {
        clear_webview_decisions(&app);
    }
    result
}

pub fn forget(trust: &Trust, url: &str) -> Result<(), String> {
    let u = parse(url)?;
    trust.forget(&key_of(&u)?)
}

fn origin_of(u: &Url) -> String {
    u.origin().ascii_serialization()
}

/// 指纹容忍大小写与冒号/空白分隔，只认 32 字节的 SHA-256。
fn normalize_fingerprint(s: &str) -> Result<String, String> {
    let hex: String = s
        .chars()
        .filter(|c| !c.is_whitespace() && *c != ':' && *c != '-')
        .collect::<String>()
        .to_ascii_lowercase();
    if hex.len() != 64 || !hex.chars().all(|c| c.is_ascii_hexdigit()) {
        return Err("指纹要填 64 位十六进制的 SHA-256（冒号分隔也行）".to_string());
    }
    Ok(hex)
}

fn hex_lower(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

/// 从 PEM 里取第一张证书的 DER。服务端 /ca.crt 就是一张根证书的 PEM。
fn first_cert_der(body: &[u8]) -> Result<Vec<u8>, String> {
    const BEGIN: &str = "-----BEGIN CERTIFICATE-----";
    const END: &str = "-----END CERTIFICATE-----";
    let text = std::str::from_utf8(body).map_err(|_| "根证书不是 PEM 文本".to_string())?;
    let start = text.find(BEGIN).ok_or("根证书里没有 CERTIFICATE 段")?;
    let rest = &text[start + BEGIN.len()..];
    let end = rest.find(END).ok_or("根证书 PEM 不完整")?;
    let b64: String = rest[..end].chars().filter(|c| !c.is_whitespace()).collect();
    base64::engine::general_purpose::STANDARD
        .decode(b64)
        .map_err(|e| format!("根证书 base64 解码失败：{e}"))
}

