// WGC 与 WASAPI 由 GStreamer 插件实现；Win32 仅枚举、校验目标身份和尺寸。
// 属性依据 GStreamer d3d11screencapturesrc / wasapi2src 官方文档（1.22+）。
use super::{AudioScope, Geometry, Settings, Source};
use crate::obstargets::windows::{listable, process_path};
use gst::prelude::*;
use gstreamer as gst;
use std::sync::{Arc, Mutex};
use windows_sys::Win32::{
    Foundation::{CloseHandle, FILETIME, HWND, LPARAM, RECT},
    Graphics::Gdi::{EnumDisplayMonitors, GetMonitorInfoW, HDC, HMONITOR, MONITORINFO},
    System::Threading::{GetProcessTimes, OpenProcess, PROCESS_QUERY_LIMITED_INFORMATION},
    UI::WindowsAndMessaging::{
        EnumWindows, GetWindowRect, GetWindowTextW, GetWindowThreadProcessId, IsIconic, IsWindow,
        IsWindowVisible,
    },
};

// 不接受 JS 提供的任意 HWND/PID 配对：只有 Rust 枚举过的标识才允许使用。
static SOURCES: Mutex<Vec<Target>> = Mutex::new(Vec::new());
#[derive(Clone)]
struct Target {
    id: String,
    handle: usize,
    process: Option<(u32, u64)>,
}

fn process_created(pid: u32) -> Result<u64, String> {
    unsafe {
        let handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
        if handle.is_null() {
            return Err("无法验证窗口所属进程，请选择其他窗口".into());
        }
        let mut creation: FILETIME = std::mem::zeroed();
        let mut exit: FILETIME = std::mem::zeroed();
        let mut kernel: FILETIME = std::mem::zeroed();
        let mut user: FILETIME = std::mem::zeroed();
        let ok = GetProcessTimes(handle, &mut creation, &mut exit, &mut kernel, &mut user);
        CloseHandle(handle);
        if ok == 0 {
            return Err("无法验证窗口所属进程".into());
        }
        Ok(((creation.dwHighDateTime as u64) << 32) | creation.dwLowDateTime as u64)
    }
}

// 列表里显示的是程序名，不带扩展名（OBS 那边要带的另算，见 obstargets::windows）
fn process_name(pid: u32) -> String {
    std::path::Path::new(&process_path(pid))
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default()
}

impl Target {
    fn rect(&self) -> Result<RECT, String> {
        unsafe {
            if let Some((pid, created)) = self.process {
                let hwnd = self.handle as HWND;
                let mut actual_pid = 0;
                if IsWindow(hwnd) == 0
                    || GetWindowThreadProcessId(hwnd, &mut actual_pid) == 0
                    || actual_pid != pid
                    || process_created(pid)? != created
                {
                    return Err("窗口或所属进程已失效，请重新选源".into());
                }
                if IsIconic(hwnd) != 0 {
                    return Err("窗口已最小化，请恢复窗口后投屏".into());
                }
                let mut rect = std::mem::zeroed();
                if GetWindowRect(hwnd, &mut rect) == 0 {
                    return Err("无法读取窗口尺寸".into());
                }
                Ok(rect)
            } else {
                let mut info: MONITORINFO = std::mem::zeroed();
                info.cbSize = std::mem::size_of::<MONITORINFO>() as u32;
                if GetMonitorInfoW(self.handle as HMONITOR, &mut info) == 0 {
                    return Err("显示器已断开，请重新选源".into());
                }
                Ok(info.rcMonitor)
            }
        }
    }
}

fn lookup(id: &str) -> Result<Target, String> {
    SOURCES
        .lock()
        .unwrap()
        .iter()
        .find(|s| s.id == id)
        .cloned()
        .ok_or_else(|| "采集源不在当前列表，请重新选源".into())
}

fn windows_build() -> u32 {
    unsafe {
        let mut version: windows_sys::Win32::System::SystemInformation::OSVERSIONINFOW =
            std::mem::zeroed();
        version.dwOSVersionInfoSize = std::mem::size_of_val(&version) as u32;
        if windows_sys::Wdk::System::SystemServices::RtlGetVersion(&mut version) < 0 {
            return 0;
        }
        version.dwBuildNumber
    }
}
fn system_audio_available() -> bool {
    crate::publish::init_gst().is_ok() && gst::ElementFactory::find("wasapi2src").is_some()
}

