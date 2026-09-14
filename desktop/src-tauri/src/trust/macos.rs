// WKWebView 的证书回调：页面的 fetch 与 wss 每条新 TLS 连接都会问一次
// `webView:didReceiveAuthenticationChallenge:completionHandler:`，放行不缓存。
//
// wry 的 navigation delegate 没有这个方法，也没有公开 API，而 navigationDelegate 只有
// 一个槽位——直接顶掉会丢掉 Tauri 的 IPC 初始化脚本、页面加载事件与下载路由。所以这里
// 建一个代理对象：只实现挑战方法，respondsToSelector: 与 forwardingTargetForSelector:
// 把其余一切转回原 delegate。
//
// 判定本身不放水：只有 host:port 命中已配对服务器时才拿那条锚做 SecTrust 评估，
// 通过才 useCredential；其余一律 performDefaultHandling 交回系统（系统信任的证书
// 由 WebKit 自己过）。没有「忽略所有错误」的分支。
use std::ffi::c_void;
use std::ptr::null_mut;
use std::sync::Arc;

use objc2::rc::Retained;
use objc2::runtime::{AnyObject, Bool, NSObject, Sel};
use objc2::{define_class, msg_send, sel, AllocAnyThread, ClassType, DefinedClass};
use objc2_core_foundation::{CFArray, CFData, CFError, CFRetained};
use objc2_foundation::{
    NSInteger, NSURLAuthenticationChallenge, NSURLAuthenticationMethodServerTrust, NSURLCredential,
};
use objc2_security::{SecCertificate, SecTrust};

use super::Trust;

/// NSURLSessionAuthChallengeDisposition
const USE_CREDENTIAL: NSInteger = 0;
const PERFORM_DEFAULT_HANDLING: NSInteger = 1;

struct Ivars {
    /// 原 navigation delegate（wry 的那个），其余选择器全部转发给它
    inner: Retained<AnyObject>,
    trust: Arc<Trust>,
}

define_class!(
    #[unsafe(super(NSObject))]
    #[thread_kind = AllocAnyThread]
    #[name = "HearthTrustNavigationDelegate"]
    #[ivars = Ivars]
    struct TrustDelegate;

    impl TrustDelegate {
        // WKWebView 在 setNavigationDelegate: 时就把「delegate 实现了哪些方法」缓存下来，
        // 所以必须如实替原 delegate 回答，否则它的回调会被整批跳过。
        #[unsafe(method(respondsToSelector:))]
        fn responds_to_selector(&self, selector: Sel) -> Bool {
            if selector == sel!(webView:didReceiveAuthenticationChallenge:completionHandler:) {
                return Bool::YES;
            }
            unsafe { msg_send![&*self.ivars().inner, respondsToSelector: selector] }
        }

        #[unsafe(method(forwardingTargetForSelector:))]
        fn forwarding_target(&self, _selector: Sel) -> *mut AnyObject {
            Retained::as_ptr(&self.ivars().inner) as *mut AnyObject
        }

        #[unsafe(method(webView:didReceiveAuthenticationChallenge:completionHandler:))]
        fn did_receive_challenge(
            &self,
            _webview: *mut AnyObject,
            challenge: &NSURLAuthenticationChallenge,
            handler: &block2::DynBlock<dyn Fn(NSInteger, *mut NSURLCredential)>,
        ) {
            let space = challenge.protectionSpace();
            let method = space.authenticationMethod();
            if &*method != unsafe { NSURLAuthenticationMethodServerTrust } {
                handler.call((PERFORM_DEFAULT_HANDLING, null_mut()));
                return;
            }
            let host = space.host().to_string();
            let port = space.port().clamp(0, u16::MAX as NSInteger) as u16;
            let Some(der) = self.ivars().trust.anchor(&host, port) else {
                handler.call((PERFORM_DEFAULT_HANDLING, null_mut()));
                return;
            };
            let raw: *mut SecTrust = unsafe { msg_send![&*space, serverTrust] };
            if raw.is_null() {
                handler.call((PERFORM_DEFAULT_HANDLING, null_mut()));
                return;
            }
            if !evaluate(unsafe { &*raw }, &der) {
                handler.call((PERFORM_DEFAULT_HANDLING, null_mut()));
                return;
            }
            let cred: Retained<NSURLCredential> =
                unsafe { msg_send![NSURLCredential::class(), credentialForTrust: raw] };
            handler.call((USE_CREDENTIAL, Retained::as_ptr(&cred) as *mut NSURLCredential));
        }
    }
);

impl TrustDelegate {
    fn new(inner: Retained<AnyObject>, trust: Arc<Trust>) -> Retained<Self> {
        let this = Self::alloc().set_ivars(Ivars { inner, trust });
        unsafe { msg_send![super(this), init] }
    }
}

/// 拿这条根做一次完整的信任评估：链、SAN、有效期照旧由系统判，我们只是多给一个锚。
/// anchorCertificatesOnly=false：系统内置根仍然有效（配对过的服务器换成公网证书也能用）。
fn evaluate(trust: &SecTrust, root_der: &[u8]) -> bool {
    let data = CFData::from_bytes(root_der);
    let Some(cert) = (unsafe { SecCertificate::with_data(None, &data) }) else {
        return false;
    };
    let anchors = CFArray::from_objects(&[&*cert]);
    let anchors: CFRetained<CFArray> = unsafe { CFRetained::cast_unchecked(anchors) };
    unsafe {
        if trust.set_anchor_certificates(Some(&anchors)) != 0 {
            return false;
        }
        if trust.set_anchor_certificates_only(false) != 0 {
            return false;
        }
        let mut err: *mut CFError = null_mut();
        let ok = trust.evaluate_with_error(&mut err);
        if !err.is_null() {
            CFRetained::from_raw(std::ptr::NonNull::new_unchecked(err));
        }
        ok
    }
}

/// 在主线程上把代理挂到 WKWebView 上（调用点是 tauri 的 with_webview）。
///
/// # Safety
/// `webview` 必须是有效的 WKWebView 指针。
pub unsafe fn install(webview: *mut c_void, trust: Arc<Trust>) {
    if webview.is_null() {
        return;
    }
    let webview = unsafe { &*(webview as *mut AnyObject) };
    let inner: Option<Retained<AnyObject>> = unsafe { msg_send![webview, navigationDelegate] };
    let Some(inner) = inner else {
        eprintln!("信任代理未挂上：WKWebView 没有 navigationDelegate");
        return;
    };
    let delegate = TrustDelegate::new(inner, trust);
    let _: () = unsafe { msg_send![webview, setNavigationDelegate: &*delegate] };
    // navigationDelegate 是 weak 属性，代理必须由我们强持有。它与主窗口同寿，
    // 故意泄漏这份强引用（Retained 不是 Send，放进 AppState 反而要额外的 unsafe）。
    std::mem::forget(delegate);
}
