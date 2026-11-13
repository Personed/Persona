use std::{
    io::{BufRead, BufReader, Read, Write},
    process::ChildStdin,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
        mpsc::{self, Receiver, RecvTimeoutError},
    },
    time::{Duration, Instant},
};
mod process;
mod remote;
mod remote_smoke;
mod setup;
mod setup_smoke;
use process::OwnedProcess;
use tauri::{
    Manager, Url,
    webview::{NewWindowResponse, PermissionResponse},
};

#[derive(Default)]
struct Supervisor {
    input: Mutex<Option<ChildStdin>>,
    stopping: AtomicBool,
    stop_at: Mutex<Option<Instant>>,
    cancelled: AtomicBool,
}

impl Supervisor {
    fn stop(&self) {
        if !self.stopping.swap(true, Ordering::SeqCst) {
            *self.stop_at.lock().unwrap() = Some(Instant::now());
            if let Some(mut input) = self.input.lock().unwrap().take() {
                let _ = input.write_all(b"{\"type\":\"stop\"}\n");
            }
        }
    }
}

fn read_frame<R: Read>(
    reader: &mut BufReader<R>,
) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    let mut bytes = Vec::new();
    reader.take(16_385).read_until(b'\n', &mut bytes)?;
    if bytes.len() > 16_384 || bytes.last() != Some(&b'\n') {
        return Err("invalid private supervisor frame".into());
    }
    Ok(serde_json::from_slice(&bytes)?)
}

fn wait_frame(
    frames: &Receiver<Result<serde_json::Value, ()>>,
    state: &Supervisor,
    startup: bool,
    grace: Duration,
) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    let started = Instant::now();
    loop {
        if state
            .stop_at
            .lock()
            .unwrap()
            .is_some_and(|at| at.elapsed() >= grace)
            || (startup && started.elapsed() >= Duration::from_secs(120))
        {
            return Err("private supervisor deadline exceeded".into());
        }
        match frames.recv_timeout(Duration::from_millis(25)) {
            Ok(Ok(frame)) => return Ok(frame),
            Ok(Err(())) | Err(RecvTimeoutError::Disconnected) => {
                return Err("private supervisor disconnected".into());
            }
            Err(RecvTimeoutError::Timeout) => (),
        }
    }
}

