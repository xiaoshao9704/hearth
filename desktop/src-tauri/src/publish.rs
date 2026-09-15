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
use tauri::{AppHandle, Manager};

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
    pub audio: Option<gst_app::AppSrc>,
    active: Arc<AtomicBool>,
    error: Arc<Mutex<Option<String>>>,
}

impl Sink {
    pub fn from_pipeline(
        pipeline: &gst::Pipeline,
        geom: capture::Geometry,
        fps: u32,
        error: Arc<Mutex<Option<String>>>,
    ) -> Result<Self, String> {
        let video = pipeline
            .by_name("vsrc")
            .and_then(|e| e.downcast::<gst_app::AppSrc>().ok())
            .ok_or("管线里没有视频 appsrc")?;
        let audio = pipeline
            .by_name("asrc")
            .and_then(|e| e.downcast::<gst_app::AppSrc>().ok());
        video.set_caps(Some(
            &gst::Caps::builder("video/x-raw")
                .field("format", "NV12")
                .field("width", geom.width as i32)
                .field("height", geom.height as i32)
                .field("framerate", gst::Fraction::new(fps as i32, 1))
                .build(),
        ));
        if let Some(audio) = &audio {
            audio.set_caps(Some(
                &gst::Caps::builder("audio/x-raw")
                    .field("format", "F32LE")
                    .field("layout", "interleaved")
                    .field("rate", capture::AUDIO_RATE as i32)
                    .field("channels", capture::AUDIO_CHANNELS as i32)
                    .build(),
            ));
        }
        Ok(Self {
            video,
            audio,
            error,
            active: Arc::new(AtomicBool::new(true)),
        })
    }

    /// 只记第一条错误：后续同因错误会连成串，头一条才有诊断价值。
    pub fn deactivate(&self) {
        self.active.store(false, Ordering::Relaxed);
    }
    pub fn active(&self) -> bool {
        self.active.load(Ordering::Relaxed)
    }

