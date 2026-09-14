// macOS 采集：ScreenCaptureKit 的 SCStream 直接喂 appsrc。
//
// 画面取 NV12（vtenc_* 的原生格式，videoconvert 在这条链上是直通），逐行拷进
// GStreamer 按 VideoInfo 算好的步长里——IOSurface 的行对齐与 GStreamer 的不一样，
// 整块 memcpy 会错位。
// 声音取所属应用的声音：excludesCurrentProcessAudio 必须开，否则 WebView 里放的
// 远端语音会被再发布回房间（回声）；captureMicrophone 不开，麦克风由网页那侧负责。
use std::ptr::{null_mut, NonNull};
use std::sync::mpsc;
use std::time::Duration;

use block2::RcBlock;
use dispatch2::{DispatchQueue, DispatchQueueAttr, DispatchRetained};
use gstreamer as gst;
use gstreamer_video as gst_video;
use objc2::rc::Retained;
use objc2::runtime::{AnyClass, ProtocolObject};
use objc2::{define_class, msg_send, AnyThread, DefinedClass};
use objc2_core_audio_types::AudioBufferList;
use objc2_core_foundation::CFRetained;
use objc2_core_media::{
    kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment,
    CMAudioFormatDescriptionGetStreamBasicDescription, CMBlockBuffer, CMSampleBuffer, CMTime,
};
use objc2_core_video::{
    kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange, CVPixelBufferGetBaseAddressOfPlane,
    CVPixelBufferGetBytesPerRowOfPlane, CVPixelBufferGetHeight, CVPixelBufferGetPixelFormatType,
    CVPixelBufferGetWidth, CVPixelBufferLockBaseAddress, CVPixelBufferLockFlags,
    CVPixelBufferUnlockBaseAddress,
};
use objc2_foundation::{NSArray, NSError, NSObject, NSObjectProtocol};
use objc2_screen_capture_kit::{
    SCContentFilter, SCDisplay, SCShareableContent, SCStream, SCStreamConfiguration, SCStreamDelegate,
    SCStreamOutput, SCStreamOutputType, SCWindow,
};

use super::{Geometry, Source, AUDIO_CHANNELS, AUDIO_RATE};
use crate::publish::Sink;

// CoreAudio 的格式标志（objc2-core-audio-types 只给了结构体，标志位是 CoreAudioBaseTypes 的常量）
const FLAG_IS_FLOAT: u32 = 1 << 0;
const FLAG_IS_NON_INTERLEAVED: u32 = 1 << 5;

/// 把完成回调里拿到的 Objective-C 对象搬回调用线程。
///
/// SCK 的 getShareableContent 在自己的队列上回调，调用线程阻塞等结果。指针在回调里
/// retain 过一次，所有权随这个包装转移，转移前后都只有一个持有者，不存在并发访问。
struct SendPtr<T>(*mut T);
unsafe impl<T> Send for SendPtr<T> {}

pub fn available() -> bool {
    AnyClass::get(c"SCStream").is_some()
}

fn error_text(err: *mut NSError) -> String {
    let Some(err) = (unsafe { Retained::retain(err) }) else {
        return "ScreenCaptureKit 返回了未知错误".to_string();
    };
    // -3801 = SCStreamErrorCode.UserDeclined：用户没给屏幕录制权限（或还没弹过框）
    if err.code() == -3801 {
        return "屏幕录制权限被拒绝：到「系统设置 → 隐私与安全性 → 屏幕录制」里勾上 Hearth，然后重开应用".to_string();
    }
    err.localizedDescription().to_string()
}

