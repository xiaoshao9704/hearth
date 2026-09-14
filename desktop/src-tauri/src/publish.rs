// WHIP 发布管线：采集 → appsrc → VideoToolbox 硬编 → whipclientsink。
//
// 两条定死的取舍（来自 docs/plan-desktop.md 的 PoC）：
//   - congestion-control=disabled 走固定码率。webrtcsink 的带宽估计驱动不了 vtenc_*，
//     开着只会让码率估算空转；OBS 走的也是固定码率。
//   - vtenc_* 必须显式 max-keyframe-interval，否则 WHIP 会话建起来后观众永远等不到关键帧。
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;
use gstreamer_webrtc as gst_webrtc;
use reqwest::Url;
use serde::Serialize;
use tauri::{AppHandle, Emitter, Manager};

use crate::capture;
use crate::trust::{self, Trust};
use crate::whipproxy;

/// ICE 连上之前/断开之后的容忍窗口：webrtc 自己会重试，短暂 disconnected 不算失败。
const ICE_FAIL_WINDOW: Duration = Duration::from_secs(5);
/// 建流后多久还一帧没编出来就认定采集/编码这条链没跑起来。
const NO_FRAME_WINDOW: Duration = Duration::from_secs(15);
/// 看门狗的轮询间隔（在 Rust 侧，不是网页定时器——WKWebView 后台会挂起 JS 定时器）。
const WATCH_TICK: Duration = Duration::from_millis(500);

/// 推给网页的发布状态。目前只在出错时推一次：网页据此复位按钮并提示。
#[derive(Clone, Serialize)]
pub struct PublishState {
    pub running: bool,
    pub error: Option<String>,
}

/// 采集侧（SCK 回调线程）往管线里推数据的入口。
#[derive(Clone)]
pub struct Sink {
    pub video: gst_app::AppSrc,
    pub audio: gst_app::AppSrc,
    error: Arc<Mutex<Option<String>>>,
}

impl Sink {
    /// 只记第一条错误：后续同因错误会连成串，头一条才有诊断价值。
    pub fn fail(&self, msg: impl Into<String>) {
        let mut slot = self.error.lock().unwrap();
        if slot.is_none() {
            *slot = Some(msg.into());
        }
    }
}

/// 测试源模式：无人值守验证 WHIP 全链路用（不碰 ScreenCaptureKit，也就不需要屏幕录制授权）。
pub fn testsrc_mode() -> bool {
    std::env::var("HEARTH_DESKTOP_TESTSRC").is_ok_and(|v| v == "1")
}

static GST_INIT: OnceLock<Result<(), String>> = OnceLock::new();

pub fn init_gst() -> Result<(), String> {
    GST_INIT
        .get_or_init(|| gst::init().map_err(|e| format!("GStreamer 初始化失败：{e}")))
        .clone()
}

#[derive(Serialize)]
pub struct Stats {
    pub running: bool,
    pub frames: u64,
    pub bitrate_kbps: u32,
    pub codec: String,
    pub error: Option<String>,
}

pub struct Publisher {
    pipeline: gst::Pipeline,
    capture: Option<capture::Capture>,
    /// https 推流时的回环反代（明文直连时为 None）
    proxy: Option<whipproxy::Proxy>,
    frames: Arc<AtomicU64>,
    error: Arc<Mutex<Option<String>>>,
    stopped: Arc<AtomicBool>,
    bitrate_kbps: u32,
    codec: String,
}

/// 发布看门狗：ICE 连续坏满窗口、或建流后迟迟没有画面，都记为错误、推给网页并收管线。
/// 判定放在 Rust 侧而不是网页轮询——WKWebView 后台会挂起 JS 定时器，轮询等于没有。
fn watchdog(
    app: AppHandle,
    stopped: Arc<AtomicBool>,
    error: Arc<Mutex<Option<String>>>,
    frames: Arc<AtomicU64>,
    ice_bad: Arc<Mutex<Option<Instant>>>,
) {
    let started = Instant::now();
    std::thread::spawn(move || loop {
        std::thread::sleep(WATCH_TICK);
        if stopped.load(Ordering::Relaxed) {
            return;
        }
        let bus_err = error.lock().unwrap().clone();
        let ice_since = *ice_bad.lock().unwrap();
        let msg = if let Some(e) = bus_err {
            e
        } else if ice_since.is_some_and(|t| t.elapsed() > ICE_FAIL_WINDOW) {
            "投屏连接已断开或未能建立（ICE 失败），已停止推流".to_string()
        } else if started.elapsed() > NO_FRAME_WINDOW && frames.load(Ordering::Relaxed) == 0 {
            "采集到编码这条链没有产出画面，已停止推流".to_string()
        } else {
            continue;
        };
        {
            let mut slot = error.lock().unwrap();
            if slot.is_none() {
                *slot = Some(msg.clone());
            }
        }
        let _ = app.emit("publish-state", PublishState { running: false, error: Some(msg) });
        // 自己把发布收干净：网页不一定在前台，不能指望它回调过来才停
        app.state::<crate::AppState>().stop();
        return;
    });
}

