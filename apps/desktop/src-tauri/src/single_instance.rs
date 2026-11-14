//! The channel carries one activation bit, never command lines or URLs.
use windows::{
    Win32::{
        Foundation::{CloseHandle, HANDLE, WAIT_ABANDONED, WAIT_OBJECT_0, WAIT_TIMEOUT},
        Security::{GetTokenInformation, TOKEN_QUERY, TOKEN_STATISTICS, TokenStatistics},
        System::Threading::{
            CreateEventW, CreateMutexW, GetCurrentProcess, OpenProcessToken, ReleaseMutex,
            SetEvent, WaitForSingleObject,
        },
    },
    core::PCWSTR,
};

struct Handle(HANDLE);
// Kernel synchronization handles support concurrent waits and signaling.
unsafe impl Send for Handle {}
unsafe impl Sync for Handle {}
impl Drop for Handle {
    fn drop(&mut self) {
        let _ = unsafe { CloseHandle(self.0) };
    }
}

pub struct Primary {
    mutex: Handle,
    event: std::sync::Arc<Handle>,
    listener: Option<(
        std::sync::Arc<std::sync::atomic::AtomicBool>,
        std::thread::JoinHandle<()>,
    )>,
}

impl Primary {
    pub fn acquire() -> Result<Option<Self>, Box<dyn std::error::Error>> {
        let mut token = HANDLE::default();
        unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token)? };
        let token = Handle(token);
        let mut stats = TOKEN_STATISTICS::default();
        let mut size = 0;
        unsafe {
            GetTokenInformation(
                token.0,
                TokenStatistics,
                Some((&mut stats as *mut TOKEN_STATISTICS).cast()),
                std::mem::size_of_val(&stats) as u32,
                &mut size,
            )?;
        }
        // Local scopes the channel to this session; the logon LUID separates users.
        // Default token DACLs protect the objects, including against other users.
        Self::named(&format!(
            "Local\\Persona.TauriPilot.{}-{}",
            stats.AuthenticationId.HighPart, stats.AuthenticationId.LowPart
        ))
    }

    fn named(name: &str) -> Result<Option<Self>, Box<dyn std::error::Error>> {
        let wide = |suffix: &str| {
            format!("{name}.{suffix}\0")
                .encode_utf16()
                .collect::<Vec<_>>()
        };
        let event_name = wide("activate");
        let mutex_name = wide("owner");
        // Create the event first: activation remains pending during native startup.
        let event =
            Handle(unsafe { CreateEventW(None, false, false, PCWSTR(event_name.as_ptr()))? });
        let mutex = Handle(unsafe { CreateMutexW(None, false, PCWSTR(mutex_name.as_ptr()))? });
        match unsafe { WaitForSingleObject(mutex.0, 0) } {
            WAIT_OBJECT_0 | WAIT_ABANDONED => Ok(Some(Self {
                mutex,
                event: std::sync::Arc::new(event),
                listener: None,
            })),
            WAIT_TIMEOUT => {
                unsafe { SetEvent(event.0)? };
                Ok(None)
            }
            _ => Err(std::io::Error::last_os_error().into()),
        }
    }

    #[cfg(test)]
    fn activated(&self) -> bool {
        unsafe { WaitForSingleObject(self.event.0, 0) == WAIT_OBJECT_0 }
    }

    pub fn listen(&mut self, app: tauri::AppHandle) {
        use std::sync::{
            Arc,
            atomic::{AtomicBool, Ordering},
        };
        use tauri::Manager;
        let stop = Arc::new(AtomicBool::new(false));
        let stopping = stop.clone();
        let event = self.event.clone();
        let worker = std::thread::spawn(move || {
            while !stopping.load(Ordering::SeqCst) {
                if unsafe { WaitForSingleObject(event.0, 100) } != WAIT_OBJECT_0 {
                    continue;
                }
                let handle = app.clone();
                let _ = app.run_on_main_thread(move || {
                    let setup = handle.state::<Arc<crate::setup::Setup>>();
                    if setup.closing.load(Ordering::SeqCst) {
                        return;
                    }
                    let desktop = handle.state::<Arc<crate::desktop::Desktop>>();
                    let window = handle
                        .get_webview_window(&desktop.activation_label())
                        .or_else(|| handle.get_webview_window("setup"));
                    if let Some(window) = window {
                        let _ = window.unminimize();
                        let _ = window.show();
                        let _ = window.set_focus();
                    }
                });
            }
        });
        self.listener = Some((stop, worker));
    }
}

impl Drop for Primary {
    fn drop(&mut self) {
        if let Some((stop, worker)) = self.listener.take() {
            stop.store(true, std::sync::atomic::Ordering::SeqCst);
            let _ = worker.join();
        }
        // Ownership stays on the main thread until the app's shutdown has settled.
        let _ = unsafe { ReleaseMutex(self.mutex.0) };
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        io::{BufRead, BufReader, Write},
        process::{Command, Stdio},
        time::{Duration, Instant},
    };

    #[test]
    #[ignore = "disposable child for the integration test"]
    fn child() {
        let Ok(name) = std::env::var("PERSONA_INSTANCE_TEST") else {
            return;
        };
        let owner = Primary::named(&name).unwrap();
        println!(
            "INSTANCE:{}",
            if owner.is_some() {
                "primary"
            } else {
                "duplicate"
            }
        );
        std::io::stdout().flush().unwrap();
        if let Some(owner) = owner {
            let deadline = Instant::now() + Duration::from_secs(15);
            while !owner.activated() {
                assert!(Instant::now() < deadline, "no second-launch activation");
                std::thread::sleep(Duration::from_millis(10));
            }
            println!("INSTANCE:activated");
        }
    }

    #[test]
    fn second_process_activates_and_owner_death_releases_lock() {
        let name = format!("Local\\Persona.Pilot.Test.{}", uuid::Uuid::new_v4());
        let spawn = || {
            Command::new(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "single_instance::tests::child",
                    "--ignored",
                    "--nocapture",
                ])
                .env("PERSONA_INSTANCE_TEST", &name)
                .stdout(Stdio::piped())
                .spawn()
                .unwrap()
        };
        let mut first = spawn();
        let mut output = BufReader::new(first.stdout.take().unwrap());
        let mut line = String::new();
        loop {
            line.clear();
            assert!(output.read_line(&mut line).unwrap() > 0);
            if line.contains("INSTANCE:primary") {
                break;
            }
        }
        let second = spawn().wait_with_output().unwrap();
        assert!(second.status.success());
        assert!(String::from_utf8_lossy(&second.stdout).contains("INSTANCE:duplicate"));
        let mut rest = String::new();
        std::io::Read::read_to_string(&mut output, &mut rest).unwrap();
        assert!(first.wait().unwrap().success());
        assert!(rest.contains("INSTANCE:activated"));
        let mut replacement = spawn();
        let mut output = BufReader::new(replacement.stdout.take().unwrap());
        loop {
            line.clear();
            assert!(output.read_line(&mut line).unwrap() > 0);
            if line.contains("INSTANCE:primary") {
                break;
            }
        }
        // Keep the kernel mutex alive so reacquisition must handle abandonment,
        // rather than merely creating a fresh object after the owner dies.
        let mutex_name = format!("{name}.owner\0").encode_utf16().collect::<Vec<_>>();
        let _observer =
            Handle(unsafe { CreateMutexW(None, false, PCWSTR(mutex_name.as_ptr())).unwrap() });
        replacement.kill().unwrap();
        replacement.wait().unwrap();
        assert!(Primary::named(&name).unwrap().is_some());
    }
}
