// 开发期保留控制台，发布版不弹控制台窗口。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

fn main() {
    namesplit_lib::run()
}
