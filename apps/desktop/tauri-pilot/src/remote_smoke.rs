use crate::{
    Supervisor,
    process::OwnedProcess,
    setup_smoke::{read, wait},
};
use std::{
    io::{BufReader, Write},
    sync::{Arc, atomic::Ordering, mpsc},
    time::{Duration, Instant},
};
use tauri::{Manager, Url};

/// A disposable Persona server used by the remote smokes.
pub struct Fixture {
    owned: OwnedProcess,
    frames: mpsc::Receiver<Result<serde_json::Value, ()>>,
    reader: Option<std::thread::JoinHandle<()>>,
}

impl Fixture {
    pub fn start() -> Result<(Self, String), String> {
        let base = crate::layout::base();
        let mut owned = OwnedProcess::spawn(&base.join("remote-server.fixture.mjs"))
            .map_err(|e| e.to_string())?;
        let mut output = BufReader::new(
            owned
                .child
                .stderr
                .take()
                .ok_or("missing remote fixture output")?,
        );
        let (send, frames) = mpsc::channel();
        let reader = std::thread::spawn(move || {
            loop {
                let frame = crate::read_frame(&mut output).map_err(|_| ());
                let failed = frame.is_err();
                if send.send(frame).is_err() || failed {
                    break;
                }
            }
        });
        let supervisor = Supervisor::default();
        let ready = crate::wait_frame(
            &frames,
            &supervisor,
            Some(std::time::Instant::now() + Duration::from_secs(120)),
            Duration::from_secs(40),
        )
        .map_err(|e| e.to_string())?;
        if ready["type"] != "ready" {
            return Err("remote fixture did not become ready".into());
        }
        let origin = ready["origin"]
            .as_str()
            .ok_or("missing remote origin")?
            .to_string();
        Ok((
            Self {
                owned,
                frames,
                reader: Some(reader),
            },
            origin,
        ))
    }

    /// Stops the server and confirms that it released its resources.
    pub fn stop(&mut self) -> Result<(), String> {
        writeln!(
            self.owned
                .child
                .stdin
                .as_mut()
                .ok_or("missing fixture input")?,
            "{{\"type\":\"stop\"}}"
        )
        .map_err(|e| e.to_string())?;
        let stopped = self
            .frames
            .recv_timeout(Duration::from_secs(60))
            .map_err(|e| e.to_string())?
            .map_err(|_| "remote fixture disconnected")?;
        if stopped["type"] != "stopped" || stopped["cleanup"] != true {
            return Err("remote fixture did not stop cleanly".into());
        }
        if !self.owned.finish().map_err(|e| e.to_string())?.success() {
            return Err("remote fixture exited unsuccessfully".into());
        }
        if let Some(reader) = self.reader.take() {
            reader.join().map_err(|_| "remote fixture reader failed")?;
        }
        Ok(())
    }
}

/// Waits for a window, failing as soon as the native attempt reports an error
/// instead of burning the whole timeout.
pub fn wait_window(
    app: &tauri::AppHandle,
    state: &Arc<crate::setup::Setup>,
    label: &str,
    timeout: Duration,
) -> Result<tauri::WebviewWindow, String> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(window) = app.get_webview_window(label) {
            return Ok(window);
        }
        if let Some(error) = state.last_error.lock().unwrap().clone() {
            return Err(format!("attempt failed: {error}"));
        }
        if Instant::now() >= deadline {
            return Err(format!("missing window: {label}"));
        }
        std::thread::sleep(Duration::from_millis(25));
    }
}

/// Fills the setup form with a remote address and submits it.
pub fn submit_remote(setup: &tauri::WebviewWindow, origin: &str, name: &str) -> Result<(), String> {
    let origin_literal = serde_json::to_string(origin).map_err(|e| e.to_string())?;
    let name_literal = serde_json::to_string(name).map_err(|e| e.to_string())?;
    // The page restores the form from its own state; let that settle first.
    setup
        .eval("window.__state='pending';window.personaSetup.state().then(()=>window.__state='ready',()=>window.__state='failed')")
        .map_err(|e| e.to_string())?;
    wait(setup, "window.__state === 'ready'")?;
    setup
        .eval(&format!(
            "(()=>{{const form=document.querySelector('#setup');form.querySelector('[name=mode][value=existing]').checked=true;form.dispatchEvent(new Event('change',{{bubbles:true}}));document.querySelector('#url').value={origin_literal};document.querySelector('#name').value={name_literal};form.requestSubmit();}})()"
        ))
        .map_err(|e| e.to_string())?;
    Ok(())
}