/// 取一份可采集内容快照（阻塞等回调）。
fn shareable_content() -> Result<Retained<SCShareableContent>, String> {
    if !available() {
        return Err("本机 macOS 版本不支持 ScreenCaptureKit（需要 12.3 及以上）".to_string());
    }
    let (tx, rx) = mpsc::channel::<Result<SendPtr<SCShareableContent>, String>>();
    let handler = RcBlock::new(move |content: *mut SCShareableContent, err: *mut NSError| {
        let msg = match unsafe { Retained::retain(content) } {
            Some(content) => Ok(SendPtr(Retained::into_raw(content))),
            None => Err(error_text(err)),
        };
        let _ = tx.send(msg);
    });
    unsafe {
        SCShareableContent::getShareableContentExcludingDesktopWindows_onScreenWindowsOnly_completionHandler(
            true, true, &handler,
        );
    }
    let ptr = rx
        .recv_timeout(Duration::from_secs(15))
        .map_err(|_| "取采集源超时：多半是屏幕录制权限还没授予".to_string())??;
    NonNull::new(ptr.0)
        .map(|p| unsafe { Retained::from_raw(p.as_ptr()) })
        .flatten()
        .ok_or_else(|| "取采集源失败".to_string())
}

pub fn list_sources() -> Result<Vec<Source>, String> {
    let content = shareable_content()?;
    let mut out = Vec::new();
    for (i, display) in unsafe { content.displays() }.iter().enumerate() {
        out.push(Source {
            id: format!("display:{}", unsafe { display.displayID() }),
            kind: "display".to_string(),
            title: format!("显示器 {}", i + 1),
            app: String::new(),
        });
    }
    for window in unsafe { content.windows() }.iter() {
        // 只留正常层级、有标题、有像素的窗口：菜单栏、Dock、各种浮层选了也没意义
        if unsafe { window.windowLayer() } != 0 {
            continue;
        }
        let title = unsafe { window.title() }.map(|t| t.to_string()).unwrap_or_default();
        let frame = unsafe { window.frame() };
        if title.is_empty() || frame.size.width < 40.0 || frame.size.height < 40.0 {
            continue;
        }
        let app = unsafe { window.owningApplication() }
            .map(|a| unsafe { a.applicationName() }.to_string())
            .unwrap_or_default();
        out.push(Source {
            id: format!("window:{}", unsafe { window.windowID() }),
            kind: "window".to_string(),
            title,
            app,
        });
    }
    Ok(out)
}

/// 已解析好的采集目标：内容过滤器 + 流配置 + 算好的分辨率。
/// 管线要先按分辨率建好 caps，再把这份交给 start()。
pub struct Prepared {
    filter: Retained<SCContentFilter>,
    config: Retained<SCStreamConfiguration>,
    pub geometry: Geometry,
}

fn find_filter(content: &SCShareableContent, source_id: &str) -> Result<Retained<SCContentFilter>, String> {
    let (kind, raw) = source_id
        .split_once(':')
        .ok_or_else(|| "采集源标识格式不对".to_string())?;
    let id: u32 = raw.parse().map_err(|_| "采集源标识格式不对".to_string())?;
    match kind {
        "display" => {
            let display: Retained<SCDisplay> = unsafe { content.displays() }
                .iter()
                .find(|d| unsafe { d.displayID() } == id)
                .ok_or_else(|| "这块显示器已经不在了".to_string())?;
            let empty: Retained<NSArray<SCWindow>> = NSArray::new();
            Ok(unsafe { SCContentFilter::initWithDisplay_excludingWindows(SCContentFilter::alloc(), &display, &empty) })
        }
        "window" => {
            let window: Retained<SCWindow> = unsafe { content.windows() }
                .iter()
                .find(|w| unsafe { w.windowID() } == id)
                .ok_or_else(|| "这个窗口已经关了".to_string())?;
            Ok(unsafe { SCContentFilter::initWithDesktopIndependentWindow(SCContentFilter::alloc(), &window) })
        }
        _ => Err("采集源标识格式不对".to_string()),
    }
}

