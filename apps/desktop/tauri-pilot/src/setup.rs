use crate::{Supervisor, run_supervisor};
use std::{
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
        mpsc,
    },
    time::{Duration, Instant},
};
use tauri::{Manager, Url};

#[derive(Default)]
pub struct Setup {
    pub active: Mutex<Option<Arc<Supervisor>>>,
    pub closing: AtomicBool,
    /// A connection is established and showing the app UI.
    pub connected: AtomicBool,
    /// The last native failure, so a caller can report why an attempt failed.
    pub last_error: Mutex<Option<String>>,
    pub accepted: AtomicUsize,
    pub navigation_blocked: AtomicUsize,
    pub failed: AtomicBool,
    switching: AtomicBool,
    switch_cancelled: AtomicBool,
    settled: Condvar,
}

impl Setup {
    fn register(&self, supervisor: Arc<Supervisor>, switching: bool) -> Result<(), String> {
        let mut active = self.active.lock().unwrap();
        if active.is_some()
            || self.closing.load(Ordering::SeqCst)
            || (!switching && self.switching.load(Ordering::SeqCst))
            || (switching && self.switch_cancelled.load(Ordering::SeqCst))
        {
            return Err("Aguarde a operação atual terminar.".into());
        }
        *active = Some(supervisor);
        *self.last_error.lock().unwrap() = None;
        Ok(())
    }

    fn reserve_switch(&self, connected_only: bool) -> Result<Option<Arc<Supervisor>>, String> {
        let active = self.active.lock().unwrap();
        if self.closing.load(Ordering::SeqCst)
            || self.switching.load(Ordering::SeqCst)
            || (connected_only && !self.connected.load(Ordering::SeqCst))
        {
            return Err("Aguarde a operação atual terminar.".into());
        }
        self.switching.store(true, Ordering::SeqCst);
        self.switch_cancelled.store(false, Ordering::SeqCst);
        if let Some(active) = active.as_ref() {
            active.superseded.store(true, Ordering::SeqCst);
        }
        Ok(active.clone())
    }

    pub fn request_close(&self) -> Option<Arc<Supervisor>> {
        let active = self.active.lock().unwrap();
        self.closing.store(true, Ordering::SeqCst);
        active.clone()
    }

    /// Waits until the running attempt settles. The condvar avoids polling.
    pub fn wait_idle(&self, timeout: Duration) -> bool {
        let deadline = Instant::now() + timeout;
        let mut active = self.active.lock().unwrap();
        while active.is_some() {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return false;
            }
            let (guard, result) = self.settled.wait_timeout(active, remaining).unwrap();
            active = guard;
            if result.timed_out() && active.is_some() {
                return false;
            }
        }
        true
    }

    pub fn finish_attempt(&self, opened: bool, supervisor: &Supervisor) -> bool {
        let mut active = self.active.lock().unwrap();
        let superseded =
            supervisor.superseded.load(Ordering::SeqCst) || self.switching.load(Ordering::SeqCst);
        let retry = (!opened || superseded) && !self.closing.load(Ordering::SeqCst);
        // Closing must either see this worker or observe idle and exit itself.
        *active = None;
        self.connected.store(false, Ordering::SeqCst);
        if !retry && !superseded {
            self.closing.store(true, Ordering::SeqCst);
        }
        self.settled.notify_all();
        retry
    }
}

pub fn check(window: &tauri::WebviewWindow) -> Result<(), String> {
    // Trust the packaged document as a unit, not as an isolation boundary between same-origin frames.
    if window.label() != "setup" || window.url().map_err(|e| e.to_string())? != url() {
        return Err("Origem de setup inválida.".into());
    }
    Ok(())
}

fn url() -> Url {
    #[cfg(windows)]
    let origin = "http://tauri.localhost/setup.html";
    #[cfg(not(windows))]
    let origin = "tauri://localhost/setup.html";
    Url::parse(origin).unwrap()
}

