// Windows：EnumWindows 枚举顶层窗口，值组成 OBS 的 window 串「标题:类名:可执行名」——
// game_capture / window_capture / wasapi_process_output_capture 三个源都收这个格式。
// 过滤规则与原生采集那条路共用（listable），免得两处列出来的窗口对不上。
use windows_sys::Win32::{
    Foundation::{CloseHandle, HWND, LPARAM},
    Graphics::Dwm::{DwmGetWindowAttribute, DWMWA_CLOAKED},
    System::Threading::{
        GetCurrentProcessId, OpenProcess, QueryFullProcessImageNameW,
        PROCESS_QUERY_LIMITED_INFORMATION,
    },
    UI::WindowsAndMessaging::{
        EnumWindows, GetClassNameW, GetWindow, GetWindowLongPtrW, GetWindowTextW,
        GetWindowThreadProcessId, IsIconic, IsWindowVisible, GWL_EXSTYLE, GW_OWNER,
        WS_EX_APPWINDOW, WS_EX_TOOLWINDOW,
    },
};

use super::ObsTarget;

// 桌面壳、托盘、任务视图这类窗口：类名是唯一稳的判据，它们都可见、有标题、不是工具窗口。
const SHELL_CLASSES: [&str; 8] = [
    "Progman",
    "WorkerW",
    "Shell_TrayWnd",
    "Shell_SecondaryTrayWnd",
    "Windows.UI.Core.CoreWindow",
    "ForegroundStaging",
    "MultitaskingViewFrame",
    "XamlExplorerHostIslandWindow",
];

/// 用户能认出来、也能真的投出去的顶层窗口才进列表：EnumWindows 原样给的是整棵窗口树，
/// 里面大量是后台 UWP、属主面板与外壳窗口，列出来只会让人翻不到自己要的那一个。
///
/// # Safety
/// `hwnd` 必须是 EnumWindows 当场给的有效窗口句柄。
pub unsafe fn listable(hwnd: HWND, pid: u32) -> bool {
    if pid == GetCurrentProcessId() {
        return false; // 自己的窗口：投出去就是无限镜像
    }
    let mut cloaked: u32 = 0;
    if DwmGetWindowAttribute(
        hwnd,
        DWMWA_CLOAKED as u32,
        &mut cloaked as *mut u32 as *mut core::ffi::c_void,
        std::mem::size_of::<u32>() as u32,
    ) == 0
        && cloaked != 0
    {
        return false; // 挂起的 UWP、不在当前虚拟桌面：IsWindowVisible 仍为真，但画面取不到
    }
    let ex = GetWindowLongPtrW(hwnd, GWL_EXSTYLE) as u32;
    if ex & WS_EX_TOOLWINDOW != 0 {
        return false;
    }
    if !GetWindow(hwnd, GW_OWNER).is_null() && ex & WS_EX_APPWINDOW == 0 {
        return false; // 属主窗口的附属面板（提示条、弹出层），不是任务栏上那一个
    }
    !SHELL_CLASSES.contains(&class_name(hwnd).as_str())
}

/// 进程的可执行文件全路径；取不到就是空串（调用方按「不知道」处理）
pub fn process_path(pid: u32) -> String {
    unsafe {
        let handle = OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid);
        if handle.is_null() {
            return String::new();
        }
        let mut path = vec![0u16; 32768];
        let mut size = path.len() as u32;
        let ok = QueryFullProcessImageNameW(handle, 0, path.as_mut_ptr(), &mut size);
        CloseHandle(handle);
        if ok == 0 {
            return String::new();
        }
        String::from_utf16_lossy(&path[..size as usize])
    }
}

unsafe fn class_name(hwnd: HWND) -> String {
    let mut class = [0u16; 256];
    let len = GetClassNameW(hwnd, class.as_mut_ptr(), class.len() as i32);
    String::from_utf16_lossy(&class[..len.max(0) as usize])
}

/// OBS 的转义规则（window-helpers.c 的 encode_dstr）：先 `#` 后 `:`，顺序不能反，
/// 否则转义出来的 `#3A` 会被第二遍再转一次。
fn encode(raw: &str) -> String {
    raw.replace('#', "#22").replace(':', "#3A")
}

pub fn list() -> Vec<ObsTarget> {
    unsafe extern "system" fn window(hwnd: HWND, data: LPARAM) -> i32 {
        let out = &mut *(data as *mut Vec<ObsTarget>);
        if IsWindowVisible(hwnd) == 0 || IsIconic(hwnd) != 0 {
            return 1;
        }
        let mut title = [0u16; 512];
        let len = GetWindowTextW(hwnd, title.as_mut_ptr(), title.len() as i32);
        if len <= 0 {
            return 1;
        }
        let title = String::from_utf16_lossy(&title[..len as usize]);
        let mut pid = 0;
        if GetWindowThreadProcessId(hwnd, &mut pid) == 0 || pid == 0 || !listable(hwnd, pid) {
            return 1;
        }
        // OBS 的可执行名带扩展名（notepad.exe），与它自己列出来的那份对得上
        let path = process_path(pid);
        let exe = std::path::Path::new(&path)
            .file_name()
            .map(|s| s.to_string_lossy().into_owned())
            .unwrap_or_default();
        out.push(ObsTarget {
            kind: "window",
            label: if exe.is_empty() {
                title.clone()
            } else {
                format!("{title} — {exe}")
            },
            value: format!(
                "{}:{}:{}",
                encode(&title),
                encode(&class_name(hwnd)),
                encode(&exe)
            )
            .into(),
        });
        1
    }
    let mut out: Vec<ObsTarget> = Vec::new();
    unsafe {
        EnumWindows(Some(window), &mut out as *mut Vec<ObsTarget> as LPARAM);
    }
    out
}
