use crate::{
    desktop::{Desktop, Instance},
    remote_smoke::{Fixture, desktop_state, submit_remote, wait_window},
    setup::Setup,
    setup_smoke::wait,
};
use std::{
    path::PathBuf,
    sync::{Arc, atomic::Ordering, mpsc},
    time::{Duration, Instant},
};
use tauri::Manager;

pub struct Check {
    directory: PathBuf,
    start: mpsc::Sender<(tauri::AppHandle, Arc<Setup>)>,
}

struct Runner {
    directory: PathBuf,
    fixture: Option<Fixture>,
    origin: String,
    failure: bool,
}

pub fn enabled() -> bool {
    if std::env::args().any(|arg| {
        arg == "--smoke-test" || arg.ends_with("-smoke-test") || arg == "--native-death-test"
    }) {
        return false;
    }
    std::env::args().any(|arg| arg == "--startup-check=restore" || arg == "--startup-check=retry")
}

pub fn prepare() -> Result<Option<Check>, String> {
    if !enabled() {
        return Ok(None);
    }
    let failure = if std::env::args().any(|arg| arg == "--startup-check=restore") {
        false
    } else if std::env::args().any(|arg| arg == "--startup-check=retry") {
        true
    } else {
        return Ok(None);
    };
    let (ready, prepared) = mpsc::channel();
    let (start, run) = mpsc::channel();
    // The fixture's Windows process handles stay on their owning thread.
    std::thread::spawn(move || match prepare_runner(failure) {
        Ok(check) => {
            if ready.send(Ok(check.directory.clone())).is_ok() {
                if let Ok((app, state)) = run.recv() {
                    check.run(app, state);
                }
            }
        }
        Err(error) => {
            let _ = ready.send(Err(error));
        }
    });
    let directory = prepared.recv().map_err(|e| e.to_string())??;
    Ok(Some(Check { directory, start }))
}

fn prepare_runner(failure: bool) -> Result<Runner, String> {
    let (mut fixture, origin) = Fixture::start()?;
    let origin = tauri::Url::parse(&origin)
        .map_err(|e| e.to_string())?
        .to_string();
    if failure {
        fixture.stop()?;
    }
    let check = Runner {
        directory: PathBuf::from(env!("CARGO_MANIFEST_DIR")).join(format!(
            "target/test-data/startup-native-{}",
            uuid::Uuid::new_v4()
        )),
        fixture: if failure { None } else { Some(fixture) },
        origin,
        failure,
    };
    desktop_at(check.directory.clone())
        .remember(Instance {
            id: "startup-fixture".into(),
            name: "Startup fixture".into(),
            mode: "existing".into(),
            runtime: "native".into(),
            url: check.origin.clone(),
        })
        .map_err(|e| e.to_string())?;
    Ok(check)
}

impl Check {
    pub fn desktop(&self) -> Desktop {
        desktop_at(self.directory.clone())
    }

    pub fn run(self, app: tauri::AppHandle, state: Arc<Setup>) {
        self.start
            .send((app, state))
            .expect("startup fixture worker exited");
    }
}

fn desktop_at(directory: PathBuf) -> Desktop {
    let mut desktop = Desktop::default();
    desktop.directory = directory;
    desktop
}

impl Runner {
    pub fn run(mut self, app: tauri::AppHandle, state: Arc<Setup>) {
        let mut result = (|| -> Result<(), String> {
            let setup = app.get_webview_window("setup").ok_or("missing setup")?;
            wait(
                &setup,
                "document.readyState === 'complete' && !!window.personaSetup",
            )?;
            if self.failure {
                let deadline = Instant::now() + Duration::from_secs(40);
                while state.last_error.lock().unwrap().is_none() {
                    if Instant::now() >= deadline {
                        return Err("startup probe did not fail".into());
                    }
                    std::thread::sleep(Duration::from_millis(25));
                }
                if !state.wait_idle(Duration::from_secs(10))
                    || state.closing.load(Ordering::SeqCst)
                    || state.connected.load(Ordering::SeqCst)
                    || app.get_webview_window("remote").is_some()
                    || !setup.is_visible().map_err(|e| e.to_string())?
                {
                    return Err("failed startup did not recover to idle visible setup".into());
                }
                wait(
                    &setup,
                    "!document.querySelector('#error').hidden && document.querySelector('#error').textContent.trim().length > 0 && !document.querySelector('#continue').disabled",
                )?;
                let (fixture, origin) = Fixture::start()?;
                self.fixture = Some(fixture);
                self.origin = origin;
                submit_remote(&setup, &self.origin, "Retry fixture")?;
                let deadline = Instant::now() + Duration::from_secs(10);
                while state.last_error.lock().unwrap().is_some() {
                    if Instant::now() >= deadline {
                        return Err("setup retry was not accepted".into());
                    }
                    std::thread::sleep(Duration::from_millis(25));
                }
            }
            let remote = wait_window(&app, &state, "remote", Duration::from_secs(40))?;
            let origin = serde_json::to_string(self.origin.trim_end_matches('/'))
                .map_err(|e| e.to_string())?;
            wait(
                &remote,
                &format!(
                    "location.origin === {origin} && document.readyState === 'complete' && !!window.personaDesktop"
                ),
            )?;
            let desktop = desktop_state(&remote)?;
            if desktop["value"]["mode"] != "existing"
                || (!self.failure && desktop["value"]["selected"] != "startup-fixture")
            {
                return Err("unexpected restored desktop state".into());
            }
            if !self.failure && state.last_error.lock().unwrap().is_some() {
                return Err("startup reported failure".into());
            }
            Ok(())
        })();
        // Close through the normal owned-attempt shutdown before deleting the fixture state.
        if let Some(active) = state.request_close() {
            active.stop();
        }
        let idle = state.wait_idle(Duration::from_secs(45));
        let stopped = self
            .fixture
            .as_mut()
            .is_none_or(|fixture| fixture.stop().is_ok());
        if !idle || !stopped {
            eprintln!(
                "pilot: startup cleanup failed: idle={idle} fixture-stopped={stopped}; evidence preserved"
            );
            result = Err("startup cleanup did not settle; evidence preserved".into());
        }
        if result.is_ok() && std::fs::remove_dir_all(&self.directory).is_err() {
            eprintln!("pilot: startup cleanup failed: could not remove disposable evidence");
            result = Err("startup evidence cleanup failed".into());
        }
        state.failed.store(result.is_err(), Ordering::SeqCst);
        if result.is_ok() {
            println!(
                "pilot: startup-check {} passed; saved state loaded without reset; remote UI verified; cleanup confirmed",
                if self.failure {
                    "failed-probe/setup/retry"
                } else {
                    "automatic-restore"
                }
            );
        } else {
            // Native errors and dynamic observation scripts may contain connection details.
            eprintln!(
                "pilot: startup-check failed (check or cleanup); disposable evidence preserved"
            );
        }
        app.exit(if result.is_ok() { 0 } else { 1 });
    }
}