#[tauri::command]
pub fn setup_state(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, Arc<Setup>>,
    desktop: tauri::State<'_, Arc<crate::desktop::Desktop>>,
) -> Result<serde_json::Value, String> {
    check(&window)?;
    state.accepted.fetch_add(1, Ordering::SeqCst);
    let (instances, _) = desktop.list();
    Ok(serde_json::json!({
        "instances": instances.iter().map(crate::desktop::Instance::json).collect::<Vec<_>>(),
        // The setup page expects the selected instance, not its id.
        "selected": desktop.selected_instance().map(|item| item.json()),
        "prepare": false,
        "error": state.last_error.lock().unwrap().clone().unwrap_or_default()
    }))
}

/// Registers one connection attempt and runs it on its own thread.
fn begin(
    app: tauri::AppHandle,
    setup: Arc<Setup>,
    desktop: Arc<crate::desktop::Desktop>,
    remote_input: Option<serde_json::Value>,
    switching: bool,
) -> Result<mpsc::Receiver<serde_json::Value>, String> {
    let supervisor = Arc::new(Supervisor::default());
    setup.register(supervisor.clone(), switching)?;
    let (send, result) = mpsc::channel();
    std::thread::spawn(move || {
        run_supervisor(app, supervisor, setup, desktop, send, remote_input);
    });
    Ok(result)
}

/// Replaces the current connection with another saved environment.
pub fn switch(app: &tauri::AppHandle, instance: crate::desktop::Instance) {
    let setup = app.state::<Arc<Setup>>().inner().clone();
    let desktop = app.state::<Arc<crate::desktop::Desktop>>().inner().clone();
    let active = match setup.reserve_switch(false) {
        Ok(active) => active,
        Err(_) => return,
    };
    let app = app.clone();
    std::thread::spawn(move || {
        if let Some(active) = active {
            active.stop();
        }
        if !setup.wait_idle(Duration::from_secs(45)) {
            setup.switching.store(false, Ordering::SeqCst);
            show_failure(
                &app,
                &setup,
                "O ambiente anterior ainda está encerrando. Aguarde e tente novamente.",
            );
            return;
        }
        let remote_input = if instance.mode == "existing" {
            Some(serde_json::json!({
                "mode": "existing",
                "url": instance.url,
                "name": instance.name,
            }))
        } else {
            None
        };
        let result = begin(app.clone(), setup.clone(), desktop, remote_input, true);
        setup.switching.store(false, Ordering::SeqCst);
        if let Err(error) = result {
            if !setup.closing.load(Ordering::SeqCst) {
                show_failure(&app, &setup, &error);
            }
        }
    });
}

#[tauri::command]
pub async fn setup_submit(
    window: tauri::WebviewWindow,
    value: serde_json::Value,
    state: tauri::State<'_, Arc<Setup>>,
) -> Result<serde_json::Value, String> {
    check(&window)?;
    let remote = value.get("mode").and_then(|v| v.as_str()) == Some("existing");
    if !remote && value != serde_json::json!({"mode": "local", "runtime": "native"}) {
        return Ok(
            serde_json::json!({"ok": false, "error": "O ambiente local oferece somente execução nativa temporária. Docker não está disponível."}),
        );
    }
    let app = window.app_handle().clone();
    let setup = state.inner().clone();
    let desktop = app.state::<Arc<crate::desktop::Desktop>>().inner().clone();
    let replacement = if setup.connected.load(Ordering::SeqCst) {
        match setup.reserve_switch(true) {
            Ok(active) => Some(active),
            Err(error) => return Ok(serde_json::json!({"ok": false, "error": error})),
        }
    } else {
        None
    };
    tauri::async_runtime::spawn_blocking(move || {
        let switching = replacement.is_some();
        if let Some(active) = replacement {
            if let Some(active) = active {
                active.stop();
            }
            if !setup.wait_idle(Duration::from_secs(45)) {
                setup.switching.store(false, Ordering::SeqCst);
                let error = "O ambiente anterior ainda está encerrando. Aguarde e tente novamente.";
                show_failure(&app, &setup, error);
                return Ok(serde_json::json!({"ok": false, "error": error}));
            }
        }
        let result = begin(app, setup.clone(), desktop, remote.then_some(value), switching);
        if switching {
            setup.switching.store(false, Ordering::SeqCst);
        }
        let result = match result {
            Ok(result) => result,
            Err(error) => return Ok(serde_json::json!({"ok": false, "cancelled": setup.switch_cancelled.load(Ordering::SeqCst), "error": error})),
        };
        result
            .recv()
            .map_err(|_| "Preparação interrompida.".to_string())
    })
    .await
    .map_err(|e| e.to_string())?
}