pub fn prepare(source_id: &str, fps: u32) -> Result<Prepared, String> {
    let content = shareable_content()?;
    let filter = find_filter(&content, source_id)?;

    let info = unsafe { SCShareableContent::infoForFilter(&filter) };
    let rect = unsafe { info.contentRect() };
    let scale = unsafe { info.pointPixelScale() } as f64;
    let geometry = Geometry::fit(rect.size.width, rect.size.height, if scale > 0.0 { scale } else { 1.0 });

    let config = unsafe { SCStreamConfiguration::init(SCStreamConfiguration::alloc()) };
    unsafe {
        config.setWidth(geometry.width as usize);
        config.setHeight(geometry.height as usize);
        config.setMinimumFrameInterval(CMTime::new(1, fps as i32));
        config.setPixelFormat(kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange);
        config.setScalesToFit(true);
        config.setPreservesAspectRatio(true);
        config.setShowsCursor(true);
        config.setQueueDepth(5);
        config.setCapturesAudio(true);
        config.setSampleRate(AUDIO_RATE as isize);
        config.setChannelCount(AUDIO_CHANNELS as isize);
        // 本进程的声音（WebView 里的远端语音）不能进来，否则等于把房间的声音再发布一遍
        config.setExcludesCurrentProcessAudio(true);
    }
    Ok(Prepared { filter, config, geometry })
}

pub struct Capture {
    stream: Retained<SCStream>,
    output: Retained<StreamOutput>,
    // 采样队列要活到 stop 之后：SCK 还会在上面派最后几个回调
    _queue: DispatchRetained<DispatchQueue>,
}

// SAFETY: Capture 只在持有它的那把锁下被访问（创建、stop 各一次），
// SCStream 的 start/stop 本身可以在任意线程调。
unsafe impl Send for Capture {}

impl Capture {
    pub fn stop(self) {
        unsafe { self.stream.stopCaptureWithCompletionHandler(None) };
        let sample_out: &ProtocolObject<dyn SCStreamOutput> = ProtocolObject::from_ref(&*self.output);
        let _ = unsafe { self.stream.removeStreamOutput_type_error(sample_out, SCStreamOutputType::Screen) };
        let _ = unsafe { self.stream.removeStreamOutput_type_error(sample_out, SCStreamOutputType::Audio) };
    }
}

pub fn start(prepared: Prepared, sink: Sink) -> Result<Capture, String> {
    let queue = DispatchQueue::new("app.hearth.capture", DispatchQueueAttr::SERIAL);
    let output = StreamOutput::new(Ivars { sink: sink.clone(), geometry: prepared.geometry });
    let sample_out: &ProtocolObject<dyn SCStreamOutput> = ProtocolObject::from_ref(&*output);
    let delegate: &ProtocolObject<dyn SCStreamDelegate> = ProtocolObject::from_ref(&*output);

    let stream = unsafe {
        SCStream::initWithFilter_configuration_delegate(
            SCStream::alloc(),
            &prepared.filter,
            &prepared.config,
            Some(delegate),
        )
    };
    unsafe {
        stream
            .addStreamOutput_type_sampleHandlerQueue_error(sample_out, SCStreamOutputType::Screen, Some(&queue))
            .map_err(|e| format!("挂画面输出失败：{}", e.localizedDescription()))?;
        stream
            .addStreamOutput_type_sampleHandlerQueue_error(sample_out, SCStreamOutputType::Audio, Some(&queue))
            .map_err(|e| format!("挂音频输出失败：{}", e.localizedDescription()))?;
    }

    // 启动是异步的：等它回一声，这样权限被拒能当场返回可读错误，而不是静默没画面
    let (tx, rx) = mpsc::channel::<Option<String>>();
    let handler = RcBlock::new(move |err: *mut NSError| {
        let _ = tx.send(if err.is_null() { None } else { Some(error_text(err)) });
    });
    unsafe { stream.startCaptureWithCompletionHandler(Some(&handler)) };
    match rx.recv_timeout(Duration::from_secs(15)) {
        Ok(None) => {}
        Ok(Some(err)) => return Err(err),
        Err(_) => return Err("启动屏幕采集超时".to_string()),
    }

    Ok(Capture { stream, output, _queue: queue })
}

