// Windows Job Object 封装：把 sidecar 放进「句柄关闭即杀进程」的 Job Object，
// 这样即使 Tauri 主进程被强杀（TerminateProcess），内核也会随句柄关闭一起杀掉 sidecar。
use std::sync::OnceLock;

use windows_sys::Win32::Foundation::{CloseHandle, HANDLE};
use windows_sys::Win32::System::JobObjects::{
    AssignProcessToJobObject, CreateJobObjectW, JobObjectExtendedLimitInformation,
    SetInformationJobObject, JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
};
use windows_sys::Win32::System::Threading::{OpenProcess, PROCESS_SET_QUOTA, PROCESS_TERMINATE};

pub struct JobHandle(HANDLE);

// HANDLE 是内核对象句柄，跨线程使用是安全的（Job Object 本身线程无关）。
unsafe impl Send for JobHandle {}
unsafe impl Sync for JobHandle {}

impl JobHandle {
    fn create() -> std::io::Result<Self> {
        unsafe {
            let job = CreateJobObjectW(std::ptr::null(), std::ptr::null());
            if job.is_null() {
                return Err(std::io::Error::last_os_error());
            }
            let mut info: JOBOBJECT_EXTENDED_LIMIT_INFORMATION = std::mem::zeroed();
            info.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            let ok = SetInformationJobObject(
                job,
                JobObjectExtendedLimitInformation,
                &info as *const _ as *const core::ffi::c_void,
                std::mem::size_of::<JOBOBJECT_EXTENDED_LIMIT_INFORMATION>() as u32,
            );
            if ok == 0 {
                CloseHandle(job);
                return Err(std::io::Error::last_os_error());
            }
            Ok(Self(job))
        }
    }

    /// 把已启动的子进程加入 Job Object。已加入时重复调用返回 false（无害）。
    pub fn assign(&self, pid: u32) -> std::io::Result<()> {
        unsafe {
            let process = OpenProcess(PROCESS_SET_QUOTA | PROCESS_TERMINATE, 0, pid);
            if process.is_null() {
                return Err(std::io::Error::last_os_error());
            }
            let ok = AssignProcessToJobObject(self.0, process);
            let err = std::io::Error::last_os_error();
            CloseHandle(process);
            if ok == 0 {
                return Err(err);
            }
            Ok(())
        }
    }
}

impl Drop for JobHandle {
    fn drop(&mut self) {
        unsafe {
            CloseHandle(self.0);
        }
    }
}

static JOB: OnceLock<JobHandle> = OnceLock::new();

/// 全局单例：进程生命周期内只创建一个 Job Object；句柄随进程退出由内核关闭。
pub fn global_job() -> std::io::Result<&'static JobHandle> {
    if let Some(job) = JOB.get() {
        return Ok(job);
    }
    let job = JobHandle::create()?;
    let _ = JOB.set(job);
    Ok(JOB.get().expect("job object 初始化失败"))
}