#[tauri::command]
pub fn setup_cancel(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, Arc<Setup>>,
) -> Result<(), String> {
    check(&window)?;
    let active = state.active.lock().unwrap();
    state.switch_cancelled.store(true, Ordering::SeqCst);
    if let Some(active) = active.as_ref() {
        active.cancelled.store(true, Ordering::SeqCst);
        active.stop();
    }
    Ok(())
}

#[tauri::command]
pub fn setup_docker(window: tauri::WebviewWindow) -> Result<(), String> {
    check(&window)?;
    Err("Docker não está disponível neste piloto. Nenhuma URL externa foi aberta.".into())
}

pub fn progress(app: &tauri::AppHandle, phase: &str, message: &str) {
    if let Some(window) = app.get_webview_window("setup") {
        let value = serde_json::json!({"phase": phase, "message": message});
        let _ = window.eval(format!(
            "window.dispatchEvent(new CustomEvent('persona-setup-progress', {{detail:{value}}}))"
        ));
    }
}

pub fn show_failure(app: &tauri::AppHandle, setup: &Setup, message: &str) {
    *setup.last_error.lock().unwrap() = Some(message.into());
    if let Some(window) = app.get_webview_window("setup") {
        let _ = window.show();
        let _ = window.set_focus();
    }
    progress(app, "error", message);
}