pub fn available() -> bool {
    windows_build() >= 18362
        && crate::publish::init_gst().is_ok()
        && gst::ElementFactory::make("d3d11screencapturesrc")
            .build()
            .is_ok_and(|e| {
                ["capture-api", "window-handle", "monitor-handle"]
                    .iter()
                    .all(|p| e.find_property(p).is_some())
            })
}
pub fn audio_available() -> bool {
    windows_build() >= 20348
        && crate::publish::init_gst().is_ok()
        && gst::ElementFactory::make("wasapi2src")
            .build()
            .is_ok_and(|e| {
                ["loopback", "loopback-mode", "loopback-target-pid"]
                    .iter()
                    .all(|p| e.find_property(p).is_some())
            })
}

pub fn list_sources() -> Result<Vec<Source>, String> {
    if !available() {
        return Err("缺少 GStreamer WGC 采集插件 d3d11screencapturesrc（需要 1.22+）".into());
    }
    struct Enumeration {
        sources: Vec<Source>,
        targets: Vec<Target>,
        audio: bool,
    }
    unsafe extern "system" fn monitor(handle: HMONITOR, _: HDC, _: *mut RECT, data: LPARAM) -> i32 {
        let out = &mut *(data as *mut Enumeration);
        let id = format!("display:{}", handle as usize);
        let target = Target {
            id: id.clone(),
            handle: handle as usize,
            process: None,
        };
        if target.rect().is_ok() {
            out.sources.push(Source {
                id,
                kind: "display".into(),
                title: format!("显示器 {}", out.sources.len() + 1),
                app: String::new(),
                audio_scope: if system_audio_available() {
                    AudioScope::System
                } else {
                    AudioScope::None
                },
            });
            out.targets.push(target);
        }
        1
    }
    unsafe extern "system" fn window(hwnd: HWND, data: LPARAM) -> i32 {
        let out = &mut *(data as *mut Enumeration);
        if IsWindowVisible(hwnd) == 0 || IsIconic(hwnd) != 0 {
            return 1;
        }
        let mut title = [0u16; 512];
        let len = GetWindowTextW(hwnd, title.as_mut_ptr(), title.len() as i32);
        if len <= 0 {
            return 1;
        }
        let mut pid = 0;
        if GetWindowThreadProcessId(hwnd, &mut pid) == 0 || pid == 0 || !listable(hwnd, pid) {
            return 1;
        }
        let Ok(created) = process_created(pid) else {
            return 1;
        };
        let id = format!("window:{}:{pid}:{created}", hwnd as usize);
        let target = Target {
            id: id.clone(),
            handle: hwnd as usize,
            process: Some((pid, created)),
        };
        let Ok(rect) = target.rect() else {
            return 1;
        };
        if rect.right - rect.left < 40 || rect.bottom - rect.top < 40 {
            return 1;
        }
        out.sources.push(Source {
            id,
            kind: "window".into(),
            title: String::from_utf16_lossy(&title[..len as usize]),
            app: process_name(pid),
            audio_scope: if out.audio {
                AudioScope::Application
            } else {
                AudioScope::None
            },
        });
        out.targets.push(target);
        1
    }
    let mut out = Enumeration {
        sources: Vec::new(),
        targets: Vec::new(),
        audio: audio_available(),
    };
    unsafe {
        let data = &mut out as *mut Enumeration as LPARAM;
        if EnumDisplayMonitors(std::ptr::null_mut(), std::ptr::null(), Some(monitor), data) == 0
            || EnumWindows(Some(window), data) == 0
        {
            return Err("枚举采集源失败".into());
        }
    }
    *SOURCES.lock().unwrap() = out.targets;
    Ok(out.sources)
}

pub struct Prepared {
    target: Target,
    pub geometry: Geometry,
    fps: u32,
}
pub struct Capture {
    probes: Vec<(gst::Pad, gst::PadProbeId)>,
}
impl Capture {
    pub fn stop(self) {
        drop(self);
    }
}
impl Drop for Capture {
    fn drop(&mut self) {
        for (pad, id) in self.probes.drain(..) {
            pad.remove_probe(id);
        }
    }
}

