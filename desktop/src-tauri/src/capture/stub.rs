use super::{Geometry, Settings, Source};
use std::sync::{Arc, Mutex};
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
pub fn audio_available() -> bool {
    false
}
pub fn list_sources() -> Result<Vec<Source>, String> {
    Err("本平台还没有原生采集实现".into())
}
pub fn geometry(_: &str, _: Settings) -> Result<Geometry, String> {
    Err("本平台还没有原生采集实现".into())
}
pub fn prepare(_: &str, _: Settings, _: bool) -> Result<Prepared, String> {
    Err("本平台还没有原生采集实现".into())
}
impl Prepared {
    pub fn video_head(&self) -> String {
        String::new()
    }
    pub fn audio_head(&self) -> Result<String, String> {
        Err("本平台还没有原生采集实现".into())
    }
    pub fn attach(
        self,
        _: &gstreamer::Pipeline,
        _: Settings,
        _: Arc<Mutex<Option<String>>>,
    ) -> Result<Capture, String> {
        Err("本平台还没有原生采集实现".into())
    }
}
