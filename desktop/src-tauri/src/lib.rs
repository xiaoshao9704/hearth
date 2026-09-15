// 桌面壳：窗口里装的就是 web/dist 那一份网页，观看、语音、聊天全归网页；
// 原生侧只做投屏发布，能力经下面这几个 IPC 命令暴露，没有第二套界面。
//
// CSP 现在是 null（不下发）：网页要连的是用户自己填的任意一台服务器，M1 阶段先不限制
// connect-src。等「应用内信任」把服务器地址收敛成一份配置后，这里应当收紧成
// 「只允许当前配置的那台服务器 + self」。
mod capture;
mod encoder;
mod preview;
mod publish;
mod trust;
mod whipproxy;

use std::sync::{atomic::AtomicBool, Arc, Mutex, OnceLock};

use serde::Serialize;
use tauri::{Emitter, Manager, RunEvent, WindowEvent};

/// 深链事件载荷：网页只认这一个字段（见 web/src/bridge 的 onDeepLink）
#[derive(Clone, Serialize)]
struct DeepLink {
    url: String,
}

#[derive(Serialize)]
pub struct Capabilities {
    /// 能不能走原生投屏发布（网页据此把「投屏」入口切成原生流程）
    native_publish: bool,
    platform: &'static str,
    /// 投屏是否带所属应用的声音
    app_audio: bool,
    native_publish_error: Option<String>,
    publish_codecs: Vec<String>,
}

#[derive(Default)]
struct AppState {
    publisher: Mutex<Option<publish::Publisher>>,
}

impl AppState {
    fn stop(&self) {
        if let Some(p) = self.publisher.lock().unwrap().take() {
            p.stop();
        }
    }

    fn stop_if_current(&self, app: &tauri::AppHandle, marker: &Arc<AtomicBool>, msg: String) {
        let mut slot = self.publisher.lock().unwrap();
        if slot.as_ref().is_some_and(|p| p.is_current(marker)) {
            slot.take().unwrap().stop();
            let _ = app.emit(
                "publish-state",
                publish::PublishState {
                    running: false,
                    error: Some(msg),
                },
            );
        }
    }

    fn refresh_geometry(&self, app: &tauri::AppHandle, marker: &Arc<AtomicBool>) {
        if publish::testsrc_mode() {
            return;
        }
        // 系统枚举不占发布锁；拿到结果后再验证实例身份，期间 stop/update 不会被旧结果覆盖。
        let Some((request, geometry)) = self
            .publisher
            .lock()
            .unwrap()
            .as_ref()
            .filter(|p| p.is_current(marker))
            .map(|p| (p.request.clone(), p.geometry))
        else {
            return;
        };
        let result = capture::geometry(&request.source_id, request.settings);
        let mut slot = self.publisher.lock().unwrap();
        if !slot
            .as_ref()
            .is_some_and(|p| p.is_current(marker) && p.request.settings == request.settings)
        {
            return;
        }
        let error = match result {
            Ok(current) if cfg!(target_os = "windows") || current == geometry => return,
            Ok(_) => match slot.as_mut().unwrap().update(request.settings) {
                Ok(()) => return,
                Err(e) => {
                    slot.take().unwrap().stop();
                    e
                }
            },
            Err(e) => {
                slot.take().unwrap().stop();
                e
            }
        };
        let _ = app.emit(
            "publish-state",
            publish::PublishState {
                running: false,
                error: Some(error),
            },
        );
    }
}

#[tauri::command(async)]
fn capabilities() -> Capabilities {
    static PROBE: OnceLock<Result<Vec<String>, String>> = OnceLock::new();
    let result = PROBE.get_or_init(|| {
        publish::init_gst()?;
        if !publish::testsrc_mode() && !capture::available() {
            return Err("原生屏幕采集接口或插件不可用".into());
        }
        for name in [
            "whipclientsink",
            "h264parse",
            "videoconvert",
            "videoscale",
            "appsrc",
            "appsink",
            "jpegenc",
        ] {
            if gstreamer::ElementFactory::find(name).is_none() {
                return Err(format!("缺少 GStreamer 插件：{name}"));
            }
        }
        let mut codecs = Vec::new();
        let mut errors = Vec::new();
        for codec in ["h264", "h265"] {
            match encoder::select(
                codec,
                capture::Geometry {
                    width: 640,
                    height: 360,
                },
                capture::Settings {
                    width: 640,
                    height: 360,
                    fps: 30,
                    bitrate_kbps: 2000,
                },
                false,
            ) {
                Ok(_) => codecs.push(codec.to_owned()),
                Err(e) => errors.push(e),
            }
        }
        if codecs.is_empty() {
            return Err(errors.join("；"));
        }
        Ok(codecs)
    });
    Capabilities {
        native_publish: result.is_ok(),
        platform: std::env::consts::OS,
        app_audio: capture::audio_available() && !publish::testsrc_mode(),
        native_publish_error: result.as_ref().err().cloned(),
        publish_codecs: result.as_ref().cloned().unwrap_or_default(),
    }
}

#[tauri::command(async)]
fn list_sources() -> Result<Vec<capture::Source>, String> {
    if publish::testsrc_mode() {
        return Ok(vec![capture::Source {
            id: "testsrc:1".to_string(),
            kind: "display".to_string(),
            title: "测试源（彩条 + 正弦音）".to_string(),
            app: String::new(),
            audio_scope: capture::AudioScope::None,
        }]);
    }
    capture::list_sources()
}

