#[cfg(windows)]
use std::time::{Duration, Instant};
use std::{
    io,
    path::Path,
    process::{Child, Command, Stdio},
};

pub struct OwnedProcess {
    pub child: Child,
    #[cfg(unix)]
    reaped: bool,
    #[cfg(windows)]
    job: windows::Win32::Foundation::HANDLE,
}

impl OwnedProcess {
    pub fn spawn(script: &Path) -> io::Result<Self> {
        let mut command = Command::new("node");
        command.env_clear();
        for key in ["PATH", "SystemRoot", "WINDIR", "SystemDrive"] {
            if let Some(value) = std::env::var_os(key) {
                command.env(key, value);
            }
        }
        #[cfg(unix)]
        {
            use std::os::unix::process::CommandExt;
            command.process_group(0);
        }
        let child = command
            .arg(script)
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .spawn()?;
        #[cfg(windows)]
        {
            use std::os::windows::io::AsRawHandle;
            use windows::Win32::{
                Foundation::{CloseHandle, HANDLE},
                System::JobObjects::*,
            };
            let job = match unsafe { CreateJobObjectW(None, None) } {
                Ok(job) => job,
                Err(error) => {
                    let mut child = child;
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(io::Error::other(error));
                }
            };
            let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
            limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
            let configured = unsafe {
                SetInformationJobObject(
                    job,
                    JobObjectExtendedLimitInformation,
                    &limits as *const _ as *const _,
                    std::mem::size_of_val(&limits) as u32,
                )
                .and_then(|_| AssignProcessToJobObject(job, HANDLE(child.as_raw_handle())))
            };
            if let Err(error) = configured {
                unsafe {
                    let _ = CloseHandle(job);
                }
                let mut child = child;
                let _ = child.kill();
                let _ = child.wait();
                return Err(io::Error::other(error));
            }
            Ok(Self { child, job })
        }
        #[cfg(not(windows))]
        {
            Ok(Self {
                child,
                #[cfg(unix)]
                reaped: false,
            })
        }
    }

    pub fn has_exited(&mut self) -> io::Result<bool> {
        #[cfg(unix)]
        {
            if self.reaped {
                return Ok(true);
            }
            // Keep the dead leader unreaped: its PID pins our process-group identity.
            let mut status: libc::siginfo_t = unsafe { std::mem::zeroed() };
            if unsafe {
                libc::waitid(
                    libc::P_PID,
                    self.child.id(),
                    &mut status,
                    libc::WEXITED | libc::WNOHANG | libc::WNOWAIT,
                )
            } != 0
            {
                return Err(io::Error::last_os_error());
            }
            Ok(unsafe { status.si_pid() } != 0)
        }
        #[cfg(not(unix))]
        {
            Ok(self.child.try_wait()?.is_some())
        }
    }

    pub fn finish(&mut self) -> io::Result<std::process::ExitStatus> {
        #[cfg(unix)]
        self.kill_group()?;
        let status = self.child.wait()?;
        #[cfg(unix)]
        {
            self.reaped = true;
        }
        Ok(status)
    }

    #[cfg(unix)]
    fn kill_group(&self) -> io::Result<()> {
        if !self.reaped
            && unsafe { libc::kill(-(self.child.id() as i32), libc::SIGKILL) } != 0
            && io::Error::last_os_error().raw_os_error() != Some(libc::ESRCH)
        {
            return Err(io::Error::last_os_error());
        }
        Ok(())
    }

    pub fn force_stop(&mut self) -> io::Result<()> {
        #[cfg(windows)]
        unsafe {
            use windows::Win32::System::JobObjects::*;
            TerminateJobObject(self.job, 1).map_err(io::Error::other)?;
            let deadline = Instant::now() + Duration::from_secs(5);
            loop {
                let mut info = JOBOBJECT_BASIC_ACCOUNTING_INFORMATION::default();
                QueryInformationJobObject(
                    Some(self.job),
                    JobObjectBasicAccountingInformation,
                    &mut info as *mut _ as *mut _,
                    std::mem::size_of_val(&info) as u32,
                    None,
                )
                .map_err(io::Error::other)?;
                if info.ActiveProcesses == 0 {
                    break;
                }
                if Instant::now() >= deadline {
                    return Err(io::Error::other("owned job has live processes"));
                }
                std::thread::sleep(Duration::from_millis(10));
            }
        }
        #[cfg(unix)]
        {
            self.kill_group()?;
        }
        self.finish()?;
        Ok(())
    }
}

impl Drop for OwnedProcess {
    fn drop(&mut self) {
        #[cfg(windows)]
        unsafe {
            let _ = windows::Win32::Foundation::CloseHandle(self.job);
        }
        #[cfg(unix)]
        {
            if !self.reaped {
                let _ = self.kill_group();
                let _ = self.child.wait();
            }
        }
    }
}
