use crate::{
    remote_smoke::{Fixture, desktop_state, submit_remote, wait_window},
    setup_smoke::wait,
};
use std::{
    sync::Arc,
    time::{Duration, Instant},
};
use tauri::Manager;

fn wait_gone(app: &tauri::AppHandle, label: &str, timeout: Duration) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    loop {
        if app.get_webview_window(label).is_none() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!("window still open: {label}"));
        }
        std::thread::sleep(Duration::from_millis(25));
    }
}

/// Connects to a saved remote environment and switches back to the local one
/// by calling the menu handler (not by interacting with the native menu).
pub fn run(app: tauri::AppHandle, state: Arc<crate::setup::Setup>) {
    let result = (|| -> Result<(), String> {
        let setup = app.get_webview_window("setup").ok_or("missing setup")?;
        wait(
            &setup,
            "document.readyState === 'complete' && !!window.personaSetup",
        )?;

        let (mut fixture, origin) = Fixture::start()?;
        submit_remote(&setup, &origin, "Servidor de teste")?;
        let remote = wait_window(&app, &state, "remote", Duration::from_secs(30))?;
        wait(
            &remote,
            "location.origin === 'http://127.0.0.1' || location.hostname === '127.0.0.1'",
        )?;
        let state_remote = desktop_state(&remote)?;
        let remote_id = state_remote["value"]["selected"]
            .as_str()
            .ok_or("remote instance was not selected")?
            .to_string();
        if state_remote["value"]["mode"] != "existing" {
            return Err("remote desktop state mismatch".into());
        }

        // Call the menu handler to replace the current environment with the local one.
        crate::desktop::menu_event(&app, "open-local");
        let local = wait_window(&app, &state, "pilot", Duration::from_secs(120))?;
        wait(
            &local,
            "location.hostname === '127.0.0.1' && document.readyState === 'complete' && !!window.personaDesktop",
        )?;
        wait_gone(&app, "remote", Duration::from_secs(20))?;
        let state_local = desktop_state(&local)?;
        if state_local["value"]["mode"] != "local" {
            return Err("local desktop state mismatch".into());
        }
        if state_local["value"]["selected"] != crate::desktop::LOCAL_ID {
            return Err("local selection mismatch".into());
        }

        // The saved remote environment comes back with the same identity.
        crate::desktop::menu_event(&app, &format!("instance:{remote_id}"));
        let remote_again = wait_window(&app, &state, "remote", Duration::from_secs(60))?;
        wait(
            &remote_again,
            &format!(
                "location.origin === {}",
                serde_json::to_string(&origin).map_err(|e| e.to_string())?
            ),
        )?;
        wait_gone(&app, "pilot", Duration::from_secs(20))?;
        let state_again = desktop_state(&remote_again)?;
        if state_again["value"]["selected"] != remote_id {
            return Err("remote selection mismatch".into());
        }
        if state_again["value"]["mode"] != "existing" {
            return Err("remote mode mismatch".into());
        }
        // Read the disposable file, not the in-memory list, to verify the next launch's state.
        let path = app
            .state::<Arc<crate::desktop::Desktop>>()
            .directory
            .join("instances.json");
        let text =
            std::fs::read_to_string(path).map_err(|_| "could not read persisted environments")?;
        let saved: serde_json::Value =
            serde_json::from_str(&text).map_err(|_| "could not parse persisted environments")?;
        let instances = saved["instances"]
            .as_array()
            .ok_or("persisted environments missing")?;
        if instances.len() != 2
            || saved["selected"].as_str() != Some(remote_id.as_str())
            || !instances.iter().any(|item| item["id"] == remote_id)
            || !instances
                .iter()
                .any(|item| item["id"] == crate::desktop::LOCAL_ID)
        {
            return Err("persisted environments or selection mismatch".into());
        }

        fixture.stop()?;
        remote_again.close().map_err(|e| e.to_string())?;
        println!(
            "pilot: environments switched via menu handler; previous window replaced; persisted file verified"
        );
        Ok(())
    })();

    if let Err(error) = result {
        eprintln!("pilot: switch smoke failed: {error}");
        state
            .failed
            .store(true, std::sync::atomic::Ordering::SeqCst);
        if let Some(active) = state.request_close() {
            active.stop();
        } else {
            app.exit(1);
        }
    }
}