fn run_supervisor(
    app: tauri::AppHandle,
    state: Arc<Supervisor>,
    setup: Arc<setup::Setup>,
    complete: mpsc::Sender<serde_json::Value>,
    remote_input: Option<serde_json::Value>,
) {
    let remote = remote_input.is_some();
    let label = if remote { "remote" } else { "pilot" };
    let mut failure_message = if remote {
        "Não foi possível conectar ao Persona.".to_string()
    } else {
        "Não foi possível preparar o serviço local temporário.".to_string()
    };
    let mut opened = false;
    let mut pending_bootstrap = None;
    setup::progress(
        &app,
        "start",
        if remote {
            "Verificando servidor…"
        } else {
            "Iniciando serviço local temporário…"
        },
    );
    let result = (|| -> Result<(), Box<dyn std::error::Error>> {
        let script = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join(if remote {
            "remote-probe.mjs"
        } else {
            "supervisor.mjs"
        });
        let mut owned = OwnedProcess::spawn(&script)?;
        let child = &mut owned.child;
        let mut input = child.stdin.take().ok_or("missing private input")?;
        writeln!(
            input,
            "{}",
            if let Some(value) = remote_input {
                serde_json::json!({"type": "start", "value": value})
            } else {
                serde_json::json!({"type": "start"})
            }
        )?;
        *state.input.lock().unwrap() = Some(input);
        if state.stopping.load(Ordering::SeqCst) {
            state.input.lock().unwrap().take();
        }
        let mut output = BufReader::new(child.stderr.take().ok_or("missing private output")?);
        let (send, frames) = mpsc::channel();
        std::thread::spawn(move || {
            loop {
                let frame = read_frame(&mut output).map_err(|_| ());
                let failed = frame.is_err();
                if send.send(frame).is_err() || failed {
                    break;
                }
            }
        });
        let mut transport_failed = false;
        let startup = (|| -> Result<(), Box<dyn std::error::Error>> {
            let ready = wait_frame(&frames, &state, true, Duration::from_secs(40))
                .inspect_err(|_| transport_failed = true)?;
            if ready["type"] != "ready" {
                if remote && let Some(message) = ready["message"].as_str() {
                    failure_message = message.to_string();
                }
                return Err("supervisor did not become ready".into());
            }
            setup::progress(
                &app,
                "session",
                if remote {
                    "Servidor verificado. Preparando janela…"
                } else {
                    "Sessão local verificada. Preparando janela…"
                },
            );
            let native_app = app.clone();
            let native_state = state.clone();
            let (complete, bootstrap) = mpsc::channel();
            let bootstrap_done = Arc::new(AtomicBool::new(false));
            let worker_done = bootstrap_done.clone();
            std::thread::spawn(move || {
                let app = native_app;
                let state = native_state;
                let result = (|| -> Result<(), Box<dyn std::error::Error>> {
                    let origin = Url::parse(ready["origin"].as_str().ok_or("missing origin")?)?;
                    if !remote
                        && (origin.scheme() != "http"
                            || origin.host_str() != Some("127.0.0.1")
                            || origin.port().is_none()
                            || origin.path() != "/"
                            || origin.query().is_some()
                            || origin.fragment().is_some()
                            || !origin.username().is_empty()
                            || origin.password().is_some())
                    {
                        return Err("invalid loopback origin".into());
                    }
                    if remote && !remote::valid_origin(&origin) {
                        return Err("invalid remote origin".into());
                    }
                    if state.stopping.load(Ordering::SeqCst) {
                        return Err("startup cancelled".into());
                    }
                    let mut config = app.config().app.windows[0].clone();
                    config.label = label.into();
                    config.visible = false;
                    config.url = tauri::WebviewUrl::External(Url::parse("about:blank")?);
                    let allowed = origin.clone();
                    let initial = Arc::new(AtomicBool::new(true));
                    let initial_navigation = initial.clone();
                    let navigation_state = state.clone();
                    let smoke = std::env::args().any(|arg| arg == "--smoke-test");
                    let loaded_origin = origin.clone();
                    let window = tauri::WebviewWindowBuilder::from_config(&app, &config)?
                .on_navigation(move |target| {
                    !navigation_state.stopping.load(Ordering::SeqCst) && (allows_navigation(&allowed, target)
                        || (initial_navigation.load(Ordering::SeqCst)
                            && target.as_str() == "about:blank"))
                })
                .on_new_window(move |url, _| {
                    if remote { remote::open_external(url); }
                    NewWindowResponse::Deny
                })
                .on_permission_request(|_, _| PermissionResponse::Deny)
                .on_page_load(move |window, payload| {
                    if smoke
                        && payload.event() == tauri::webview::PageLoadEvent::Finished
                        && allows_navigation(&loaded_origin, payload.url())
                    {
                        println!(
                            "pilot: loopback navigation finished after native cookie verification"
                        );
                        let _ = window.close();
                    }
                })
                .build()?;
                    if state.stopping.load(Ordering::SeqCst) {
                        return Err("bootstrap cancelled".into());
                    }
                    let stop = state.clone();
                    let closing_window = window.clone();
                    window.on_window_event(move |event| {
                        if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                            api.prevent_close();
                            stop.stop();
                            let _ = closing_window.hide();
                        }
                    });
                    if !remote {
                        let token = ready["token"].as_str().ok_or("missing private session")?;
                        if token.len() < 32 || token.len() > 4096 {
                            return Err("invalid private session".into());
                        }
                        use tauri::webview::cookie::{Cookie, SameSite};
                        window.set_cookie(
                            Cookie::build(("persona_session", token))
                                .domain("127.0.0.1")
                                .path("/")
                                .http_only(true)
                                .same_site(SameSite::Strict)
                                .build(),
                        )?;
                        // set_cookie queues a runtime action; read back off the UI thread before navigation.
                        let deadline = Instant::now() + Duration::from_secs(10);
                        loop {
                            if state.stopping.load(Ordering::SeqCst) {
                                return Err("bootstrap cancelled".into());
                            }
                            let verified =
                                window
                                    .cookies_for_url(origin.clone())?
                                    .iter()
                                    .any(|cookie| {
                                        cookie.name() == "persona_session"
                                            && cookie.value() == token
                                            && cookie.http_only() == Some(true)
                                            && cookie.same_site() == Some(SameSite::Strict)
                                            && cookie.path() == Some("/")
                                    });
                            if verified {
                                break;
                            }
                            if Instant::now() >= deadline {
                                return Err("native cookie readback failed".into());
                            }
                            std::thread::sleep(Duration::from_millis(25));
                        }
                    }
                    if state.stopping.load(Ordering::SeqCst) {
                        return Err("service stopped during startup".into());
                    }
                    if remote {
                        println!("pilot: remote probe verified; opening isolated remote UI");
                    } else {
                        println!("pilot: native cookie verified; opening temporary loopback UI");
                    }
                    initial.store(false, Ordering::SeqCst);
                    window.navigate(if remote {
                        origin
                    } else {
                        origin.join("index.html")?
                    })?;
                    window.show()?;
                    Ok(())
                })();
                worker_done.store(true, Ordering::SeqCst);
                let _ = complete.send(result.is_ok());
            });
            let result = wait_bootstrap(&bootstrap, &state, &mut owned, Duration::from_secs(20));
            if result.is_err() && !bootstrap_done.load(Ordering::SeqCst) {
                pending_bootstrap = Some(bootstrap);
            }
            result
        })();
        if startup.is_ok() {
            opened = true;
            setup::progress(
                &app,
                "ready",
                if remote {
                    "Conexão pronta."
                } else {
                    "Serviço local temporário pronto."
                },
            );
            if let Some(window) = app.get_webview_window("setup") {
                let _ = window.hide();
            }
            let _ = complete.send(serde_json::json!({"ok": true}));
        }
        if startup.is_err() {
            state.stop();
        }
        // A terminal frame or EOF closes the UI; never reconnect to this port.
        let normal_stop = if startup.is_ok() {
            wait_frame(&frames, &state, false, Duration::from_secs(40))
                .inspect_err(|_| transport_failed = true)
                .is_ok_and(|frame| {
                    frame["type"] == "stopped"
                        && frame["mode"] == "graceful"
                        && frame["cleanup"] == true
                })
        } else {
            false
        };
        state.stop();
        let hiding_app = app.clone();
        std::thread::spawn(move || {
            if let Some(window) = hiding_app.get_webview_window(label) {
                let _ = window.hide();
            }
        });
        if transport_failed {
            owned.force_stop()?;
        }
        let deadline =
            state.stop_at.lock().unwrap().unwrap_or_else(Instant::now) + Duration::from_secs(40);
        while !owned.has_exited()? {
            if transport_failed || Instant::now() >= deadline {
                owned.force_stop()?;
                break;
            }
            std::thread::sleep(Duration::from_millis(25));
        }
        let status = owned.finish()?;
        if !status.success() || startup.is_err() || !normal_stop {
            return Err("pilot startup or shutdown failed".into());
        }
        Ok(())
    })();
    // A cancelled native worker must settle before another attempt can reuse its label.
    if pending_bootstrap
        .is_some_and(|pending| pending.recv_timeout(Duration::from_secs(2)).is_err())
    {
        setup.failed.store(true, Ordering::SeqCst);
        setup.closing.store(true, Ordering::SeqCst);
    }
    if !opened && !setup.closing.load(Ordering::SeqCst) {
        if let Some(window) = app.get_webview_window(label) {
            let _ = window.destroy();
            let deadline = Instant::now() + Duration::from_secs(2);
            while app.get_webview_window(label).is_some() && Instant::now() < deadline {
                std::thread::sleep(Duration::from_millis(25));
            }
            if app.get_webview_window(label).is_some() {
                setup.failed.store(true, Ordering::SeqCst);
                setup.closing.store(true, Ordering::SeqCst);
            }
        }
    }
    if setup.finish_attempt(opened) {
        let _ = complete.send(serde_json::json!({"ok": false, "cancelled": state.cancelled.load(Ordering::SeqCst), "error": failure_message}));
        return;
    }
    if result.is_err() {
        eprintln!("pilot: startup or shutdown failed; no reconnect");
    }
    let exit_code = if result.is_ok() && !setup.failed.load(Ordering::SeqCst) {
        0
    } else {
        1
    };
    // A frozen native UI must not retain the app after owned-process shutdown settles.
    std::thread::spawn(move || {
        std::thread::sleep(Duration::from_secs(2));
        std::process::exit(exit_code);
    });
    app.exit(exit_code);
}