/// Reads the desktop state exposed to the UI window.
pub fn desktop_state(window: &tauri::WebviewWindow) -> Result<serde_json::Value, String> {
    window
        .eval("window.__desktop='pending';window.personaDesktop.state().then(result=>window.__desktop=JSON.stringify(result),()=>window.__desktop='rejected')")
        .map_err(|e| e.to_string())?;
    wait(window, "window.__desktop !== 'pending'")?;
    let desktop = read(window, "window.__desktop")?;
    serde_json::from_str(desktop.as_str().unwrap_or_default()).map_err(|e| e.to_string())
}

pub fn run(app: tauri::AppHandle, state: Arc<crate::setup::Setup>) {
    let result = (|| -> Result<(), String> {
        let setup = app.get_webview_window("setup").ok_or("missing setup")?;
        wait(
            &setup,
            "document.readyState === 'complete' && !!window.personaSetup",
        )?;
        setup.eval("window.__desktop='pending';window.__TAURI_INTERNALS__.invoke('desktop_state').then(()=>window.__desktop='allowed',()=>window.__desktop='denied')").map_err(|e| e.to_string())?;
        wait(&setup, "window.__desktop === 'denied'")?;

        let (mut fixture, origin) = Fixture::start()?;
        let origin_url = Url::parse(&origin).map_err(|e| e.to_string())?;
        let origin_literal = serde_json::to_string(&origin).map_err(|e| e.to_string())?;
        submit_remote(&setup, &origin, "Servidor de teste")?;
        let remote = wait_window(&app, &state, "remote", Duration::from_secs(30))?;
        wait(
            &remote,
            &format!("location.origin === {origin_literal} && document.readyState === 'complete'"),
        )?;
        wait(&remote, "!!window.personaDesktop")?;
        let desktop = desktop_state(&remote)?;
        if desktop["value"]["mode"] != "existing" {
            return Err(format!("remote desktop state mismatch: {desktop}"));
        }
        remote
            .eval("window.__prepare='pending';window.__TAURI_INTERNALS__.invoke('desktop_prepare').then(()=>window.__prepare='allowed',()=>window.__prepare='denied')")
            .map_err(|e| e.to_string())?;
        wait(&remote, "window.__prepare === 'denied'")?;

        let before = state.accepted.load(Ordering::SeqCst);
        remote
            .eval("window.__probe='pending';if(window.__TAURI_INTERNALS__)window.__TAURI_INTERNALS__.invoke('setup_state').then(()=>window.__probe='allowed',()=>window.__probe='denied');else window.__probe='denied'")
            .map_err(|e| e.to_string())?;
        wait(
            &remote,
            "window.__probe === 'allowed' || window.__probe === 'denied'",
        )?;
        if state.accepted.load(Ordering::SeqCst) != before {
            return Err("remote window passed native setup validation".into());
        }

        let _ = remote.navigate(Url::parse("https://example.invalid/").unwrap());
        std::thread::sleep(Duration::from_millis(250));
        let current = remote.url().map_err(|e| e.to_string())?;
        if current.scheme() != origin_url.scheme()
            || current.host_str() != origin_url.host_str()
            || current.port_or_known_default() != origin_url.port_or_known_default()
        {
            return Err("remote navigation left the verified origin".into());
        }

        fixture.stop()?;
        remote.close().map_err(|e| e.to_string())?;
        println!(
            "pilot: remote UI loaded; desktop state exposed; setup/local commands and external navigation denied; controlled server stopped"
        );
        Ok(())
    })();

    if let Err(error) = result {
        eprintln!("pilot: remote smoke failed: {error}");
        state.failed.store(true, Ordering::SeqCst);
        if let Some(active) = state.request_close() {
            active.stop();
        } else {
            app.exit(1);
        }
    }
}
