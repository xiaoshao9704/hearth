// WebView2 的证书错误事件：网页里每条 TLS 校验失败的请求（导航与 XHR/fetch 都算）都会问一次
// `ServerCertificateErrorDetected`，wry/Tauri 不暴露它，只能从 ICoreWebView2Controller 取
// ICoreWebView2 再 cast 到 ICoreWebView2_14 自己挂。
//
// 与 macOS 那条路的两处差别（看代码时容易误判，写在这里）：
//   - 事件只给证书的 PEM（叶 + 签发链），没有系统的信任评估对象，所以链校验在 Rust 侧做：
//     rustls-webpki 以配对存下的那张根为唯一锚，按 serverAuth、有效期、主机名判。锚是唯一的
//     不影响公网证书：那种证书 WebView2 自己就判过了，根本不会触发这个事件。
//   - ALWAYS_ALLOW 会按 host+证书在本 session 内缓存，所以换根/删根后必须调
//     ClearServerCertificateErrorActions，否则旧决定还在。
//
// 没配对过的 host:port 一律 DEFAULT：对非导航请求就是直接拒绝，对导航是 WebView2 自己的
// 拦截页。没有「忽略所有错误」的分支。
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use base64::Engine as _;
use reqwest::Url;
use rustls_pki_types::{CertificateDer, ServerName, UnixTime};
use tauri::Manager;
use webpki::{anchor_from_trusted_cert, EndEntityCert, KeyUsage};
use webview2_com::Microsoft::Web::WebView2::Win32::{
    ICoreWebView2ServerCertificateErrorDetectedEventArgs, ICoreWebView2_14,
    COREWEBVIEW2_SERVER_CERTIFICATE_ERROR_ACTION_ALWAYS_ALLOW,
    COREWEBVIEW2_SERVER_CERTIFICATE_ERROR_ACTION_DEFAULT,
};
use webview2_com::{
    ClearServerCertificateErrorActionsCompletedHandler, ServerCertificateErrorDetectedEventHandler,
};
use windows::core::{Interface, PWSTR};

use super::Trust;

/// 在主线程上挂证书错误回调（调用点是 tauri 的 with_webview）。
pub fn install(webview: &tauri::webview::PlatformWebview, trust: Arc<Trust>) {
    let Some(core) = core_webview(webview) else {
        return;
    };
    let handler = ServerCertificateErrorDetectedEventHandler::create(Box::new(move |_, args| {
        if let Some(args) = args {
            let action = if allowed(&args, &trust) {
                COREWEBVIEW2_SERVER_CERTIFICATE_ERROR_ACTION_ALWAYS_ALLOW
            } else {
                COREWEBVIEW2_SERVER_CERTIFICATE_ERROR_ACTION_DEFAULT
            };
            let _ = unsafe { args.SetAction(action) };
        }
        Ok(())
    }));
    let mut token = 0i64;
    if let Err(e) = unsafe { core.add_ServerCertificateErrorDetected(&handler, &mut token) } {
        eprintln!("信任回调未挂上：{e}");
    }
}

/// 清掉 WebView2 按 host+证书缓存的放行决定。换根或删根后必须调，否则旧决定继续生效。
pub fn clear_decisions(app: &tauri::AppHandle) {
    let Some(win) = app.get_webview_window("main") else {
        return;
    };
    // with_webview 把闭包投递到主线程上跑：WebView2 的接口只能在事件循环所在线程上碰。
    let _ = win.with_webview(|webview| {
        let Some(core) = core_webview(&webview) else {
            return;
        };
        let done = ClearServerCertificateErrorActionsCompletedHandler::create(Box::new(|_| Ok(())));
        let _ = unsafe { core.ClearServerCertificateErrorActions(&done) };
    });
}

fn core_webview(webview: &tauri::webview::PlatformWebview) -> Option<ICoreWebView2_14> {
    let core = match unsafe { webview.controller().CoreWebView2() } {
        Ok(c) => c,
        Err(e) => {
            eprintln!("取 WebView2 实例失败：{e}");
            return None;
        }
    };
    match core.cast::<ICoreWebView2_14>() {
        Ok(c) => Some(c),
        // 事件要 WebView2 运行时 1.0.1466.46+：拿不到接口就当没有应用内信任，不降级放行。
        Err(e) => {
            eprintln!("WebView2 运行时不支持证书错误事件：{e}");
            None
        }
    }
}

