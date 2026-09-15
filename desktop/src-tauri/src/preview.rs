// 一个请求只采一帧，不接 WHIP/音频；退出函数时先停系统采集，再释放 GStreamer。
use crate::{capture, publish};
use base64::{engine::general_purpose::STANDARD, Engine};
use gst::prelude::*;
use gstreamer as gst;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

static PREVIEW: Mutex<Option<Instant>> = Mutex::new(None);
struct Preview {
    pipeline: gst::Pipeline,
    capture: Option<capture::Capture>,
}
impl Drop for Preview {
    fn drop(&mut self) {
        if let Some(c) = self.capture.take() {
            c.stop();
        }
        let _ = self.pipeline.set_state(gst::State::Null);
    }
}

pub fn one_frame(source_id: &str) -> Result<Option<String>, String> {
    let waiting = Instant::now();
    let mut last = loop {
        if let Ok(guard) = PREVIEW.try_lock() {
            break guard;
        }
        if waiting.elapsed() > Duration::from_secs(10) {
            return Err("预览队列繁忙，请重试".into());
        }
        std::thread::sleep(Duration::from_millis(25));
    };
    if let Some(previous) = *last {
        if let Some(delay) = Duration::from_millis(100).checked_sub(previous.elapsed()) {
            std::thread::sleep(delay);
        }
    }
    *last = Some(Instant::now());
    // 单次预览的总时限：取不到帧的源只占这么久，后面排队的源不跟着一起卡住。
    let deadline = Instant::now() + Duration::from_secs(3);
    publish::init_gst()?;
    let settings = capture::Settings {
        width: 480,
        height: 270,
        fps: 2,
        bitrate_kbps: 500,
    };
    let prepared = if publish::testsrc_mode() {
        None
    } else {
        Some(capture::prepare(source_id, settings, false)?)
    };
    let head = match &prepared {
        Some(p) => p.video_head(),
        None => "videotestsrc is-live=true ! video/x-raw,width=480,height=270,framerate=2/1".into(),
    };
    let pipeline = gst::parse::launch(&format!("{head} ! videoconvert ! video/x-raw,format=I420 ! jpegenc quality=65 ! appsink name=preview max-buffers=1 drop=true sync=false"))
        .map_err(|e| format!("预览管线构建失败：{e}"))?.downcast::<gst::Pipeline>().map_err(|_| "预览管线类型错误")?;
    let mut preview = Preview {
        pipeline,
        capture: None,
    };
    let sink = preview
        .pipeline
        .by_name("preview")
        .and_then(|e| e.downcast::<gstreamer_app::AppSink>().ok())
        .ok_or("缺少预览 appsink")?;
    let error = Arc::new(Mutex::new(None));
    if let Some(p) = prepared {
        preview.capture = Some(p.attach(&preview.pipeline, settings, error.clone())?);
    }
    preview
        .pipeline
        .set_state(gst::State::Playing)
        .map_err(|e| format!("预览启动失败：{e}"))?;
    let left = deadline.saturating_duration_since(Instant::now());
    let sample = sink.try_pull_sample(gst::ClockTime::from_nseconds(left.as_nanos() as u64));
    if let Some(e) = error.lock().unwrap().take() {
        return Err(e);
    }
    let Some(sample) = sample else {
        if let Some(msg) = preview
            .pipeline
            .bus()
            .and_then(|b| b.pop_filtered(&[gst::MessageType::Error]))
        {
            if let gst::MessageView::Error(e) = msg.view() {
                return Err(format!("预览失败：{}", e.error()));
            }
        }
        return Ok(None);
    };
    let buffer = sample.buffer().ok_or("预览没有图像数据")?;
    let map = buffer.map_readable().map_err(|_| "无法读取预览")?;
    if map.size() > 128 * 1024 {
        return Err("预览图超过 IPC 大小上限".into());
    }
    Ok(Some(format!(
        "data:image/jpeg;base64,{}",
        STANDARD.encode(map.as_slice())
    )))
}