impl Publisher {
    /// 建管线 → 置 Playing → 起采集。任何一步失败都把已建的部分收干净再返回。
    pub fn start(
        app: &AppHandle,
        trust: &Arc<Trust>,
        endpoint: &str,
        token: &str,
        source_id: &str,
        bitrate_kbps: u32,
        codec: &str,
    ) -> Result<Self, String> {
        init_gst()?;

        let testsrc = testsrc_mode();
        let fps = 30u32;
        // 测试源模式下没有真实采集源，尺寸取一个固定值；真采集先解析目标再按内容尺寸算。
        let prepared: Option<capture::Prepared> =
            if testsrc { None } else { Some(capture::prepare(source_id, fps)?) };
        let geom = match &prepared {
            Some(p) => p.geometry,
            None => capture::Geometry { width: 1280, height: 720 },
        };

        let (enc, parser) = match codec {
            "h264" => ("vtenc_h264", "h264parse"),
            _ => ("vtenc_h265", "h265parse"),
        };
        let video_head = if testsrc {
            format!(
                "videotestsrc is-live=true ! video/x-raw,width={w},height={h},framerate={fps}/1 ! videoconvert",
                w = geom.width,
                h = geom.height,
            )
        } else {
            // SCK 直出 NV12，videoconvert 在这条链上是直通，留着只为 caps 不匹配时兜底。
            "appsrc name=vsrc is-live=true format=time do-timestamp=true max-buffers=8 leaky-type=downstream ! videoconvert".to_string()
        };
        let audio_head = if testsrc {
            "audiotestsrc is-live=true".to_string()
        } else {
            "appsrc name=asrc is-live=true format=time do-timestamp=true max-buffers=32 leaky-type=downstream".to_string()
        };
        let desc = format!(
            "whipclientsink name=ws congestion-control=disabled \
             {video_head} ! {enc} realtime=true allow-frame-reordering=false max-keyframe-interval=60 bitrate={bitrate_kbps} ! \
             {parser} name=vparse config-interval=-1 ! ws.video_0 \
             {audio_head} ! audioconvert ! audioresample ! opusenc ! ws.audio_0"
        );

        let pipeline = gst::parse::launch(&desc)
            .map_err(|e| format!("管线构建失败：{e}"))?
            .downcast::<gst::Pipeline>()
            .map_err(|_| "管线构建失败：parse 结果不是 pipeline".to_string())?;

        // https 的推流一律经回环反代出去：whipclientsink 的 signaller 没有任何 CA 入口，
        // 自签服务器的信任锚只能由我们自己这一跳带上（见 whipproxy 模块注释）。
        let target = Url::parse(endpoint).map_err(|_| "推流地址格式不对".to_string())?;
        let proxy = if target.scheme() == "https" {
            Some(whipproxy::Proxy::start(&target, trust::client_for(trust, &target)?)?)
        } else {
            None
        };
        let signal_endpoint = match &proxy {
            Some(p) => p.endpoint.clone(),
            None => endpoint.to_string(),
        };

        // 地址与令牌走属性而不是拼进 launch 串：令牌里的引号/空白不该有机会改变管线结构。
        let ws = pipeline.by_name("ws").ok_or("管线里没有 whipclientsink")?;
        let signaller: gst::glib::Object = ws.property("signaller");
        signaller.set_property("whip-endpoint", &signal_endpoint);
        signaller.set_property("auth-token", token);

        let error: Arc<Mutex<Option<String>>> = Arc::new(Mutex::new(None));
        if let Some(bus) = pipeline.bus() {
            let slot = error.clone();
            // 同步处理器就地记错，不另起线程也不需要 glib 主循环。
            bus.set_sync_handler(move |_, msg| {
                if let gst::MessageView::Error(err) = msg.view() {
                    let mut slot = slot.lock().unwrap();
                    if slot.is_none() {
                        *slot = Some(format!("{}：{}", err.error(), err.debug().unwrap_or_default()));
                    }
                }
                gst::BusSyncReply::Drop
            });
        }

        // 已发送帧数从编码后的 parser 出口数，测试源与真采集同一处计。
        let frames = Arc::new(AtomicU64::new(0));
        if let Some(pad) = pipeline.by_name("vparse").and_then(|e| e.static_pad("src")) {
            let counter = frames.clone();
            pad.add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                counter.fetch_add(1, Ordering::Relaxed);
                gst::PadProbeReturn::Ok
            });
        }

        // ICE 失败时 whipclientsink 不会往 bus 上 post error，网页会一直显示「投屏中」。
        // 盯住 webrtcbin 的连接状态：坏了先记时间戳，连续坏满窗口才算失败（见看门狗）。
        let ice_bad: Arc<Mutex<Option<Instant>>> = Arc::new(Mutex::new(None));
        {
            let ice_bad = ice_bad.clone();
            ws.connect("consumer-added", false, move |vals| {
                let bin = vals.get(2).and_then(|v| v.get::<gst::Element>().ok())?;
                for prop in ["ice-connection-state", "connection-state"] {
                    let ice_bad = ice_bad.clone();
                    bin.connect_notify(Some(prop), move |bin, pspec| {
                        let bad = match pspec.name() {
                            "ice-connection-state" => matches!(
                                bin.property::<gst_webrtc::WebRTCICEConnectionState>(
                                    "ice-connection-state"
                                ),
                                gst_webrtc::WebRTCICEConnectionState::Failed
                                    | gst_webrtc::WebRTCICEConnectionState::Disconnected
                            ),
                            _ => matches!(
                                bin.property::<gst_webrtc::WebRTCPeerConnectionState>(
                                    "connection-state"
                                ),
                                gst_webrtc::WebRTCPeerConnectionState::Failed
                                    | gst_webrtc::WebRTCPeerConnectionState::Disconnected
                            ),
                        };
                        let mut slot = ice_bad.lock().unwrap();
                        match (bad, slot.is_none()) {
                            (true, true) => *slot = Some(Instant::now()),
                            (false, _) => *slot = None,
                            _ => {}
                        }
                    });
                }
                None
            });
        }

        let sink = if testsrc {
            None
        } else {
            let video = pipeline
                .by_name("vsrc")
                .and_then(|e| e.downcast::<gst_app::AppSrc>().ok())
                .ok_or("管线里没有视频 appsrc")?;
            let audio = pipeline
                .by_name("asrc")
                .and_then(|e| e.downcast::<gst_app::AppSrc>().ok())
                .ok_or("管线里没有音频 appsrc")?;
            video.set_caps(Some(
                &gst::Caps::builder("video/x-raw")
                    .field("format", "NV12")
                    .field("width", geom.width as i32)
                    .field("height", geom.height as i32)
                    .field("framerate", gst::Fraction::new(fps as i32, 1))
                    .build(),
            ));
            audio.set_caps(Some(
                &gst::Caps::builder("audio/x-raw")
                    .field("format", "F32LE")
                    .field("layout", "interleaved")
                    .field("rate", capture::AUDIO_RATE as i32)
                    .field("channels", capture::AUDIO_CHANNELS as i32)
                    .build(),
            ));
            Some(Sink { video, audio, error: error.clone() })
        };

        if let Err(e) = pipeline.set_state(gst::State::Playing) {
            if let Some(p) = proxy {
                p.stop();
            }
            return Err(format!("管线启动失败：{e}"));
        }

        let capture = match (prepared, sink) {
            (Some(prepared), Some(sink)) => match capture::start(prepared, sink) {
                Ok(c) => Some(c),
                Err(e) => {
                    let _ = pipeline.set_state(gst::State::Null);
                    if let Some(p) = proxy {
                        p.stop();
                    }
                    return Err(e);
                }
            },
            _ => None,
        };

        let stopped = Arc::new(AtomicBool::new(false));
        watchdog(app.clone(), stopped.clone(), error.clone(), frames.clone(), ice_bad);

        Ok(Self {
            pipeline,
            capture,
            proxy,
            frames,
            error,
            stopped,
            bitrate_kbps,
            codec: codec.to_string(),
        })
    }

    pub fn stats(&self) -> Stats {
        Stats {
            running: self.pipeline.current_state() == gst::State::Playing,
            frames: self.frames.load(Ordering::Relaxed),
            bitrate_kbps: self.bitrate_kbps,
            codec: self.codec.clone(),
            error: self.error.lock().unwrap().clone(),
        }
    }

    /// 停采集 → 发 EOS → 置 Null → 关回环反代。
    /// WHIP 的 DELETE 由 whipclientsink 在转 Null 时自己发，所以反代要最后关。
    pub fn stop(mut self) {
        self.stopped.store(true, Ordering::Relaxed);
        if let Some(capture) = self.capture.take() {
            capture.stop();
        }
        self.pipeline.send_event(gst::event::Eos::new());
        let _ = self.pipeline.set_state(gst::State::Null);
        if let Some(bus) = self.pipeline.bus() {
            bus.unset_sync_handler();
        }
        if let Some(proxy) = self.proxy.take() {
            proxy.stop();
        }
    }
}
