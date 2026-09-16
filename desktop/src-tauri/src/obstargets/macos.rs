// macOS：应用取自 NSWorkspace 的 runningApplications（只留 regular，即 Dock 里有图标的那些），
// 窗口取自 CGWindowListCopyWindowInfo。两者正好对上 OBS screen_capture 的 type 2（应用，
// 键 application 收 bundle id）与 type 1（窗口，键 window 收 CGWindowID）。
use objc2_app_kit::{NSApplicationActivationPolicy, NSWorkspace};
use objc2_core_foundation::{CFArray, CFDictionary, CFNumber, CFString, CFType};
use objc2_core_graphics::{
    kCGNullWindowID, kCGWindowLayer, kCGWindowName, kCGWindowNumber, kCGWindowOwnerName,
    kCGWindowOwnerPID, CGWindowListCopyWindowInfo, CGWindowListOption,
};

use super::ObsTarget;

pub fn list() -> Vec<ObsTarget> {
    let mut out = apps();
    out.extend(onscreen_windows());
    out
}

fn apps() -> Vec<ObsTarget> {
    let me = std::process::id() as i32;
    let workspace = NSWorkspace::sharedWorkspace();
    let running = workspace.runningApplications();
    let mut out = Vec::new();
    for app in running.iter() {
        // 后台代理与纯服务进程（accessory / prohibited）没有可投的画面，列出来只是噪音
        if app.activationPolicy() != NSApplicationActivationPolicy::Regular
            || app.processIdentifier() == me
        {
            continue;
        }
        // 没有 bundle id 就没法写给 screen_capture 的 application 键
        let (Some(name), Some(bundle)) = (app.localizedName(), app.bundleIdentifier()) else {
            continue;
        };
        out.push(ObsTarget {
            kind: "app",
            label: name.to_string(),
            value: bundle.to_string().into(),
        });
    }
    out
}

fn onscreen_windows() -> Vec<ObsTarget> {
    let me = std::process::id() as i64;
    let option =
        CGWindowListOption::OptionOnScreenOnly | CGWindowListOption::ExcludeDesktopElements;
    let Some(list) = CGWindowListCopyWindowInfo(option, kCGNullWindowID) else {
        return Vec::new();
    };
    // CGWindowListCopyWindowInfo 给的是 CFDictionary 数组，键是文档写死的那几个 CFString 常量
    let list: &CFArray<CFDictionary<CFString, CFType>> = unsafe { list.cast_unchecked() };
    let mut out = Vec::new();
    for i in 0..list.len() {
        let Some(dict) = list.get(i) else { continue };
        // layer 0 才是普通应用窗口：菜单栏、Dock、输入法候选框都在别的层
        if num(&dict, unsafe { kCGWindowLayer }) != Some(0)
            || num(&dict, unsafe { kCGWindowOwnerPID }) == Some(me)
        {
            continue;
        }
        let Some(id) = num(&dict, unsafe { kCGWindowNumber }) else {
            continue;
        };
        // 无标题的选不中也认不出，一概不列。注意 macOS 10.15 起 kCGWindowName 要「屏幕录制」
        // 权限才给值：没授权的进程这里会空手而归，窗口这一档就是空的，应用那一档不受影响。
        let Some(title) = text(&dict, unsafe { kCGWindowName }).filter(|t| !t.is_empty()) else {
            continue;
        };
        let owner = text(&dict, unsafe { kCGWindowOwnerName }).unwrap_or_default();
        out.push(ObsTarget {
            kind: "window",
            label: if owner.is_empty() {
                title
            } else {
                format!("{owner} — {title}")
            },
            value: id.into(),
        });
    }
    out
}

fn num(dict: &CFDictionary<CFString, CFType>, key: &CFString) -> Option<i64> {
    let value = dict.get(key)?;
    value.downcast_ref::<CFNumber>()?.as_i64()
}

fn text(dict: &CFDictionary<CFString, CFType>, key: &CFString) -> Option<String> {
    let value = dict.get(key)?;
    Some(value.downcast_ref::<CFString>()?.to_string())
}
