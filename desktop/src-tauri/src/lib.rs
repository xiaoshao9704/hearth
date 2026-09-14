// 桌面壳：窗口里装的就是 web/dist 那一份网页，观看、语音、聊天全归网页；
// 原生侧只做投屏发布，能力经下面这几个 IPC 命令暴露，没有第二套界面。
//
// CSP 现在是 null（不下发）：网页要连的是用户自己填的任意一台服务器，M1 阶段先不限制
// connect-src。等「应用内信任」把服务器地址收敛成一份配置后，这里应当收紧成
// 「只允许当前配置的那台服务器 + self」。
mod capture;
mod publish;

use std::sync::Mutex;

use serde::Serialize;
use tauri::{Manager, RunEvent, WindowEvent};

#[derive(Serialize)]
pub struct Capabilities {
    /// 能不能走原生投屏发布（网页据此把「投屏」入口切成原生流程）
    native_publish: bool,
    platform: &'static str,
    /// 投屏是否带所属应用的声音
    app_audio: bool,
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
}

#[tauri::command(async)]
fn capabilities() -> Capabilities {
    // 测试源模式下不碰 SCK，也就不看它可不可用——这条路子本来就是给无人值守验证用的
    let native_publish = publish::testsrc_mode() || capture::available();
    Capabilities {
        native_publish,
        platform: std::env::consts::OS,
        app_audio: cfg!(target_os = "macos") && !publish::testsrc_mode(),
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
        }]);
    }
    capture::list_sources()
}

#[tauri::command(async, rename_all = "snake_case")]
fn start_publish(
    state: tauri::State<'_, AppState>,
    endpoint: String,
    token: String,
    source_id: String,
    bitrate_kbps: u32,
    codec: String,
) -> Result<(), String> {
    // 参数在进管线之前先卡死：这几个值会变成管线属性与网络目标
    if !(endpoint.starts_with("http://") || endpoint.starts_with("https://")) {
        return Err("推流地址必须是 http/https".to_string());
    }
    if token.is_empty() || token.len() > 256 {
        return Err("推流令牌不合法".to_string());
    }
    if codec != "h264" && codec != "h265" {
        return Err("编码只支持 h264 / h265".to_string());
    }
    let bitrate_kbps = bitrate_kbps.clamp(500, 20000);

    let mut slot = state.publisher.lock().unwrap();
    if let Some(old) = slot.take() {
        old.stop(); // 同时只允许一路发布：再点一次就是换目标
    }
    *slot = Some(publish::Publisher::start(&endpoint, &token, &source_id, bitrate_kbps, &codec)?);
    Ok(())
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
    tauri::Builder::default()
        .manage(AppState::default())
        .invoke_handler(tauri::generate_handler![
            capabilities,
            list_sources,
            start_publish,
            stop_publish,
            publish_stats
        ])
        // 关窗与退出都要把在途发布收回来，否则 WHIP 会话会一直挂在服务端
        .on_window_event(|window, event| {
            if matches!(event, WindowEvent::Destroyed | WindowEvent::CloseRequested { .. }) {
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
