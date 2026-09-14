// 非 macOS 平台：还没有原生采集实现（Windows 在 M2 用 GStreamer 现成元素接）。
use super::{Geometry, Source};
use crate::publish::Sink;

pub struct Prepared {
    pub geometry: Geometry,
}

pub struct Capture;

impl Capture {
    pub fn stop(self) {}
}

pub fn available() -> bool {
    false
}

pub fn list_sources() -> Result<Vec<Source>, String> {
    Err("本平台还没有原生采集实现".to_string())
}

pub fn prepare(_source_id: &str, _fps: u32) -> Result<Prepared, String> {
    Err("本平台还没有原生采集实现".to_string())
}

pub fn start(_prepared: Prepared, _sink: Sink) -> Result<Capture, String> {
    Err("本平台还没有原生采集实现".to_string())
}