struct Ivars {
    sink: Sink,
    geometry: Geometry,
}

define_class!(
    // SAFETY: 超类 NSObject 没有子类化约束；本类不实现 Drop。
    #[unsafe(super(NSObject))]
    #[name = "HearthStreamOutput"]
    #[ivars = Ivars]
    struct StreamOutput;

    unsafe impl NSObjectProtocol for StreamOutput {}

    unsafe impl SCStreamOutput for StreamOutput {
        #[unsafe(method(stream:didOutputSampleBuffer:ofType:))]
        unsafe fn did_output(&self, _stream: &SCStream, sbuf: &CMSampleBuffer, kind: SCStreamOutputType) {
            let ivars = self.ivars();
            match kind {
                SCStreamOutputType::Screen => unsafe { push_video(&ivars.sink, sbuf, ivars.geometry) },
                SCStreamOutputType::Audio => unsafe { push_audio(&ivars.sink, sbuf) },
                _ => {}
            }
        }
    }

    unsafe impl SCStreamDelegate for StreamOutput {
        #[unsafe(method(stream:didStopWithError:))]
        unsafe fn did_stop(&self, _stream: &SCStream, err: &NSError) {
            self.ivars()
                .sink
                .fail(format!("屏幕采集中断：{}", err.localizedDescription()));
        }
    }
);

impl StreamOutput {
    fn new(ivars: Ivars) -> Retained<Self> {
        let this = Self::alloc().set_ivars(ivars);
        unsafe { msg_send![super(this), init] }
    }
}

/// CVPixelBuffer(NV12) → GStreamer buffer。逐面逐行拷贝：两边的行步长不一样。
unsafe fn push_video(sink: &Sink, sbuf: &CMSampleBuffer, geometry: Geometry) {
    let Some(image) = (unsafe { sbuf.image_buffer() }) else {
        return; // 状态为 idle/blank 的帧没有像素，画面没变化，跳过即可
    };
    if CVPixelBufferGetPixelFormatType(&image) != kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange {
        sink.fail("采集到的像素格式不是 NV12");
        return;
    }
    let width = CVPixelBufferGetWidth(&image) as u32;
    let height = CVPixelBufferGetHeight(&image) as u32;
    if width != geometry.width || height != geometry.height {
        // caps 是按建流时的分辨率定死的；换显示器分辨率要重新发起投屏
        sink.fail("采集分辨率变了，请重新发起投屏");
        return;
    }
    let Ok(info) = gst_video::VideoInfo::builder(gst_video::VideoFormat::Nv12, width, height).build() else {
        sink.fail("视频格式信息构造失败");
        return;
    };

    if CVPixelBufferLockBaseAddress(&image, CVPixelBufferLockFlags::ReadOnly) != 0 {
        sink.fail("锁定采集帧失败");
        return;
    }
    let mut buffer = gst::Buffer::with_size(info.size()).expect("分配视频缓冲失败");
    {
        let mut map = buffer.get_mut().unwrap().map_writable().expect("映射视频缓冲失败");
        let dst = map.as_mut_slice();
        // NV12：面 0 是全尺寸的 Y，面 1 是半高的交错 UV（每行字节数与 Y 面相同）
        for plane in 0..2 {
            let src = CVPixelBufferGetBaseAddressOfPlane(&image, plane) as *const u8;
            let src_stride = CVPixelBufferGetBytesPerRowOfPlane(&image, plane);
            let dst_stride = info.stride()[plane] as usize;
            let dst_offset = info.offset()[plane];
            let rows = if plane == 0 { height as usize } else { height as usize / 2 };
            let row_bytes = dst_stride.min(src_stride);
            if src.is_null() {
                continue;
            }
            for row in 0..rows {
                let from = unsafe { src.add(row * src_stride) };
                let to = dst[dst_offset + row * dst_stride..].as_mut_ptr();
                unsafe { std::ptr::copy_nonoverlapping(from, to, row_bytes) };
            }
        }
    }
    CVPixelBufferUnlockBaseAddress(&image, CVPixelBufferLockFlags::ReadOnly);

    if sink.video.push_buffer(buffer).is_err() {
        sink.fail("画面写入管线失败");
    }
}

