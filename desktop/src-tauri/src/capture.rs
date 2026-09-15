// 平台采集只在 Rust / GStreamer 内传帧；IPC 只允许按需的小尺寸 JPEG。
use serde::{Deserialize, Serialize};

pub const AUDIO_RATE: u32 = 48000;
pub const AUDIO_CHANNELS: usize = 2;

#[derive(Clone, Copy, Debug, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum AudioScope {
    Application,
    System,
    None,
}

#[derive(Serialize)]
pub struct Source {
    pub id: String,
    pub kind: String,
    pub title: String,
    pub app: String,
    pub audio_scope: AudioScope,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
pub struct Settings {
    pub width: u32,
    pub height: u32,
    pub fps: u32,
    pub bitrate_kbps: u32,
}

impl Settings {
    pub fn validate(self) -> Result<Self, String> {
        if !(2..=7680).contains(&self.width) || !(2..=4320).contains(&self.height) {
            return Err("画面上限须在 2–7680 × 2–4320 像素范围内".into());
        }
        if !(1..=120).contains(&self.fps) {
            return Err("帧率须在 1–120 fps 范围内".into());
        }
        if !(500..=100_000).contains(&self.bitrate_kbps) {
            return Err("视频码率须在 500–100000 kbps 范围内".into());
        }
        Ok(self)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Geometry {
    pub width: u32,
    pub height: u32,
}

impl Geometry {
    /// 两个维度都是上限，不放大；NV12 向下取偶数。过小或失效的源不能安全编码。
    pub fn fit(w: f64, h: f64, limits: Settings) -> Result<Self, String> {
        if !w.is_finite() || !h.is_finite() || w < 2.0 || h < 2.0 {
            return Err("采集源尺寸无效或窗口已最小化".into());
        }
        let ratio = (limits.width as f64 / w)
            .min(limits.height as f64 / h)
            .min(1.0);
        let width = (w * ratio).floor() as u32 & !1;
        let height = (h * ratio).floor() as u32 & !1;
        if width < 2 || height < 2 {
            return Err("采集源比例无法在当前尺寸上限内编码".into());
        }
        Ok(Self { width, height })
    }
}

#[cfg(target_os = "macos")]
mod macos;
#[cfg(target_os = "macos")]
pub use macos::*;
#[cfg(target_os = "windows")]
mod windows;
#[cfg(target_os = "windows")]
pub use windows::*;
#[cfg(not(any(target_os = "macos", target_os = "windows")))]
mod stub;
#[cfg(not(any(target_os = "macos", target_os = "windows")))]
pub use stub::*;

#[cfg(test)]
mod tests {
    use super::*;
    fn limits(w: u32, h: u32) -> Settings {
        Settings {
            width: w,
            height: h,
            fps: 60,
            bitrate_kbps: 6000,
        }
    }
    #[test]
    fn fit_preserves_bounds_and_never_upscales() {
        for (w, h, l, expected) in [
            (
                3840.,
                2160.,
                limits(1920, 1080),
                Geometry {
                    width: 1920,
                    height: 1080,
                },
            ),
            (
                900.,
                1600.,
                limits(1920, 1080),
                Geometry {
                    width: 606,
                    height: 1080,
                },
            ),
            (
                641.,
                481.,
                limits(1920, 1080),
                Geometry {
                    width: 640,
                    height: 480,
                },
            ),
            (
                3840.,
                2160.,
                limits(2560, 1440),
                Geometry {
                    width: 2560,
                    height: 1440,
                },
            ),
            (
                1920.,
                1080.,
                limits(7680, 4320),
                Geometry {
                    width: 1920,
                    height: 1080,
                },
            ),
        ] {
            assert_eq!(Geometry::fit(w, h, l).unwrap(), expected);
        }
        for w in [0., 1., f64::NAN, f64::INFINITY] {
            assert!(Geometry::fit(w, 100., limits(1920, 1080)).is_err());
        }
        assert!(Geometry::fit(100000., 2., limits(1920, 1080)).is_err());
    }
    #[test]
    fn settings_reject_out_of_range_instead_of_clamping() {
        let s = limits(2560, 1440);
        assert!(s.validate().is_ok());
        for bad in [
            Settings { fps: 0, ..s },
            Settings { fps: 121, ..s },
            Settings { width: 0, ..s },
            Settings { height: 4321, ..s },
            Settings {
                bitrate_kbps: 499,
                ..s
            },
            Settings {
                bitrate_kbps: 100001,
                ..s
            },
        ] {
            assert!(bad.validate().is_err());
        }
    }
}
