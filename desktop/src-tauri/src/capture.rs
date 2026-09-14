// 采集源抽象。M1 只有 macOS 的 ScreenCaptureKit 实现（GStreamer 在 macOS 没有可用的
// 屏幕与系统声采集元素，见 docs/plan-desktop.md 的 PoC 表）；其余平台先给空实现，
// 能力检测因此返回 native_publish=false，网页自动退回浏览器投屏。
use serde::Serialize;

/// SCK 的音频固定按这个格式要：48kHz 立体声，appsrc 的 caps 与交错逻辑都按它来。
pub const AUDIO_RATE: u32 = 48000;
pub const AUDIO_CHANNELS: usize = 2;

/// 采集画面的像素上限。超过这个尺寸的显示器按比例缩到框内，
/// 免得 5K 屏直接喂进编码器（固定码率下也只是把码率摊薄）。
const MAX_WIDTH: u32 = 1920;
const MAX_HEIGHT: u32 = 1080;

#[derive(Serialize)]
pub struct Source {
    pub id: String,
    /// display 或 window
    pub kind: String,
    pub title: String,
    pub app: String,
}

#[derive(Clone, Copy)]
pub struct Geometry {
    pub width: u32,
    pub height: u32,
}

impl Geometry {
    /// 按内容尺寸算出实际采集分辨率：先乘像素密度，再缩进上限框，最后取偶数
    /// （NV12 的色度面是半宽半高，奇数边长会让补齐规则变复杂）。
    fn fit(points_w: f64, points_h: f64, scale: f64) -> Self {
        let (w, h) = (points_w * scale, points_h * scale);
        let ratio = (MAX_WIDTH as f64 / w).min(MAX_HEIGHT as f64 / h).min(1.0);
        let even = |v: f64| ((v * ratio).round() as u32).max(2) & !1;
        Self { width: even(w), height: even(h) }
    }
}

#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "macos")]
pub use macos::{available, list_sources, prepare, start, Capture, Prepared};

#[cfg(not(target_os = "macos"))]
mod stub;
#[cfg(not(target_os = "macos"))]
pub use stub::{available, list_sources, prepare, start, Capture, Prepared};
