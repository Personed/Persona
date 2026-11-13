use std::{
    sync::{Arc, atomic::Ordering},
    time::Duration,
};
use tauri::Manager;

pub fn enabled() -> bool {
    std::env::args()
        .any(|arg| arg == "--persistence-write-test" || arg == "--persistence-read-test")
}

pub fn run(app: tauri::AppHandle, state: Arc<crate::setup::Setup>) {
    let result = (|| -> Result<(), String> {
        let base = crate::layout::base()
            .canonicalize()
            .map_err(|e| e.to_string())?;
        let test_base = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("target/test-data")
            .canonicalize()
            .map_err(|e| e.to_string())?;
        if base.parent() != Some(test_base.as_path())
            || !base
                .file_name()
                .unwrap_or_default()
                .to_string_lossy()
                .starts_with("native-persistence-")
            || std::fs::read_to_string(base.join("native-persistence-fixture"))
                .ok()
                .as_deref()
                != Some("isolated native persistence acceptance\n")
            || !crate::persistent_local()
        {
            return Err("acceptance requires a fresh isolated installation fixture".into());
        }
        let setup = app.get_webview_window("setup").ok_or("missing setup")?;
        crate::setup_smoke::wait(
            &setup,
            "document.readyState === 'complete' && !!window.personaSetup",
        )?;
        setup.eval("document.querySelector('[name=runtime][value=native]').checked=true;document.querySelector('#setup').requestSubmit()").map_err(|e| e.to_string())?;
        let window =
            crate::remote_smoke::wait_window(&app, &state, "pilot", Duration::from_secs(120))?;
        crate::setup_smoke::wait(
            &window,
            "location.hostname === '127.0.0.1' && document.readyState === 'complete' && !!window.personaDesktop",
        )?;
        let write = std::env::args().any(|arg| arg == "--persistence-write-test");
        let script = if write {
            "window.__persistence='pending';(async()=>{const p=await fetch('/hosting/pairing',{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify({scope:'client'})});if(!p.ok)throw Error();const code=(await p.json()).code;const e=await fetch('/hosting/exchange',{method:'POST',credentials:'omit',headers:{'content-type':'application/json'},body:JSON.stringify({code,label:'pilot-native-synthetic-record'})});if(!e.ok)throw Error();const r=await fetch('/hosting/devices');if(!r.ok)throw Error();const d=(await r.json()).find(d=>d.label==='pilot-native-synthetic-record');if(!d)throw Error();window.__persistence={id:d.id,label:d.label,scope:d.scope}})().catch(()=>window.__persistence='failed')"
        } else {
            "window.__persistence='pending';fetch('/hosting/devices').then(async r=>{if(!r.ok)throw Error();const d=(await r.json()).find(d=>d.label==='pilot-native-synthetic-record');if(!d)throw Error();window.__persistence={id:d.id,label:d.label,scope:d.scope}}).catch(()=>window.__persistence='failed')"
        };
        window.eval(script).map_err(|e| e.to_string())?;
        crate::setup_smoke::wait(&window, "window.__persistence !== 'pending'")?;
        let record = crate::setup_smoke::read(&window, "window.__persistence")?;
        if record["label"] != "pilot-native-synthetic-record"
            || record["scope"] != "client"
            || !record["id"].is_string()
        {
            return Err("synthetic application record unavailable through native webview".into());
        }
        let expected = base.join("native-synthetic-record.json");
        if write {
            std::fs::write(
                expected,
                serde_json::to_vec(&record).map_err(|e| e.to_string())?,
            )
            .map_err(|e| e.to_string())?;
        } else {
            let saved: serde_json::Value =
                serde_json::from_slice(&std::fs::read(expected).map_err(|e| e.to_string())?)
                    .map_err(|e| e.to_string())?;
            if record != saved {
                return Err("application record changed across executable launches".into());
            }
        }
        println!(
            "pilot: native persistence acceptance {} verified",
            if write { "write" } else { "read" }
        );
        window.close().map_err(|e| e.to_string())?;
        Ok(())
    })();
    if let Err(error) = result {
        eprintln!("pilot: native persistence acceptance failed: {error}");
        state.failed.store(true, Ordering::SeqCst);
        if let Some(active) = state.request_close() {
            active.stop();
        } else {
            app.exit(1);
        }
    }
}
