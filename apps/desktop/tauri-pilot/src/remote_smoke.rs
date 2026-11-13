use crate::{Supervisor, process::OwnedProcess, setup_smoke::wait};
use std::{
    io::{BufReader, Write},
    sync::{Arc, atomic::Ordering, mpsc},
    time::{Duration, Instant},
};
use tauri::{Manager, Url};

fn wait_window(
    app: &tauri::AppHandle,
    label: &str,
    timeout: Duration,
) -> Result<tauri::WebviewWindow, String> {
    let deadline = Instant::now() + timeout;
    loop {
        if let Some(window) = app.get_webview_window(label) {
            return Ok(window);
        }
        if Instant::now() >= deadline {
            return Err(format!("missing remote window: {label}"));
        }
        std::thread::sleep(Duration::from_millis(25));
    }
}

pub fn run(app: tauri::AppHandle, state: Arc<crate::setup::Setup>) {
    let result = (|| -> Result<(), String> {
        let setup = app.get_webview_window("setup").ok_or("missing setup")?;
        wait(
            &setup,
            "document.readyState === 'complete' && !!window.personaSetup",
        )?;

        let base = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
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
        let ready = crate::wait_frame(&frames, &supervisor, true, Duration::from_secs(120))
            .map_err(|e| e.to_string())?;
        if ready["type"] != "ready" {
            return Err("remote fixture did not become ready".into());
        }
        let origin = ready["origin"].as_str().ok_or("missing remote origin")?;
        let origin_url = Url::parse(origin).map_err(|e| e.to_string())?;
        let origin_literal = serde_json::to_string(origin).map_err(|e| e.to_string())?;

        setup
            .eval(&format!(
                "(()=>{{const form=document.querySelector('#setup');form.querySelector('[name=mode][value=existing]').checked=true;form.dispatchEvent(new Event('change',{{bubbles:true}}));document.querySelector('#url').value={origin_literal};document.querySelector('#name').value='Servidor de teste';form.requestSubmit();}})()"
            ))
            .map_err(|e| e.to_string())?;
        let remote = wait_window(&app, "remote", Duration::from_secs(30))?;
        wait(
            &remote,
            &format!("location.origin === {origin_literal} && document.readyState === 'complete'"),
        )?;

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

        writeln!(
            owned.child.stdin.as_mut().ok_or("missing fixture input")?,
            "{{\"type\":\"stop\"}}"
        )
        .map_err(|e| e.to_string())?;
        let stopped = frames
            .recv_timeout(Duration::from_secs(60))
            .map_err(|e| e.to_string())?
            .map_err(|_| "remote fixture disconnected")?;
        if stopped["type"] != "stopped" || stopped["cleanup"] != true {
            return Err("remote fixture did not stop cleanly".into());
        }
        if !owned.finish().map_err(|e| e.to_string())?.success() {
            return Err("remote fixture exited unsuccessfully".into());
        }
        reader.join().map_err(|_| "remote fixture reader failed")?;
        remote.close().map_err(|e| e.to_string())?;
        println!(
            "pilot: remote UI loaded; setup IPC and external navigation denied; controlled server stopped"
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