    pub fn fail(&self, msg: impl Into<String>) {
        if !self.active() {
            return;
        }
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

/// 装到 .app 里时把 GStreamer 指向 bundle 自带的那一份（见 scripts/bundle-gst-macos.sh）。
/// 开发态（可执行文件旁没有 Resources/gstreamer-1.0）什么都不做，照旧用系统安装的 GStreamer。
/// 三个环境变量都只在外部没设时才写：排障时能从命令行覆盖。
#[cfg(target_os = "macos")]
fn use_bundled_gst() {
    use std::path::PathBuf;

    let plugins = match std::env::current_exe().ok().and_then(|exe| {
        // …/Hearth.app/Contents/MacOS/hearth-desktop → …/Contents/Resources/gstreamer-1.0
        let dir = exe.parent()?.parent()?.join("Resources/gstreamer-1.0");
        dir.is_dir().then_some(dir)
    }) {
        Some(p) => p,
        None => return,
    };
    let set = |k: &str, v: &std::ffi::OsStr| {
        if std::env::var_os(k).is_none() {
            std::env::set_var(k, v);
        }
    };
    // SYSTEM_PATH 是替换而不是追加：设了它就不会再去扫编译期写死的 Homebrew 目录，
    // 「装到没有 Homebrew 的机器上」与「本机有 Homebrew 但不许用」因此是同一条路径。
    set("GST_PLUGIN_SYSTEM_PATH_1_0", plugins.as_os_str());
    // 不带 gst-plugin-scanner：多一个可执行文件就多一份签名与 hardened runtime 的面，
    // 而插件是我们自己挑的那十几个，进程内扫描崩不着。
    set("GST_REGISTRY_FORK", std::ffi::OsStr::new("no"));
    // 注册表要可写，bundle 内不可写：放用户缓存目录（标识与 tauri.conf.json 的 identifier 一致）
    if let Some(home) = std::env::var_os("HOME") {
        let dir = PathBuf::from(home).join("Library/Caches/app.hearth.desktop");
        if std::fs::create_dir_all(&dir).is_ok() {
            set(
                "GST_REGISTRY",
                dir.join("gstreamer-registry.bin").as_os_str(),
            );
        }
    }
}

#[cfg(target_os = "windows")]
fn use_bundled_gst_windows() {
    let Some(exe_dir) = std::env::current_exe()
        .ok()
        .and_then(|p| p.parent().map(|p| p.to_owned()))
    else {
        return;
    };
    let runtime = exe_dir.join("gstreamer");
    if !runtime.is_dir() {
        return;
    }
    let set = |key: &str, value: &std::ffi::OsStr| {
        if std::env::var_os(key).is_none() {
            std::env::set_var(key, value);
        }
    };
    set(
        "GST_PLUGIN_SYSTEM_PATH_1_0",
        runtime.join("lib/gstreamer-1.0").as_os_str(),
    );
    set(
        "GST_PLUGIN_SCANNER_1_0",
        runtime
            .join("libexec/gstreamer-1.0/gst-plugin-scanner.exe")
            .as_os_str(),
    );
    let mut paths = vec![runtime.join("bin")];
    if let Some(path) = std::env::var_os("PATH") {
        paths.extend(std::env::split_paths(&path));
    }
    if let Ok(path) = std::env::join_paths(paths) {
        std::env::set_var("PATH", path);
    }
    if let Some(cache) = std::env::var_os("LOCALAPPDATA") {
        let cache = std::path::PathBuf::from(cache).join("app.hearth.desktop/Cache");
        if std::fs::create_dir_all(&cache).is_ok() {
            set(
                "GST_REGISTRY",
                cache.join("gstreamer-registry.bin").as_os_str(),
            );
        }
    }
}

pub fn init_gst() -> Result<(), String> {
    GST_INIT
        .get_or_init(|| {
            #[cfg(target_os = "macos")]
            use_bundled_gst();
            #[cfg(target_os = "windows")]
            use_bundled_gst_windows();
            gst::init().map_err(|e| format!("GStreamer 初始化失败：{e}"))
        })
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
    pub request: Request,
    pub geometry: capture::Geometry,
}

#[derive(Clone)]
pub struct Request {
    pub endpoint: String,
    pub token: String,
    pub source_id: String,
    pub settings: capture::Settings,
    pub audio: bool,
    pub codec: String,
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
    let mut last_geometry = Instant::now();
    std::thread::spawn(move || loop {
        std::thread::sleep(WATCH_TICK);
        if stopped.load(Ordering::Relaxed) {
            return;
        }
        if last_geometry.elapsed() >= Duration::from_secs(2) {
            last_geometry = Instant::now();
            app.state::<crate::AppState>()
                .refresh_geometry(&app, &stopped);
            if stopped.load(Ordering::Relaxed) {
                return;
            }
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
        // 在同一把锁下比对发布身份、停流并发事件，旧看门狗不能影响新流。
        app.state::<crate::AppState>()
            .stop_if_current(&app, &stopped, msg);
        return;
    });
}

impl Publisher {
    /// 建管线 → 置 Playing → 起采集。任何一步失败都把已建的部分收干净再返回。
    pub fn start(
        app: &AppHandle,
        trust: &Arc<Trust>,
        request: Request,
        fallback: bool,
    ) -> Result<Self, String> {
        init_gst()?;
        let settings = request.settings.validate()?;
        let fps = settings.fps;
        let bitrate_kbps = settings.bitrate_kbps;
        let endpoint = &request.endpoint;
        let token = &request.token;
        let testsrc = testsrc_mode();
        let prepared = if testsrc {
            None
        } else {
            Some(capture::prepare(
                &request.source_id,
                settings,
                request.audio,
            )?)
        };
        let geom = match &prepared {
            Some(p) => p.geometry,
            None => capture::Geometry::fit(1280., 720., settings)?,
        };
        let encoder = crate::encoder::select(&request.codec, geom, settings, fallback)?;
        let codec = encoder.codec;
        let parser = if codec == "h264" {
            "h264parse"
        } else {
            "h265parse"
        };
        let video_head = match &prepared {
            Some(p) => p.video_head(),
            None => format!("videotestsrc is-live=true do-timestamp=true ! video/x-raw,format=NV12,width={},height={},framerate={fps}/1", geom.width,geom.height),
        };
        let audio_branch = if request.audio {
            let head = match &prepared {
                Some(p) => p.audio_head()?,
                None => "audiotestsrc is-live=true".into(),
            };
            format!("{head} ! audioconvert ! audioresample ! opusenc ! ws.audio_0")
        } else {
            String::new()
        };
        let video = gst::parse::bin_from_description_with_name(
            &format!(
                "{video_head} ! {} ! {parser} name=vparse config-interval=-1",
                encoder.launch
            ),
            true,
            "videochain",
        )
        .map_err(|e| format!("视频支路构建失败：{e}"))?;
        let desc = format!("pipeline name=publisher whipclientsink name=ws congestion-control=disabled {audio_branch}");

        let pipeline = gst::parse::launch(&desc)
            .map_err(|e| format!("管线构建失败：{e}"))?
            .downcast::<gst::Pipeline>()
            .map_err(|_| "管线构建失败：parse 结果不是 pipeline".to_string())?;

        // https 的推流一律经回环反代出去：whipclientsink 的 signaller 没有任何 CA 入口，
        // 自签服务器的信任锚只能由我们自己这一跳带上（见 whipproxy 模块注释）。
        let target = Url::parse(endpoint).map_err(|_| "推流地址格式不对".to_string())?;
        let proxy = if target.scheme() == "https" {
            Some(whipproxy::Proxy::start(
                &target,
                trust::client_for(trust, &target)?,
            )?)
        } else {
            None
        };
        let signal_endpoint = match &proxy {
            Some(p) => p.endpoint.clone(),
            None => endpoint.to_string(),
        };

        // 地址与令牌走属性而不是拼进 launch 串：令牌里的引号/空白不该有机会改变管线结构。
        let ws = pipeline.by_name("ws").ok_or("管线里没有 whipclientsink")?;
        pipeline
            .add(&video)
            .map_err(|e| format!("挂视频支路失败：{e}"))?;
        let video_pad = ws
            .request_pad_simple("video_%u")
            .ok_or("WHIP 缺少视频入口")?;
        video
            .static_pad("src")
            .ok_or("视频支路缺少出口")?
            .link(&video_pad)
            .map_err(|e| format!("连接视频支路失败：{e}"))?;
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
                        *slot = Some(format!(
                            "{}：{}",
                            err.error(),
                            err.debug().unwrap_or_default()
                        ));
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

        // attach 先挂好 appsrc/caps 或复验 HWND，随后启动管线；所有失败路径由 guard 收回资源。
        let mut publisher = Self {
            pipeline,
            capture: None,
            proxy,
            frames,
            error,
            stopped: Arc::new(AtomicBool::new(false)),
            bitrate_kbps,
            codec: codec.to_string(),
            request: Request {
                codec: codec.to_string(),
                ..request
            },
            geometry: geom,
        };
        // SCK appsrc 是 live source，可先起采集，队列有上限；Windows attach 不起额外采集。
        if let Some(prepared) = prepared {
            publisher.capture =
                Some(prepared.attach(&publisher.pipeline, settings, publisher.error.clone())?);
        }
        publisher
            .pipeline
            .set_state(gst::State::Playing)
            .map_err(|e| format!("管线启动失败：{e}"))?;
        watchdog(
            app.clone(),
            publisher.stopped.clone(),
            publisher.error.clone(),
            publisher.frames.clone(),
            ice_bad,
        );
        Ok(publisher)
    }

    /// 只替换视频支路，保留 whipclientsink、请求 pad、音频支路和信令对象。
    /// 这里绝不能调用 start 或重新 POST WHIP：设备票可能已过期。
    pub fn update(&mut self, settings: capture::Settings) -> Result<(), String> {
        let settings = settings.validate()?;
        let prepared = if testsrc_mode() {
            None
        } else {
            Some(capture::prepare(
                &self.request.source_id,
                settings,
                self.request.audio,
            )?)
        };
        let geom = match &prepared {
            Some(p) => p.geometry,
            None => capture::Geometry::fit(1280., 720., settings)?,
        };
        let encoder = crate::encoder::select(&self.codec, geom, settings, false)?;
        let head = match &prepared {
            Some(p) => p.video_head(),
            None => format!(
                "videotestsrc is-live=true do-timestamp=true ! video/x-raw,format=NV12,width={},height={},framerate={}/1",
                geom.width, geom.height, settings.fps
            ),
        };
        let parser = if self.codec == "h264" {
            "h264parse"
        } else {
            "h265parse"
        };
        let replacement = gst::parse::bin_from_description_with_name(
            &format!(
                "{head} ! {} ! {parser} name=vparse config-interval=-1",
                encoder.launch
            ),
            true,
            "videochain",
        )
        .map_err(|e| format!("更新视频支路失败：{e}"))?;
        let old = self
            .pipeline
            .by_name("videochain")
            .ok_or("旧视频支路已失效")?;
        let src = old.static_pad("src").ok_or("旧视频出口已失效")?;
        let peer = src.peer().ok_or("WHIP 视频入口已失效")?;
        // 下游先阻断，待 streaming thread 空闲后再拆链；不向 WHIP 发送 EOS/Flush。
        let (tx, rx) = std::sync::mpsc::sync_channel(1);
        let probe = src.add_probe(gst::PadProbeType::IDLE, move |_, _| {
            let _ = tx.try_send(());
            gst::PadProbeReturn::Ok
        });
        if rx.recv_timeout(Duration::from_secs(3)).is_err() {
            if let Some(probe) = probe {
                src.remove_probe(probe);
            }
            return Err("视频支路未能安全暂停，请重新开始投屏".into());
        }
        if let Some(capture) = self.capture.take() {
            capture.stop();
        }
        let unlink = src.unlink(&peer);
        if let Some(probe) = probe {
            src.remove_probe(probe);
        }
        unlink.map_err(|e| format!("断开旧视频支路失败：{e}"))?;
        old.set_state(gst::State::Null)
            .map_err(|e| format!("停止旧视频支路失败：{e}"))?;
        self.pipeline
            .remove(&old)
            .map_err(|e| format!("移除旧视频支路失败：{e}"))?;
        self.pipeline
            .add(&replacement)
            .map_err(|e| format!("挂新视频支路失败：{e}"))?;
        replacement
            .static_pad("src")
            .ok_or("新视频支路缺少出口")?
            .link(&peer)
            .map_err(|e| format!("重接视频支路失败：{e}"))?;
        let counter = self.frames.clone();
        replacement
            .by_name("vparse")
            .and_then(|e| e.static_pad("src"))
            .ok_or("缺少编码输出")?
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                counter.fetch_add(1, Ordering::Relaxed);
                gst::PadProbeReturn::Ok
            });
        let before = self.frames.load(Ordering::Relaxed);
        // 同步父管线的时钟与 base_time；新 appsrc 的时间戳沿用在途会话的时间轴。
        replacement
            .sync_state_with_parent()
            .map_err(|e| format!("新视频支路启动失败：{e}"))?;
        if let Some(prepared) = prepared {
            self.capture = Some(prepared.attach(&self.pipeline, settings, self.error.clone())?);
        }
        let deadline = Instant::now();
        while self.frames.load(Ordering::Relaxed) == before {
            if let Some(error) = self.error.lock().unwrap().clone() {
                return Err(error);
            }
            if deadline.elapsed() > Duration::from_secs(3) {
                return Err("更新后没有编码画面，请重新开始投屏".into());
            }
            std::thread::sleep(Duration::from_millis(20));
        }
        self.request.settings = settings;
        self.geometry = geom;
        self.bitrate_kbps = settings.bitrate_kbps;
        Ok(())
    }

    pub fn is_current(&self, marker: &Arc<AtomicBool>) -> bool {
        Arc::ptr_eq(&self.stopped, marker) && !self.stopped.load(Ordering::Relaxed)
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

    /// 停采集 → 置 Null → 关回环反代。
    /// WHIP 的 DELETE 由 whipclientsink 在转 Null 时自己发，所以反代要最后关。
    pub fn stop(self) {
        drop(self);
    }
}

impl Drop for Publisher {
    fn drop(&mut self) {
        self.stopped.store(true, Ordering::Relaxed);
        if let Some(capture) = self.capture.take() {
            capture.stop();
        }
        let _ = self.pipeline.set_state(gst::State::Null);
        if let Some(bus) = self.pipeline.bus() {
            bus.unset_sync_handler();
        }
        if let Some(proxy) = self.proxy.take() {
            proxy.stop();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 需要桌面会话/GStreamer 硬编。只用彩条，不采集真实屏幕或音频，也不联网。
    #[test]
    #[ignore = "需要可用的 GStreamer 硬件编码运行时"]
    fn runtime_update_keeps_sink_and_audio_and_continues_frames() {
        std::env::set_var("HEARTH_DESKTOP_TESTSRC", "1");
        init_gst().unwrap();
        let settings = capture::Settings {
            width: 640,
            height: 360,
            fps: 30,
            bitrate_kbps: 2000,
        };
        let geometry = capture::Geometry {
            width: 640,
            height: 360,
        };
        let encoder = crate::encoder::select("h264", geometry, settings, false).unwrap();
        let pipeline = gst::parse::launch(concat!(
            "pipeline name=test fakesink name=ws sync=false ",
            "audiotestsrc is-live=true name=asrc ! fakesink name=audio_sink sync=false"
        ))
        .unwrap()
        .downcast::<gst::Pipeline>()
        .unwrap();
        let video = gst::parse::bin_from_description_with_name(
            &format!(
                concat!(
                    "videotestsrc is-live=true do-timestamp=true ! ",
                    "video/x-raw,format=NV12,width=640,height=360,framerate=30/1 ! ",
                    "{} ! h264parse name=vparse config-interval=-1"
                ),
                encoder.launch
            ),
            true,
            "videochain",
        )
        .unwrap();
        let ws = pipeline.by_name("ws").unwrap();
        let audio = pipeline.by_name("asrc").unwrap();
        pipeline.add(&video).unwrap();
        video.link(&ws).unwrap();
        let frames = Arc::new(AtomicU64::new(0));
        let counter = frames.clone();
        video
            .by_name("vparse")
            .unwrap()
            .static_pad("src")
            .unwrap()
            .add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                counter.fetch_add(1, Ordering::Relaxed);
                gst::PadProbeReturn::Ok
            });
        let timestamps = Arc::new(Mutex::new(Vec::new()));
        let pts = timestamps.clone();
        let peer = ws.static_pad("sink").unwrap();
        let segment = Mutex::new(gst::FormattedSegment::<gst::ClockTime>::new());
        peer.add_probe(
            gst::PadProbeType::BUFFER | gst::PadProbeType::EVENT_DOWNSTREAM,
            move |_, info| {
                if let Some(event) = info.event() {
                    if let gst::EventView::Segment(e) = event.view() {
                        if let Some(s) = e.segment().downcast_ref::<gst::ClockTime>() {
                            *segment.lock().unwrap() = s.clone();
                        }
                    }
                }
                if let Some(t) = info
                    .buffer()
                    .and_then(|b| b.pts())
                    .and_then(|t| segment.lock().unwrap().to_running_time(t))
                {
                    pts.lock().unwrap().push(t);
                }
                gst::PadProbeReturn::Ok
            },
        );
        let error = Arc::new(Mutex::new(None));
        let slot = error.clone();
        pipeline.bus().unwrap().set_sync_handler(move |_, msg| {
            if let gst::MessageView::Error(e) = msg.view() {
                *slot.lock().unwrap() = Some(e.error().to_string());
            }
            gst::BusSyncReply::Drop
        });
        let mut publisher = Publisher {
            pipeline,
            capture: None,
            proxy: None,
            frames,
            error,
            stopped: Arc::new(AtomicBool::new(false)),
            bitrate_kbps: 2000,
            codec: "h264".into(),
            geometry,
            request: Request {
                endpoint: "expired-device-ticket-must-not-be-used".into(),
                token: "expired".into(),
                source_id: "testsrc:1".into(),
                settings,
                audio: true,
                codec: "h264".into(),
            },
        };
        publisher.pipeline.set_state(gst::State::Playing).unwrap();
        let deadline = Instant::now();
        while publisher.frames.load(Ordering::Relaxed) < 5
            && deadline.elapsed() < Duration::from_secs(5)
        {
            std::thread::sleep(Duration::from_millis(20));
        }
        assert!(
            publisher.frames.load(Ordering::Relaxed) >= 5,
            "{:?}",
            publisher.error.lock().unwrap()
        );
        let marker = publisher.stopped.clone();
        for settings in [
            capture::Settings {
                width: 960,
                height: 540,
                fps: 60,
                bitrate_kbps: 4000,
            },
            capture::Settings {
                width: 480,
                height: 270,
                fps: 15,
                bitrate_kbps: 1000,
            },
        ] {
            let before = publisher.frames.load(Ordering::Relaxed);
            publisher.update(settings).unwrap();
            let deadline = Instant::now();
            while publisher.frames.load(Ordering::Relaxed) < before + 5
                && deadline.elapsed() < Duration::from_secs(5)
            {
                std::thread::sleep(Duration::from_millis(20));
            }
            assert!(
                publisher.frames.load(Ordering::Relaxed) >= before + 5,
                "{:?}",
                publisher.error.lock().unwrap()
            );
            assert_eq!(publisher.pipeline.by_name("ws").unwrap(), ws);
            assert_eq!(publisher.pipeline.by_name("asrc").unwrap(), audio);
            assert_eq!(
                publisher
                    .pipeline
                    .by_name("videochain")
                    .unwrap()
                    .static_pad("src")
                    .unwrap()
                    .peer()
                    .unwrap(),
                peer
            );
            assert_eq!(ws.current_state(), gst::State::Playing);
            assert!(publisher.is_current(&marker));
            assert!(!publisher.is_current(&Arc::new(AtomicBool::new(false))));
            assert!(
                publisher.error.lock().unwrap().is_none(),
                "{:?}",
                publisher.error.lock().unwrap()
            );
        }
        let pts = timestamps.lock().unwrap();
        assert!(
            pts.windows(2).all(|p| p[1] >= p[0]),
            "更新不得重置时间轴: {:?}",
            *pts
        );
        drop(pts);
        drop(publisher);
        let image = crate::preview::one_frame("testsrc:1").unwrap().unwrap();
        assert!(image.starts_with("data:image/jpeg;base64,"));
        assert!(image.len() < 180_000);
    }
}
