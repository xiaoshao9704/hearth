// 桌面壳入口：窗口装的就是 web/dist 那份网页，原生能力全部经 IPC 命令暴露。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    hearth_desktop_lib::run()
}