pub fn geometry(source_id: &str, settings: Settings) -> Result<Geometry, String> {
    let rect = lookup(source_id)?.rect()?;
    Geometry::fit(
        (rect.right - rect.left) as f64,
        (rect.bottom - rect.top) as f64,
        settings,
    )
}
pub fn prepare(source_id: &str, settings: Settings, audio: bool) -> Result<Prepared, String> {
    let target = lookup(source_id)?;
    let geometry = geometry(source_id, settings)?;
    if audio
        && !(if target.process.is_some() {
            audio_available()
        } else {
            system_audio_available()
        })
    {
        return Err("应用音频需要支持进程树 loopback 的 Windows（build 20348+）及 WASAPI 插件；请关闭投屏音频或更新系统/运行时".into());
    }
    Ok(Prepared {
        target,
        geometry,
        fps: settings.fps,
    })
}
impl Prepared {
    pub fn video_head(&self) -> String {
        let prop = if self.target.process.is_some() {
            "window-handle"
        } else {
            "monitor-handle"
        };
        // HWND 仅来自经过校验的整数；不拼接任何用户文本。源、缩放、编码都不经过 JS。
        format!("d3d11screencapturesrc name=vsrc capture-api=wgc {prop}={} show-cursor=true ! video/x-raw,framerate={}/1 ! videoconvert ! videoscale add-borders=false ! capsfilter name=vlimit caps=video/x-raw,format=NV12,width={},height={},pixel-aspect-ratio=1/1", self.target.handle, self.fps, self.geometry.width, self.geometry.height)
    }
    pub fn audio_head(&self) -> Result<String, String> {
        self.target.rect()?;
        Ok(match self.target.process {
            Some((pid,_)) => format!("wasapi2src name=asrc loopback=true loopback-mode=include-process-tree loopback-target-pid={pid} low-latency=true"),
            None => "wasapi2src name=asrc loopback=true loopback-mode=default low-latency=true".into(),
        })
    }
    pub fn attach(
        self,
        pipeline: &gst::Pipeline,
        settings: Settings,
        error: Arc<Mutex<Option<String>>>,
    ) -> Result<Capture, String> {
        self.target.rect()?;
        let limit = pipeline.by_name("vlimit").ok_or("缺少视频尺寸限制")?;
        let target = self.target.clone();
        let video = pipeline
            .by_name("vsrc")
            .and_then(|e| e.static_pad("src"))
            .ok_or("缺少 WGC 视频输出")?;
        let video_error = error.clone();
        // 实际 WGC 帧尺寸才是像素权威（窗口边框/DPI 会使 GetWindowRect 不同）。
        // CAPS 事件到达缩放器前收紧尺寸；尺寸变化不触碰 WHIP 会话。
        let update_caps = move |caps: &gst::CapsRef| {
            let Some(s) = caps.structure(0) else {
                return;
            };
            if let (Ok(w), Ok(h)) = (s.get::<i32>("width"), s.get::<i32>("height")) {
                if let Ok(geometry) = Geometry::fit(w as f64, h as f64, settings) {
                    let caps = gst::Caps::builder("video/x-raw")
                        .field("format", "NV12")
                        .field("width", geometry.width as i32)
                        .field("height", geometry.height as i32)
                        .field("pixel-aspect-ratio", gst::Fraction::new(1, 1))
                        .build();
                    limit.set_property("caps", &caps);
                }
            }
        };
        if let Some(caps) = video.current_caps() {
            update_caps(&caps);
        }
        let mut probes = Vec::new();
        let probe = video.add_probe(
            gst::PadProbeType::BUFFER | gst::PadProbeType::EVENT_DOWNSTREAM,
            move |_, info| {
                if let Err(e) = target.rect() {
                    let mut error = video_error.lock().unwrap();
                    if error.is_none() {
                        *error = Some(e);
                    }
                    return gst::PadProbeReturn::Drop;
                }
                if let Some(event) = info.event() {
                    if let gst::EventView::Caps(caps) = event.view() {
                        update_caps(caps.caps());
                    }
                }
                gst::PadProbeReturn::Ok
            },
        );
        if let Some(probe) = probe {
            probes.push((video, probe));
        }
        if let Some(audio) = pipeline.by_name("asrc").and_then(|e| e.static_pad("src")) {
            let target = self.target;
            // 音频也独立复验，窗口关闭/句柄复用时不得继续发给错误应用。
            let probe = audio.add_probe(gst::PadProbeType::BUFFER, move |_, _| {
                if let Err(e) = target.rect() {
                    let mut error = error.lock().unwrap();
                    if error.is_none() {
                        *error = Some(e);
                    }
                    return gst::PadProbeReturn::Drop;
                }
                gst::PadProbeReturn::Ok
            });
            if let Some(probe) = probe {
                probes.push((audio, probe));
            }
        }
        Ok(Capture { probes })
    }
}