fn wait_bootstrap(
    result: &Receiver<bool>,
    state: &Supervisor,
    owned: &mut OwnedProcess,
    timeout: Duration,
) -> Result<(), Box<dyn std::error::Error>> {
    let started = Instant::now();
    loop {
        if state.stopping.load(Ordering::SeqCst) || owned.has_exited()? {
            return Err("bootstrap cancelled or supervisor exited".into());
        }
        if started.elapsed() >= timeout {
            return Err("native bootstrap deadline exceeded".into());
        }
        match result.recv_timeout(Duration::from_millis(25)) {
            Ok(true) => return Ok(()),
            Ok(false) | Err(RecvTimeoutError::Disconnected) => {
                return Err("native bootstrap failed".into());
            }
            Err(RecvTimeoutError::Timeout) => (),
        }
    }
}

fn allows_navigation(internal: &Url, target: &Url) -> bool {
    target.scheme() == internal.scheme()
        && target.host_str() == internal.host_str()
        && target.port_or_known_default() == internal.port_or_known_default()
        && target.username().is_empty()
        && target.password().is_none()
}

fn main() {
    let state = Arc::new(setup::Setup::default());
    let app = tauri::Builder::default()
        .runtime(tauri_runtime_wry::Wry::default())
        .manage(state.clone())
        .invoke_handler(tauri::generate_handler![
            setup::setup_state,
            setup::setup_submit,
            setup::setup_cancel,
            setup::setup_docker
        ])
        .setup(move |app| {
            setup::open(app.handle())?;
            if std::env::args().any(|arg| arg == "--setup-smoke-test") {
                let handle = app.handle().clone();
                let state = app.state::<Arc<setup::Setup>>().inner().clone();
                std::thread::spawn(move || setup_smoke::run(handle, state));
            }
            if std::env::args().any(|arg| arg == "--remote-smoke-test") {
                let handle = app.handle().clone();
                let state = app.state::<Arc<setup::Setup>>().inner().clone();
                std::thread::spawn(move || remote_smoke::run(handle, state));
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to run the isolated Persona Tauri pilot");
    app.run(move |_, event| {
        if let tauri::RunEvent::ExitRequested { api, .. } = event {
            if let Some(active) = state.request_close() {
                api.prevent_exit();
                active.stop();
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn blocked_native_bootstrap_does_not_own_process_shutdown() {
        for phase in ["create", "cookies"] {
            let base = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
            let directory = base
                .join("target/test-data")
                .join(format!("bootstrap-{}-{phase}", std::process::id()));
            std::fs::create_dir_all(&directory).unwrap();
            let marker = directory.join("writer");
            let mut owned =
                OwnedProcess::spawn(&base.join("silent-supervisor.fixture.mjs")).unwrap();
            let mut reader = BufReader::new(owned.child.stderr.take().unwrap());
            writeln!(
                owned.child.stdin.as_mut().unwrap(),
                "{}",
                serde_json::json!({"phase":"before", "marker":marker})
            )
            .unwrap();
            assert_eq!(read_frame(&mut reader).unwrap()["type"], "fixture-live");
            let state = Arc::new(Supervisor::default());
            let native_state = state.clone();
            let (release, blocked) = mpsc::channel();
            let (complete, result) = mpsc::channel();
            let native = std::thread::spawn(move || {
                // Simulates a native call that never returns until the test releases it.
                blocked.recv().unwrap();
                let _ = complete.send(!native_state.stopping.load(Ordering::SeqCst));
            });
            assert_eq!(
                wait_bootstrap(&result, &state, &mut owned, Duration::from_millis(100))
                    .unwrap_err()
                    .to_string(),
                "native bootstrap deadline exceeded"
            );
            state.stop();
            owned.force_stop().unwrap();
            assert!(owned.has_exited().unwrap());
            assert!(matches!(result.try_recv(), Err(mpsc::TryRecvError::Empty)));
            release.send(()).unwrap();
            assert!(!result.recv_timeout(Duration::from_secs(5)).unwrap());
            native.join().unwrap();
            assert!(marker.exists());
            // Preserve fixture data: killing writers does not prove runtime cleanup.
        }
    }

    #[cfg(unix)]
    #[test]
    fn drop_kills_descendant_when_unreaped_group_leader_has_exited() {
        let base = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
        let directory = base
            .join("target/test-data")
            .join(format!("orphan-{}", std::process::id()));
        std::fs::create_dir_all(&directory).unwrap();
        let mut owned = OwnedProcess::spawn(&base.join("silent-supervisor.fixture.mjs")).unwrap();
        let mut reader = BufReader::new(owned.child.stderr.take().unwrap());
        writeln!(
            owned.child.stdin.as_mut().unwrap(),
            "{}",
            serde_json::json!({"phase":"leader-exit", "marker":directory.join("writer")})
        )
        .unwrap();
        assert_eq!(read_frame(&mut reader).unwrap()["type"], "fixture-live");
        let deadline = Instant::now() + Duration::from_secs(5);
        while !owned.has_exited().unwrap() {
            assert!(Instant::now() < deadline, "fixture leader did not exit");
            std::thread::yield_now();
        }
        let (closed, eof) = mpsc::channel();
        std::thread::spawn(move || {
            let mut bytes = Vec::new();
            closed.send(reader.read_to_end(&mut bytes).is_ok()).unwrap();
        });
        assert!(matches!(eof.try_recv(), Err(mpsc::TryRecvError::Empty)));
        drop(owned);
        // The controlled writer retains this inherited pipe until process death.
        assert!(eof.recv_timeout(Duration::from_secs(5)).unwrap());
        assert!(directory.join("writer").exists());
    }

    #[test]
    fn stop_deadline_is_not_restarted_between_reader_phases() {
        let state = Supervisor::default();
        *state.stop_at.lock().unwrap() = Some(Instant::now() - Duration::from_secs(1));
        let (send, frames) = mpsc::channel();
        send.send(Ok(serde_json::json!({"type": "stopped"})))
            .unwrap();
        for startup in [true, false] {
            assert!(wait_frame(&frames, &state, startup, Duration::from_millis(100)).is_err());
        }
    }

    #[test]
    fn silent_supervisor_stop_deadline_reaches_owned_writer_before_and_after_ready() {
        for phase in ["before", "after"] {
            let base = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));
            let directory = base.join("target/test-data").join(format!(
                "silent-{}-{}",
                std::process::id(),
                phase
            ));
            std::fs::create_dir_all(&directory).unwrap();
            let marker = directory.join("writer");
            let mut owned =
                OwnedProcess::spawn(&base.join("silent-supervisor.fixture.mjs")).unwrap();
            let mut reader = BufReader::new(owned.child.stderr.take().unwrap());
            let (send, frames) = mpsc::channel();
            let reader_thread = std::thread::spawn(move || {
                loop {
                    let frame = read_frame(&mut reader).map_err(|_| ());
                    let failed = frame.is_err();
                    if send.send(frame).is_err() || failed {
                        break;
                    }
                }
            });
            let state = Supervisor::default();
            let command = serde_json::json!({ "phase": phase, "marker": marker });
            let mut input = owned.child.stdin.take().unwrap();
            writeln!(input, "{command}").unwrap();
            *state.input.lock().unwrap() = Some(input);
            let live = frames
                .recv_timeout(Duration::from_secs(10))
                .unwrap()
                .unwrap();
            assert_eq!(live["type"], "fixture-live");
            if phase == "after" {
                assert_eq!(
                    frames
                        .recv_timeout(Duration::from_secs(10))
                        .unwrap()
                        .unwrap()["type"],
                    "ready"
                );
            }
            state.stop();
            assert!(
                wait_frame(
                    &frames,
                    &state,
                    phase == "before",
                    Duration::from_millis(100)
                )
                .is_err()
            );
            // Windows waits for ActiveProcesses=0 in the job; no deletion precedes that.
            owned.force_stop().unwrap();
            assert!(owned.child.try_wait().unwrap().is_some());
            reader_thread.join().unwrap();
            assert!(marker.exists());
            #[cfg(windows)]
            std::fs::remove_dir_all(directory).unwrap();
            // Unix descendant reaping is not proven here; retain the fixture evidence there.
        }
    }

    #[test]
    fn navigation_stays_on_exact_runtime_origin() {
        for origin in [
            "http://127.0.0.1:45678/",
            "http://tauri.localhost/",
            "https://tauri.localhost/",
            "tauri://localhost/",
        ] {
            let internal = Url::parse(origin).unwrap();
            for path in ["index.html", "assets/app.js", "route?q=1#section"] {
                assert!(allows_navigation(&internal, &internal.join(path).unwrap()));
            }
            for external in [
                "https://example.com",
                "http://localhost",
                "http://tauri.localhost.evil/",
                "http://tauri.localhost:1234/",
                "file:///index.html",
                "data:text/html,hello",
                "about:blank",
                "javascript:alert(1)",
                "blob:http://tauri.localhost/id",
                "tauri://evil/",
                "http://user@tauri.localhost/",
                "http://127.0.0.1:45679/",
                "https://127.0.0.1:45678/",
            ] {
                assert!(
                    !allows_navigation(&internal, &Url::parse(external).unwrap()),
                    "{origin} allowed {external}"
                );
            }
        }
    }

    #[test]
    fn private_frames_require_bounded_complete_json_lines() {
        assert_eq!(
            read_frame(&mut BufReader::new(&b"{\"type\":\"stopped\"}\n"[..])).unwrap()["type"],
            "stopped"
        );
        for bytes in [b"".as_slice(), b"{}".as_slice(), b"invalid\n".as_slice()] {
            assert!(read_frame(&mut BufReader::new(bytes)).is_err());
        }
        let oversized = vec![b' '; 16_385];
        assert!(read_frame(&mut BufReader::new(oversized.as_slice())).is_err());
    }
}
