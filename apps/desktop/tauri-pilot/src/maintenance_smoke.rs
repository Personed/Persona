use std::{
    io::{Read, Write},
    net::TcpListener,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tauri::Manager;

// Synthetic HTTP and optional native dialog only; never starts a supervisor or worker.
pub fn run(app: tauri::AppHandle, select_files: bool) {
    let stopped = Arc::new(AtomicBool::new(false));
    let stop_server = stopped.clone();
    let result = (|| -> Result<(), String> {
        let listener = TcpListener::bind("127.0.0.1:0").map_err(|e| e.to_string())?;
        let origin = tauri::Url::parse(&format!(
            "http://{}/",
            listener.local_addr().map_err(|e| e.to_string())?
        ))
        .map_err(|e| e.to_string())?;
        listener.set_nonblocking(true).map_err(|e| e.to_string())?;
        std::thread::spawn(move || {
            while !stop_server.load(Ordering::SeqCst) {
                if let Ok((mut stream, _)) = listener.accept() {
                    let _ = stream.set_read_timeout(Some(Duration::from_secs(1)));
                    let mut request = [0; 4096];
                    let _ = stream.read(&mut request);
                    let body = "<!doctype html><title>Synthetic maintenance ACL probe</title><p>No service or profile</p>";
                    let _ = write!(
                        stream,
                        "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
                        body.len(),
                        body
                    );
                } else {
                    std::thread::sleep(Duration::from_millis(10));
                }
            }
        });
        let desktop = app.state::<Arc<crate::desktop::Desktop>>();
        desktop.replace(crate::desktop::Target {
            instance: crate::desktop::Instance {
                id: "synthetic-maintenance".into(),
                name: "Synthetic".into(),
                mode: "local".into(),
                runtime: "native".into(),
                url: origin.to_string(),
            },
            label: "maintenance-http-probe".into(),
            origin: origin.clone(),
        });
        crate::desktop::grant(&app, "maintenance-http-probe", &origin, true)?;
        let probe = tauri::WebviewWindowBuilder::new(
            &app,
            "maintenance-http-probe",
            tauri::WebviewUrl::External(origin),
        )
        .title("Synthetic maintenance ACL smoke")
        .inner_size(420.0, 240.0)
        .incognito(true)
        .initialization_script(crate::desktop::bridge_script())
        .build()
        .map_err(|e| e.to_string())?;
        crate::setup_smoke::wait(
            &probe,
            "location.hostname === '127.0.0.1' && document.readyState === 'complete' && !!window.personaDesktop",
        )?;
        probe.eval("window.__maintenanceDenied=false;Promise.all(['maintenance_state','maintenance_start','maintenance_cancel'].map(command=>window.__TAURI_INTERNALS__.invoke(command,{password:'synthetic-only-password',action:'backup'}).then(()=>false,error=>/not allowed|not permitted|capabilit|permission|denied/i.test(String(error))))).then(values=>window.__maintenanceDenied=values.every(Boolean))").map_err(|e| e.to_string())?;
        crate::setup_smoke::wait(&probe, "window.__maintenanceDenied === true")?;
        probe.eval("window.__opened=null;personaDesktop.maintain().then(result=>window.__opened=result)").map_err(|e| e.to_string())?;
        crate::setup_smoke::wait(
            &probe,
            "window.__opened?.ok === true && window.__opened.value?.opened === true && window.__opened.value?.started === false",
        )?;
        let confirmation = app
            .get_webview_window("maintenance")
            .ok_or("Packaged confirmation did not open")?;
        crate::setup_smoke::wait(
            &confirmation,
            "document.readyState === 'complete' && !!window.personaMaintenance",
        )?;
        confirmation.eval("window.__packaged=null;personaMaintenance.state().then(result=>window.__packaged=result)").map_err(|e| e.to_string())?;
        crate::setup_smoke::wait(
            &confirmation,
            "window.__packaged?.ok === true && window.__packaged.value?.status === 'idle'",
        )?;
        confirmation.eval("window.__desktopDenied=false;window.__TAURI_INTERNALS__.invoke('desktop_maintain').then(()=>{},()=>window.__desktopDenied=true)").map_err(|e| e.to_string())?;
        crate::setup_smoke::wait(&confirmation, "window.__desktopDenied === true")?;
        if app
            .state::<Arc<crate::setup::Setup>>()
            .active
            .lock()
            .unwrap()
            .is_some()
        {
            return Err("Synthetic smoke unexpectedly started a supervisor".into());
        }
        if select_files {
            #[cfg(windows)]
            {
                let owner = confirmation
                    .hwnd()
                    .map_err(|_| "Native dialog owner unavailable")?
                    .0 as isize;
                for (action, key) in [
                    ("backup", "PERSONA_TAURI_SMOKE_BACKUP_PATH"),
                    ("restore", "PERSONA_TAURI_SMOKE_RESTORE_PATH"),
                ] {
                    let expected = std::path::PathBuf::from(
                        std::env::var_os(key).ok_or("Synthetic dialog path unavailable")?,
                    );
                    if expected.parent() != Some(crate::layout::base().as_path()) {
                        return Err(
                            "Synthetic dialog path must stay in the portable fixture".into()
                        );
                    }
                    let selected = crate::maintenance::choose_archive(action, owner)?
                        .ok_or("Synthetic file selection cancelled")?;
                    if selected != expected {
                        return Err("Native dialog returned an unexpected synthetic path".into());
                    }
                }
            }
            #[cfg(not(windows))]
            return Err("Native file dialog smoke is Windows-only".into());
        }
        if select_files {
            println!(
                "pilot: synthetic maintenance ACL passed; native save/open dialogs returned the expected fixture paths; no operation dispatched"
            );
        } else {
            println!(
                "pilot: synthetic maintenance HTTP ACL denied state/start/cancel; packaged confirmation opened through bridge; packaged state allowed; no operation dispatched"
            );
        }
        Ok(())
    })();
    stopped.store(true, Ordering::SeqCst);
    if let Err(error) = &result {
        eprintln!("pilot: synthetic maintenance smoke failed: {error}");
    }
    app.exit(if result.is_ok() { 0 } else { 1 });
}