/// CMSampleBuffer(Float32 平面) → 交错 F32LE 推进 appsrc。
unsafe fn push_audio(sink: &Sink, sbuf: &CMSampleBuffer) {
    let Some(format) = (unsafe { sbuf.format_description() }) else {
        return;
    };
    let asbd = unsafe { CMAudioFormatDescriptionGetStreamBasicDescription(&format) };
    let Some(asbd) = (unsafe { asbd.as_ref() }) else {
        return;
    };
    if asbd.mFormatFlags & FLAG_IS_FLOAT == 0 || asbd.mBitsPerChannel != 32 {
        sink.fail("采集到的音频不是 32 位浮点");
        return;
    }
    if asbd.mSampleRate as u32 != AUDIO_RATE || asbd.mChannelsPerFrame as usize != AUDIO_CHANNELS {
        sink.fail("采集到的音频采样率或声道数与预期不符");
        return;
    }
    let frames = unsafe { sbuf.num_samples() } as usize;
    if frames == 0 {
        return;
    }

    // 先问要多大，再照着分配；block buffer 管着数据的生命周期，用完就释放
    let mut needed = 0usize;
    unsafe {
        sbuf.audio_buffer_list_with_retained_block_buffer(&mut needed, null_mut(), 0, None, None, 0, null_mut());
    }
    if needed == 0 {
        return;
    }
    let mut storage = vec![0u8; needed];
    let mut block: *mut CMBlockBuffer = null_mut();
    let status = unsafe {
        sbuf.audio_buffer_list_with_retained_block_buffer(
            null_mut(),
            storage.as_mut_ptr() as *mut AudioBufferList,
            needed,
            None,
            None,
            kCMSampleBufferFlag_AudioBufferList_Assure16ByteAlignment,
            &mut block,
        )
    };
    let _block = NonNull::new(block).map(|p| unsafe { CFRetained::from_raw(p) });
    if status != 0 {
        sink.fail(format!("取音频数据失败（OSStatus {status}）"));
        return;
    }

    let list = unsafe { &*(storage.as_ptr() as *const AudioBufferList) };
    let count = list.mNumberBuffers as usize;
    if count == 0 {
        return;
    }
    let buffers = unsafe { std::slice::from_raw_parts(list.mBuffers.as_ptr(), count) };
    let mut pcm = vec![0f32; frames * AUDIO_CHANNELS];
    if asbd.mFormatFlags & FLAG_IS_NON_INTERLEAVED != 0 {
        for ch in 0..AUDIO_CHANNELS.min(count) {
            let src = buffers[ch].mData as *const f32;
            if src.is_null() {
                continue;
            }
            let avail = buffers[ch].mDataByteSize as usize / 4;
            for i in 0..frames.min(avail) {
                pcm[i * AUDIO_CHANNELS + ch] = unsafe { *src.add(i) };
            }
        }
    } else {
        let src = buffers[0].mData as *const f32;
        if src.is_null() {
            return;
        }
        let avail = buffers[0].mDataByteSize as usize / 4;
        let n = pcm.len().min(avail);
        unsafe { std::ptr::copy_nonoverlapping(src, pcm.as_mut_ptr(), n) };
    }

    let bytes: &[u8] = unsafe { std::slice::from_raw_parts(pcm.as_ptr() as *const u8, pcm.len() * 4) };
    if sink.audio.push_buffer(gst::Buffer::from_slice(bytes.to_vec())).is_err() {
        sink.fail("声音写入管线失败");
    }
}
