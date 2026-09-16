// 壳自己列出本机可采集的应用/窗口：网页选中一项，直接经 obs-websocket 写进 OBS 的源，
// 不再弹 OBS 的属性窗口。为什么不让 OBS 自己列：obs-websocket 5.7.3 的
// GetInputPropertiesListPropertyItems 碰到空名列表项会 strlen(NULL)，整个 OBS 段错误
// （Obs_ArrayHelper.cpp），任何平台都不能调。
//
// 这条路只用系统枚举 API，不碰 GStreamer，所以默认编入薄壳（不在 native-capture 里）。
use serde::Serialize;

#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "windows")]
pub mod windows;

/// 一个能写进 OBS 源设置的采集目标。
/// value 原样交给 OBS：macOS 应用是 bundle id（串）、窗口是 CGWindowID（数字），
/// Windows 是 OBS 的 window 串「标题:类名:可执行名」。
#[derive(Serialize)]
pub struct ObsTarget {
    /// app | window
    pub kind: &'static str,
    pub label: String,
    pub value: serde_json::Value,
}

/// 这个平台能不能由壳来列（capabilities 的 obs_targets）
pub const fn available() -> bool {
    cfg!(any(target_os = "macos", target_os = "windows"))
}

// 同步命令：AppKit 的 NSWorkspace 在主线程上调最稳妥，枚举本身是亚毫秒级的。
#[tauri::command]
pub fn list_obs_targets() -> Vec<ObsTarget> {
    #[cfg(target_os = "macos")]
    {
        macos::list()
    }
    #[cfg(target_os = "windows")]
    {
        windows::list()
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        Vec::new()
    }
}
