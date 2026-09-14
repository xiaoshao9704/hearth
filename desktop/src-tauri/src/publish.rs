// WHIP 发布管线：采集 → appsrc → VideoToolbox 硬编 → whipclientsink。
//
// 两条定死的取舍（来自 docs/plan-desktop.md 的 PoC）：
//   - congestion-control=disabled 走固定码率。webrtcsink 的带宽估计驱动不了 vtenc_*，
//     开着只会让码率估算空转；OBS 走的也是固定码率。
//   - vtenc_* 必须显式 max-keyframe-interval，否则 WHIP 会话建起来后观众永远等不到关键帧。
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};

use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_app as gst_app;
use serde::Serialize;

use crate::capture;

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
    frames: Arc<AtomicU64>,
    error: Arc<Mutex<Option<String>>>,
    bitrate_kbps: u32,
    codec: String,
}

impl Publisher {
    /// 建管线 → 置 Playing → 起采集。任何一步失败都把已建的部分收干净再返回。
    pub fn start(
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

        // 地址与令牌走属性而不是拼进 launch 串：令牌里的引号/空白不该有机会改变管线结构。
        let ws = pipeline.by_name("ws").ok_or("管线里没有 whipclientsink")?;
        let signaller: gst::glib::Object = ws.property("signaller");
        signaller.set_property("whip-endpoint", endpoint);
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

        pipeline
            .set_state(gst::State::Playing)
            .map_err(|e| format!("管线启动失败：{e}"))?;

        let capture = match (prepared, sink) {
            (Some(prepared), Some(sink)) => match capture::start(prepared, sink) {
                Ok(c) => Some(c),
                Err(e) => {
                    let _ = pipeline.set_state(gst::State::Null);
                    return Err(e);
                }
            },
            _ => None,
        };

        Ok(Self {
            pipeline,
            capture,
            frames,
            error,
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

    /// 停采集 → 发 EOS → 置 Null。WHIP 的 DELETE 由 whipclientsink 在转 Null 时自己发。
    pub fn stop(mut self) {
        if let Some(capture) = self.capture.take() {
            capture.stop();
        }
        self.pipeline.send_event(gst::event::Eos::new());
        let _ = self.pipeline.set_state(gst::State::Null);
        if let Some(bus) = self.pipeline.bus() {
            bus.unset_sync_handler();
        }
    }
}
