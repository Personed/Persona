use std::{
    sync::{Arc, atomic::Ordering, mpsc},
    time::{Duration, Instant},
};
use tauri::Manager;

pub(crate) fn read(
    window: &tauri::WebviewWindow,
    script: &str,
) -> Result<serde_json::Value, String> {
    let (send, result) = mpsc::channel();
    window
        .eval_with_callback(script, move |value| {
            let _ = send.send(value);
        })
        .map_err(|e| e.to_string())?;
    serde_json::from_str(
        &result
            .recv_timeout(Duration::from_secs(10))
            .map_err(|e| e.to_string())?,
    )
    .map_err(|e| e.to_string())
}

pub(crate) fn wait(window: &tauri::WebviewWindow, script: &str) -> Result<(), String> {
    let deadline = Instant::now() + Duration::from_secs(100);
    loop {
        if read(window, script)? == true {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!("native setup smoke timed out: {script}"));
        }
        std::thread::sleep(Duration::from_millis(25));
    }
}

pub fn run(app: tauri::AppHandle, state: Arc<crate::setup::Setup>) {
    let result = (|| -> Result<(), String> {
        let setup = app.get_webview_window("setup").ok_or("missing setup")?;
        wait(
            &setup,
            "document.readyState === 'complete' && !!document.querySelector('#continue') && !!window.personaSetup",
        )?;
        // Grid/flex items are blockified; probe the shared class inside a normal block container.
        let styles = read(
            &setup,
            "(()=>{const host=document.createElement('div');const button=document.createElement('button');button.className='button';host.append(button);document.body.append(host);const css=getComputedStyle(button);const result={display:css.display,radius:css.borderRadius,token:getComputedStyle(document.documentElement).getPropertyValue('--radius-round').trim()};host.remove();return result})()",
        )?;
        if styles
            != serde_json::json!({"display": "inline-flex", "radius": "999px", "token": "999px"})
        {
            return Err(format!("setup CSS/token mismatch: {styles}"));
        }
        wait(
            &setup,
            "document.documentElement.dataset.platform === 'win32'",
        )?;
        let probe = tauri::WebviewWindowBuilder::new(
            &app,
            "probe",
            tauri::WebviewUrl::App("setup.html".into()),
        )
        .visible(false)
        .incognito(true)
        .build()
        .map_err(|e| e.to_string())?;
        wait(
            &probe,
            "!!window.__TAURI_INTERNALS__ && !!document.querySelector('#continue')",
        )?;
        let before = state.accepted.load(Ordering::SeqCst);
        if crate::setup::check(&probe).is_ok() {
            return Err("native label validator accepted probe".into());
        }
        probe.eval("window.__probe = 'pending'; window.__TAURI_INTERNALS__.invoke('setup_state').then(()=>window.__probe='allowed',()=>window.__probe='denied')").map_err(|e| e.to_string())?;
        wait(&probe, "window.__probe === 'denied'")?;
        if state.accepted.load(Ordering::SeqCst) != before {
            return Err("cross-window passed native setup validation".into());
        }
        probe.destroy().map_err(|e| e.to_string())?;
        for target in [
            "http://tauri.localhost/other.html",
            "https://example.invalid/",
        ] {
            let before = state.navigation_blocked.load(Ordering::SeqCst);
            setup
                .navigate(tauri::Url::parse(target).unwrap())
                .map_err(|e| e.to_string())?;
            let deadline = Instant::now() + Duration::from_secs(10);
            while state.navigation_blocked.load(Ordering::SeqCst) == before {
                if Instant::now() >= deadline {
                    return Err("native navigation callback did not deny target".into());
                }
                std::thread::sleep(Duration::from_millis(25));
            }
            crate::setup::check(&setup)?;
        }
        setup.eval("window.__frameBlocked=false; window.addEventListener('securitypolicyviolation',e=>{if(e.effectiveDirective==='frame-src')window.__frameBlocked=true}); const frame=document.createElement('iframe');frame.src='setup.html';document.body.append(frame)").map_err(|e| e.to_string())?;
        wait(&setup, "window.__frameBlocked === true")?;
        setup.eval("document.querySelector('iframe').remove(); document.querySelector('[name=runtime][value=docker]').checked=true; document.querySelector('#setup').requestSubmit()").map_err(|e| e.to_string())?;
        wait(
            &setup,
            "!document.querySelector('#error').hidden && document.querySelector('#error').textContent.includes('somente execução nativa') && !document.querySelector('#continue').disabled",
        )?;
        if state.active.lock().unwrap().is_some() {
            return Err("unsupported runtime started service".into());
        }
        setup.eval("window.__cancelOnce=true; personaSetup.progress(p=>{if(p.phase==='session' && window.__cancelOnce){window.__cancelOnce=false;document.querySelector('#cancel').click()}}); document.querySelector('[name=runtime][value=native]').checked=true; document.querySelector('#setup').requestSubmit()").map_err(|e| e.to_string())?;
        wait(
            &setup,
            "!document.querySelector('#error').hidden && document.querySelector('#error').textContent.includes('Operação cancelada') && !document.querySelector('#continue').disabled",
        )?;
        if state.active.lock().unwrap().is_some() {
            return Err("cancel returned before process settle".into());
        }
        println!(
            "pilot: setup DOM/CSS loaded; navigation/frame CSP/cross-window denied; argument error and cancellation rendered; process settled"
        );
        setup
            .eval("document.querySelector('#setup').requestSubmit()")
            .map_err(|e| e.to_string())?;
        let deadline = Instant::now() + Duration::from_secs(100);
        let pilot = loop {
            if let Some(window) = app.get_webview_window("pilot") {
                break window;
            }
            if Instant::now() >= deadline {
                return Err("missing temporary HTTP window".into());
            }
            std::thread::sleep(Duration::from_millis(25));
        };
        wait(
            &pilot,
            "location.hostname === '127.0.0.1' && document.readyState === 'complete' && !!window.__TAURI_INTERNALS__",
        )?;
        let before = state.accepted.load(Ordering::SeqCst);
        if crate::setup::check(&pilot).is_ok() {
            return Err("native validator accepted HTTP window".into());
        }
        pilot.eval("window.__probe='pending'; window.__TAURI_INTERNALS__.invoke('setup_state').then(()=>window.__probe='allowed',()=>window.__probe='denied')").map_err(|e| e.to_string())?;
        wait(&pilot, "window.__probe === 'denied'")?;
        if state.accepted.load(Ordering::SeqCst) != before {
            return Err("HTTP passed native setup validation".into());
        }
        println!(
            "pilot: temporary HTTP UI loaded; HTTP cross-window IPC denied; closing owned service"
        );
        pilot.close().map_err(|e| e.to_string())?;
        Ok(())
    })();
    if let Err(error) = result {
        eprintln!("pilot: setup smoke failed: {error}");
        state.failed.store(true, Ordering::SeqCst);
        if let Some(active) = state.request_close() {
            active.stop();
        } else {
            app.exit(1);
        }
    }
}
