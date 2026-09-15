// 用不采集屏幕、不联网的短测试管线验证硬件实际能产帧，不能只凭 factory 名称报可用。
use crate::capture::{Geometry, Settings};
use gst::prelude::*;
use gstreamer as gst;

pub struct Encoder {
    pub codec: &'static str,
    /// 实际选中的 GStreamer 元素名：设置页据此告诉用户这台机器在用哪个硬编
    pub name: &'static str,
    pub launch: String,
}

fn candidates(codec: &str) -> Vec<&'static str> {
    #[cfg(target_os = "macos")]
    {
        return if codec == "h264" {
            vec!["vtenc_h264_hw"]
        } else {
            vec!["vtenc_h265"]
        };
    }
    #[cfg(target_os = "windows")]
    {
        return if codec == "h264" {
            vec!["nvh264enc", "qsvh264enc", "amfh264enc", "mfh264enc"]
        } else {
            vec!["nvh265enc", "qsvh265enc", "amfh265enc", "mfh265enc"]
        };
    }
    #[cfg(not(any(target_os = "macos", target_os = "windows")))]
    {
        let _ = codec;
        Vec::new()
    }
}

fn description(name: &str, s: Settings) -> String {
    let gop = s.fps * 2;
    let bitrate = s.bitrate_kbps;
    if name.starts_with("vtenc") {
        format!("{name} name=venc realtime=true allow-frame-reordering=false max-keyframe-interval={gop} bitrate={bitrate}")
    } else if name.starts_with("qsv") {
        format!("{name} name=venc gop-size={gop} b-frames=0 rate-control=cbr bitrate={bitrate}")
    } else if name.starts_with("mf") {
        format!("{name} name=venc gop-size={gop} bframes=0 low-latency=true rc-mode=cbr bitrate={bitrate}")
    } else if name.starts_with("amf") {
        format!("{name} name=venc gop-size={gop} usage=ultra-low-latency rate-control=cbr bitrate={bitrate}")
    } else {
        format!("{name} name=venc gop-size={gop} bframes=0 rc-mode=cbr bitrate={bitrate}")
    }
}

pub fn select(
    codec: &str,
    geometry: Geometry,
    settings: Settings,
    fallback: bool,
) -> Result<Encoder, String> {
    crate::publish::init_gst()?;
    let codecs: &[&'static str] = match (codec, fallback) {
        ("h264", _) => &["h264"],
        (_, true) => &["h265", "h264"],
        _ => &["h265"],
    };
    let mut failures = Vec::new();
    for &codec in codecs {
        for name in candidates(codec) {
            let Some(factory) = gst::ElementFactory::find(name) else {
                // 每个候选都要留下失败原因：用户看到的「没有硬编」必须能答出是哪一步没过
                failures.push(format!("{name}: 插件未注册（缺插件或驱动不支持）"));
                continue;
            };
            if name.starts_with("mf")
                && !factory
                    .metadata("klass")
                    .is_some_and(|k| k.contains("Hardware"))
            {
                failures.push(format!("{name}: 只有软件实现，不算硬编"));
                continue;
            }
            let launch = description(name, settings);
            let desc = format!("videotestsrc num-buffers=3 ! video/x-raw,format=NV12,width={},height={},framerate={}/1 ! {launch} ! fakesink sync=false", geometry.width, geometry.height, settings.fps);
            let pipeline = match gst::parse::launch(&desc).and_then(|p| {
                p.downcast::<gst::Pipeline>()
                    .map_err(|_| gst::glib::Error::new(gst::CoreError::Failed, "pipeline"))
            }) {
                Ok(p) => p,
                Err(e) => {
                    failures.push(format!("{name}: {e}"));
                    continue;
                }
            };
            let result = if pipeline.set_state(gst::State::Playing).is_ok() {
                pipeline.bus().and_then(|b| {
                    b.timed_pop_filtered(
                        gst::ClockTime::from_seconds(3),
                        &[gst::MessageType::Eos, gst::MessageType::Error],
                    )
                })
            } else {
                None
            };
            let _ = pipeline.set_state(gst::State::Null);
            if matches!(
                result.as_ref().map(|m| m.view()),
                Some(gst::MessageView::Eos(_))
            ) {
                return Ok(Encoder {
                    codec,
                    name,
                    launch,
                });
            }
            failures.push(match result.as_ref().map(|m| m.view()) {
                Some(gst::MessageView::Error(e)) => format!("{name}: {}", e.error()),
                _ => format!("{name}: 硬件编码启动失败或超时"),
            });
        }
    }
    Err(format!(
        "没有可用的 {codec} 硬件编码器；请检查 GStreamer 编码插件、显卡驱动或降低画质。{}",
        failures.join("；")
    ))
}