pub fn open(app: &tauri::AppHandle) -> tauri::Result<()> {
    let expected = url();
    let navigation = app.state::<Arc<Setup>>().inner().clone();
    let auto_start =
        std::env::args().any(|arg| arg == "--smoke-test" || arg == "--native-death-test");
    let window =
        tauri::WebviewWindowBuilder::new(app, "setup", tauri::WebviewUrl::App("setup.html".into()))
            .title(if crate::persistent_local() {
                "Configurar Persona — piloto persistente isolado"
            } else {
                "Configurar Persona — piloto temporário"
            })
            .inner_size(680.0, 760.0)
            .min_inner_size(360.0, 540.0)
            .incognito(true)
            .disable_drag_drop_handler()
            .on_navigation(move |target| {
                let allowed = target == &expected;
                if !allowed {
                    navigation.navigation_blocked.fetch_add(1, Ordering::SeqCst);
                }
                allowed
            })
            .on_page_load(move |window, payload| {
                if auto_start && payload.event() == tauri::webview::PageLoadEvent::Finished {
                    let _ = window.eval("document.querySelector('#setup').requestSubmit()");
                }
            })
            .on_new_window(|_, _| tauri::webview::NewWindowResponse::Deny)
            .on_permission_request(|_, _| tauri::webview::PermissionResponse::Deny)
            .build()?;
    let state = app.state::<Arc<Setup>>().inner().clone();
    let handle = app.clone();
    let closing_window = window.clone();
    window.on_window_event(move |event| {
        if let tauri::WindowEvent::CloseRequested { api, .. } = event {
            api.prevent_close();
            let _ = closing_window.hide();
            // A live connection outlives the setup window; only an idle app quits.
            if state.connected.load(Ordering::SeqCst) {
                return;
            }
            if let Some(active) = state.request_close() {
                active.stop();
            } else {
                handle.exit(0);
            }
        }
    });
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Barrier;

    #[test]
    fn connected_submission_reserves_replacement_and_honors_gap_cancellation() {
        for cancelled in [false, true] {
            let state = Setup::default();
            let old = Arc::new(Supervisor::default());
            state.register(old.clone(), false).unwrap();
            assert!(state.reserve_switch(true).is_err());
            state.connected.store(true, Ordering::SeqCst);
            assert!(state.reserve_switch(true).unwrap().is_some());
            assert!(state.reserve_switch(true).is_err());
            assert!(state.finish_attempt(true, &old));
            assert!(
                state
                    .register(Arc::new(Supervisor::default()), false)
                    .is_err()
            );
            state.switch_cancelled.store(cancelled, Ordering::SeqCst);
            assert_eq!(
                state
                    .register(Arc::new(Supervisor::default()), true)
                    .is_ok(),
                !cancelled
            );
            assert!(!state.closing.load(Ordering::SeqCst));
        }
    }

    #[test]
    fn switching_keeps_app_open_but_explicit_close_wins() {
        for closing in [false, true] {
            let state = Setup::default();
            let supervisor = Arc::new(Supervisor::default());
            *state.active.lock().unwrap() = Some(supervisor.clone());
            state.connected.store(true, Ordering::SeqCst);
            assert!(state.reserve_switch(true).unwrap().is_some());
            if closing {
                state.request_close();
            }
            assert_eq!(state.finish_attempt(true, &supervisor), !closing);
            assert_eq!(state.closing.load(Ordering::SeqCst), closing);
            assert!(!state.connected.load(Ordering::SeqCst));
            assert!(state.wait_idle(Duration::ZERO));
        }
    }

    #[test]
    fn close_and_exit_cannot_lose_the_settled_worker() {
        // Both native event handlers use request_close; exercise their shared handoff.
        for event in ["CloseRequested", "ExitRequested"] {
            for close_first in [true, false] {
                let state = Arc::new(Setup::default());
                let supervisor = Arc::new(Supervisor::default());
                *state.active.lock().unwrap() = Some(supervisor.clone());
                let ready = Arc::new(Barrier::new(2));
                let ordered = Arc::new(Barrier::new(2));
                let worker = {
                    let state = state.clone();
                    let ready = ready.clone();
                    let ordered = ordered.clone();
                    std::thread::spawn(move || {
                        // Reproduce the stale pre-cleanup read from the reported race.
                        assert!(!state.closing.load(Ordering::SeqCst));
                        ready.wait();
                        if close_first {
                            ordered.wait();
                        }
                        let retry = state.finish_attempt(false, &Supervisor::default());
                        if !close_first {
                            ordered.wait();
                        }
                        retry
                    })
                };
                let closer = {
                    let state = state.clone();
                    std::thread::spawn(move || {
                        ready.wait();
                        if !close_first {
                            ordered.wait();
                        }
                        let active = state.request_close();
                        let exits_directly = active.is_none();
                        if let Some(active) = active {
                            active.stop();
                        }
                        if close_first {
                            ordered.wait();
                        }
                        exits_directly
                    })
                };
                let retry = worker.join().unwrap();
                let exits_directly = closer.join().unwrap();
                assert_eq!(retry, !close_first, "{event}, close_first={close_first}");
                assert_eq!(
                    exits_directly, !close_first,
                    "{event}, close_first={close_first}"
                );
                assert!(!retry || exits_directly, "{event}: no owner left to exit");
                assert!(state.active.lock().unwrap().is_none());
                assert!(state.closing.load(Ordering::SeqCst));
                assert_eq!(supervisor.stopping.load(Ordering::SeqCst), close_first);
            }
        }
    }
}
