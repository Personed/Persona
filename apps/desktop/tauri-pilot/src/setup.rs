use crate::{Supervisor, run_supervisor};
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, AtomicUsize, Ordering},
    mpsc,
};
use tauri::{Manager, Url};

#[derive(Default)]
pub struct Setup {
    pub active: Mutex<Option<Arc<Supervisor>>>,
    pub closing: AtomicBool,
    pub accepted: AtomicUsize,
    pub navigation_blocked: AtomicUsize,
    pub failed: AtomicBool,
}

impl Setup {
    pub fn request_close(&self) -> Option<Arc<Supervisor>> {
        let active = self.active.lock().unwrap();
        self.closing.store(true, Ordering::SeqCst);
        active.clone()
    }

    pub fn finish_attempt(&self, opened: bool) -> bool {
        let mut active = self.active.lock().unwrap();
        let retry = !opened && !self.closing.load(Ordering::SeqCst);
        // Closing must either see this worker or observe idle and exit itself.
        *active = None;
        if !retry {
            self.closing.store(true, Ordering::SeqCst);
        }
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
) -> Result<serde_json::Value, String> {
    check(&window)?;
    state.accepted.fetch_add(1, Ordering::SeqCst);
    Ok(serde_json::json!({"instances": [], "prepare": false, "error": ""}))
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
    let supervisor = Arc::new(Supervisor::default());
    {
        let mut active = state.active.lock().unwrap();
        if active.is_some() || state.closing.load(Ordering::SeqCst) {
            return Ok(
                serde_json::json!({"ok": false, "error": "Aguarde a operação atual terminar."}),
            );
        }
        *active = Some(supervisor.clone());
    }
    let app = window.app_handle().clone();
    let setup = state.inner().clone();
    let desktop = app.state::<Arc<crate::desktop::Desktop>>().inner().clone();
    let (send, result) = mpsc::channel();
    std::thread::spawn(move || {
        run_supervisor(
            app,
            supervisor,
            setup,
            desktop,
            send,
            remote.then_some(value),
        )
    });
    tauri::async_runtime::spawn_blocking(move || {
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
    if let Some(active) = state.active.lock().unwrap().as_ref() {
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

pub fn open(app: &tauri::AppHandle) -> tauri::Result<()> {
    let expected = url();
    let navigation = app.state::<Arc<Setup>>().inner().clone();
    let auto_start =
        std::env::args().any(|arg| arg == "--smoke-test" || arg == "--native-death-test");
    let window =
        tauri::WebviewWindowBuilder::new(app, "setup", tauri::WebviewUrl::App("setup.html".into()))
            .title("Configurar Persona — piloto temporário")
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
                        let retry = state.finish_attempt(false);
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