#[tauri::command(async, rename_all = "snake_case")]
fn start_publish(
    app: tauri::AppHandle,
    state: tauri::State<'_, AppState>,
    trust: tauri::State<'_, std::sync::Arc<trust::Trust>>,
    endpoint: String,
    token: String,
    source_id: String,
    bitrate_kbps: u32,
    codec: String,
    width: u32,
    height: u32,
    fps: u32,
    audio: bool,
) -> Result<StartedPublish, String> {
    // 参数在进管线之前先卡死：这几个值会变成管线属性与网络目标
    if !(endpoint.starts_with("http://") || endpoint.starts_with("https://")) {
        return Err("推流地址必须是 http/https".to_string());
    }
    if token.is_empty() || token.len() > 256 {
        return Err("推流令牌不合法".to_string());
    }
    if !["h264", "h265", "vp9", "av1"].contains(&codec.as_str()) {
        return Err("未知的视频编码偏好".into());
    }
    let settings = capture::Settings {
        width,
        height,
        fps,
        bitrate_kbps,
    }
    .validate()?;

    let mut slot = state.publisher.lock().unwrap();
    if let Some(old) = slot.take() {
        old.stop(); // 同时只允许一路发布：再点一次就是换目标
    }
    let publisher = publish::Publisher::start(
        &app,
        &trust,
        publish::Request {
            endpoint,
            token,
            source_id,
            settings,
            audio,
            codec,
        },
        true,
    )?;
    let result = StartedPublish {
        codec: publisher.request.codec.clone(),
    };
    *slot = Some(publisher);
    Ok(result)
}

#[derive(Serialize)]
struct StartedPublish {
    codec: String,
}

#[tauri::command(async, rename_all = "snake_case")]
fn update_publish(
    app: tauri::AppHandle,
    state: tauri::State<'_, AppState>,
    width: u32,
    height: u32,
    fps: u32,
    bitrate_kbps: u32,
) -> Result<(), String> {
    let settings = capture::Settings {
        width,
        height,
        fps,
        bitrate_kbps,
    }
    .validate()?;
    let mut slot = state.publisher.lock().unwrap();
    let old = slot.as_ref().ok_or("当前没有原生投屏")?;
    if old.request.settings == settings {
        return Ok(());
    }
    match slot.as_mut().unwrap().update(settings) {
        Ok(()) => Ok(()),
        Err(e) => {
            slot.take().unwrap().stop();
            let e = format!("画质更新失败，请重新开始投屏获取新设备票：{e}");
            let _ = app.emit(
                "publish-state",
                publish::PublishState {
                    running: false,
                    error: Some(e.clone()),
                },
            );
            Err(e)
        }
    }
}

#[tauri::command(async, rename_all = "snake_case")]
fn source_preview(source_id: String) -> Result<Option<String>, String> {
    preview::one_frame(&source_id)
}

#[tauri::command(async)]
fn stop_publish(state: tauri::State<'_, AppState>) {
    state.stop();
}

#[tauri::command(async)]
fn publish_stats(state: tauri::State<'_, AppState>) -> Option<publish::Stats> {
    state.publisher.lock().unwrap().as_ref().map(|p| p.stats())
}

pub fn run() {
    // 启动即初始化 GStreamer：运行时缺插件要第一时间暴露，不拖到用户点投屏才报
    if let Err(e) = publish::init_gst() {
        eprintln!("{e}");
    }
    tauri::Builder::default()
        .plugin(tauri_plugin_opener::init())
        // hearth:// 深链：浏览器跳转登录完成后由系统唤回本应用，URL 里只有一次性码
        .plugin(tauri_plugin_deep_link::init())
        .manage(AppState::default())
        .invoke_handler(tauri::generate_handler![
            capabilities,
            list_sources,
            start_publish,
            update_publish,
            source_preview,
            stop_publish,
            publish_stats,
            trust::check_server,
            trust::pair_server,
            trust::forget_server
        ])
        .setup(|app| {
            // 深链只往 main 窗口透传，且只认 hearth://：系统 URL 分发是公共通道，
            // 别的 scheme 一律不是给我们的。启动时带的 URL 也走这里（插件把
            // RunEvent::Opened 转成同一个事件），但那种情况下网页可能还没挂上监听——
            // 换会话本来就要壳内这次会话里的 verifier，冷启动重来一遍才是正确行为。
            {
                use tauri_plugin_deep_link::DeepLinkExt;
                let handle = app.handle().clone();
                app.deep_link().on_open_url(move |event| {
                    for url in event.urls() {
                        let url = url.to_string();
                        if url.starts_with("hearth://") {
                            let _ = handle.emit_to("main", "deep-link", DeepLink { url });
                        }
                    }
                });
            }
            // 信任配置放 app 配置目录；WebView 的证书回调与 Rust 侧出站 https 共用这一份
            let dir = app
                .path()
                .app_config_dir()
                .map_err(|e| format!("取配置目录失败：{e}"))?;
            let trust = trust::Trust::load(dir);
            app.manage(trust.clone());
            #[cfg(target_os = "macos")]
            if let Some(win) = app.get_webview_window("main") {
                // with_webview 的闭包在主线程上跑：WebView 只能在事件循环所在线程上碰
                let trust = trust.clone();
                win.with_webview(move |wv| unsafe { trust::macos::install(wv.inner(), trust) })?;
            }
            Ok(())
        })
        // 关窗与退出都要把在途发布收回来，否则 WHIP 会话会一直挂在服务端
        .on_window_event(|window, event| {
            if matches!(
                event,
                WindowEvent::Destroyed | WindowEvent::CloseRequested { .. }
            ) {
                window.state::<AppState>().stop();
            }
        })
        .build(tauri::generate_context!())
        .expect("桌面端启动失败")
        .run(|app, event| {
            if matches!(event, RunEvent::ExitRequested { .. } | RunEvent::Exit) {
                app.state::<AppState>().stop();
            }
        });
}
