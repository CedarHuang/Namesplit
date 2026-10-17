fn main() {
    // tauri-build 不会跟踪图标文件，图标变化时必须让本构建脚本重新执行，否则 exe 资源不会更新。
    println!("cargo:rerun-if-changed=icons/icon.ico");
    tauri_build::build()
}