/// 判定这条请求能不能放行：host:port 有锚，且证书链在这条锚下完整通过。
fn allowed(args: &ICoreWebView2ServerCertificateErrorDetectedEventArgs, trust: &Trust) -> bool {
    inspect(args, trust).unwrap_or(false)
}

fn inspect(
    args: &ICoreWebView2ServerCertificateErrorDetectedEventArgs,
    trust: &Trust,
) -> Option<bool> {
    let mut uri = PWSTR::null();
    unsafe { args.RequestUri(&mut uri) }.ok()?;
    let url = Url::parse(&webview2_com::take_pwstr(uri)).ok()?;
    let host = url.host_str()?;
    let port = url.port_or_known_default()?;
    let root = trust.anchor(host, port)?;

    let cert = unsafe { args.ServerCertificate() }.ok()?;
    let mut pem = PWSTR::null();
    unsafe { cert.ToPemEncoding(&mut pem) }.ok()?;
    let leaf = der_from_pem(&webview2_com::take_pwstr(pem))?;
    let chain = unsafe { cert.PemEncodedIssuerCertificateChain() }.ok()?;
    let mut count = 0u32;
    unsafe { chain.Count(&mut count) }.ok()?;
    let mut issuers = Vec::new();
    for i in 0..count {
        let mut item = PWSTR::null();
        if unsafe { chain.GetValueAtIndex(i, &mut item) }.is_err() {
            continue;
        }
        if let Some(der) = der_from_pem(&webview2_com::take_pwstr(item)) {
            issuers.push(der);
        }
    }
    // ServerName 不收方括号；锚的键沿用 url 的 host 原样，两者不要混。
    let name = host.trim_start_matches('[').trim_end_matches(']');
    Some(verify(&root, &leaf, &issuers, name))
}

/// 链校验：以 root 为唯一信任锚，serverAuth 用途、有效期、主机名（域名与 IP 都认）全过才算通。
fn verify(root_der: &[u8], leaf_der: &[u8], issuers: &[Vec<u8>], host: &str) -> bool {
    let root = CertificateDer::from(root_der);
    let Ok(anchor) = anchor_from_trusted_cert(&root) else {
        return false;
    };
    let leaf = CertificateDer::from(leaf_der);
    let Ok(cert) = EndEntityCert::try_from(&leaf) else {
        return false;
    };
    // 服务器常把根一起发下来；它已经是锚，留在中间证书里只会多走一次无谓的路径尝试。
    let intermediates: Vec<CertificateDer> = issuers
        .iter()
        .filter(|der| der.as_slice() != root_der)
        .map(|der| CertificateDer::from(der.as_slice()))
        .collect();
    let Ok(since_epoch) = SystemTime::now().duration_since(UNIX_EPOCH) else {
        return false;
    };
    if cert
        .verify_for_usage(
            webpki::ALL_VERIFICATION_ALGS,
            &[anchor],
            &intermediates,
            UnixTime::since_unix_epoch(since_epoch),
            KeyUsage::server_auth(),
            None,
            None,
        )
        .is_err()
    {
        return false;
    }
    let Ok(name) = ServerName::try_from(host) else {
        return false;
    };
    cert.verify_is_valid_for_subject_name(&name).is_ok()
}

/// 取一张证书的 DER。WebView2 的 PEM 是否带 BEGIN/END 头尾，文档没写死，两种都收。
fn der_from_pem(text: &str) -> Option<Vec<u8>> {
    const BEGIN: &str = "-----BEGIN CERTIFICATE-----";
    const END: &str = "-----END CERTIFICATE-----";
    let body = match (text.find(BEGIN), text.find(END)) {
        (Some(s), Some(e)) if e > s + BEGIN.len() => &text[s + BEGIN.len()..e],
        _ => text,
    };
    let b64: String = body.chars().filter(|c| !c.is_whitespace()).collect();
    base64::engine::general_purpose::STANDARD.decode(b64).ok()
}
