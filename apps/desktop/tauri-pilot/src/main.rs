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
mod desktop;
mod import_backup;
mod layout;
mod maintenance;
mod maintenance_smoke;
mod persistence_acceptance;
mod preparation;
mod process;
mod remote;
mod remote_smoke;
mod setup;
mod setup_smoke;
#[cfg(windows)]
mod single_instance;
mod startup_check;
mod switch_smoke;
mod tray;
use process::OwnedProcess;
use tauri::{
    Manager, Url,
    webview::{NewWindowResponse, PermissionResponse},
};

#[derive(Default)]
struct Supervisor {
    input: Mutex<Option<ChildStdin>>,
    stopping: AtomicBool,
    /// The attempt was replaced by another environment; it must not end the app.
    superseded: AtomicBool,
    stop_at: Mutex<Option<Instant>>,
    cancelled: AtomicBool,
    shutdown: Mutex<Option<bool>>,
    persistent: AtomicBool,
    maintenance_supported: AtomicBool,
    docker: AtomicBool,
    profile: Mutex<Option<std::path::PathBuf>>,
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
    startup_deadline: Option<Instant>,
    grace: Duration,
) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    loop {
        if state
            .stop_at
            .lock()
            .unwrap()
            .is_some_and(|at| at.elapsed() >= grace)
            || startup_deadline.is_some_and(|deadline| Instant::now() >= deadline)
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

fn shutdown_grace(state: &Supervisor) -> Duration {
    if state.docker.load(Ordering::SeqCst) {
        Duration::from_secs(90)
    } else {
        Duration::from_secs(40)
    }
}

fn wait_startup_frame(
    frames: &Receiver<Result<serde_json::Value, ()>>,
    state: &Supervisor,
    app: &tauri::AppHandle,
    timeout: Duration,
) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    let deadline = Instant::now() + timeout;
    loop {
        let frame = wait_frame(frames, state, Some(deadline), shutdown_grace(state))?;
        if frame["type"] == "progress" {
            let phase = frame["phase"].as_str().ok_or("invalid progress phase")?;
            let message = frame["message"]
                .as_str()
                .ok_or("invalid progress message")?;
            setup::progress(app, phase, message);
            continue;
        }
        return Ok(frame);
    }
}

fn run_supervisor(
    app: tauri::AppHandle,
    state: Arc<Supervisor>,
    setup: Arc<setup::Setup>,
    desktop: Arc<desktop::Desktop>,
    complete: mpsc::Sender<serde_json::Value>,
    remote_input: Option<serde_json::Value>,
    local_runtime: String,
) {
    let remote = remote_input.is_some();
    let docker = !remote && local_runtime == "docker";
    let persistent = !remote && (persistent_local() || docker);
    state.persistent.store(persistent, Ordering::SeqCst);
    state.docker.store(docker, Ordering::SeqCst);
    state.maintenance_supported.store(
        persistent && matches!(local_runtime.as_str(), "native" | "docker"),
        Ordering::SeqCst,
    );
    if !remote && let Some(window) = app.get_webview_window("setup") {
        let title = if local_runtime == "docker" {
            "Configurar Persona — piloto Docker persistente isolado"
        } else if persistent {
            "Configurar Persona — piloto persistente isolado"
        } else {
            "Configurar Persona — piloto temporário"
        };
        let _ = window.set_title(title);
    }
    let label = if remote { "remote" } else { "pilot" };
    let remote_name = remote_input
        .as_ref()
        .and_then(|value| value.get("name"))
        .and_then(|value| value.as_str())
        .map(str::trim)
        .filter(|name| !name.is_empty())
        .unwrap_or("Persona remoto")
        .to_string();
    let mut failure_message = if remote {
        "Não foi possível conectar ao Persona.".to_string()
    } else {
        "Não foi possível preparar o serviço local isolado do piloto.".to_string()
    };
    let mut opened = false;
    let mut pending_bootstrap = None;
    setup::progress(
        &app,
        "start",
        if remote {
            "Verificando servidor…"
        } else if local_runtime == "docker" {
            "Preparando perfil Docker persistente e isolado do piloto…"
        } else if persistent {
            "Iniciando serviço persistente isolado do piloto…"
        } else {
            "Iniciando serviço local temporário…"
        },
    );
    let result = (|| -> Result<(), Box<dyn std::error::Error>> {
        let script = layout::base().join(if remote {
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
                serde_json::json!({"type": "start", "persistent": persistent, "runtime": local_runtime})
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
            let timeout = if local_runtime == "docker" {
                Duration::from_secs(63 * 60)
            } else {
                Duration::from_secs(120)
            };
            let ready = wait_startup_frame(&frames, &state, &app, timeout)
                .inspect_err(|_| transport_failed = true)?;
            if ready["type"] != "ready" {
                if let Some(message) = ready["message"].as_str() {
                    failure_message = message.to_string();
                }
                return Err("supervisor did not become ready".into());
            }
            if persistent {
                *state.profile.lock().unwrap() = if local_runtime == "docker" {
                    maintenance::validated_docker_profile().ok()
                } else {
                    maintenance::validated_profile().ok()
                };
            } else {
                *state.profile.lock().unwrap() = None;
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
            let native_desktop = desktop.clone();
            let native_setup = setup.clone();
            let native_runtime = local_runtime.clone();
            let (complete, bootstrap) = mpsc::channel();
            let bootstrap_done = Arc::new(AtomicBool::new(false));
            let worker_done = bootstrap_done.clone();
            std::thread::spawn(move || {
                let app = native_app;
                let state = native_state;
                let desktop = native_desktop;
                let setup = native_setup;
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
                    desktop::grant(&app, label, &origin, !remote)?;
                    let url = origin.to_string();
                    let instance = desktop::Instance {
                        id: if remote {
                            desktop
                                .by_url(&url)
                                .map(|item| item.id)
                                .unwrap_or_else(|| uuid::Uuid::new_v4().to_string())
                        } else {
                            desktop::LOCAL_ID.into()
                        },
                        name: if remote {
                            remote_name.clone()
                        } else if docker {
                            desktop::LOCAL_DOCKER_NAME.into()
                        } else if persistent {
                            "Este computador (piloto persistente isolado)".into()
                        } else {
                            desktop::LOCAL_NAME.into()
                        },
                        mode: if remote { "existing" } else { "local" }.into(),
                        runtime: if remote {
                            "native".into()
                        } else {
                            native_runtime.clone()
                        },
                        url: url.clone(),
                    };
                    let mut config = app.config().app.windows[0].clone();
                    config.label = label.into();
                    config.visible = false;
                    config.url = tauri::WebviewUrl::External(Url::parse("about:blank")?);
                    // A switch reuses the label: the previous window must be gone first.
                    if let Some(previous) = app.get_webview_window(label) {
                        let _ = previous.destroy();
                        let deadline = Instant::now() + Duration::from_secs(2);
                        while app.get_webview_window(label).is_some() && Instant::now() < deadline {
                            std::thread::sleep(Duration::from_millis(25));
                        }
                        if app.get_webview_window(label).is_some() {
                            return Err("previous window still alive".into());
                        }
                    }
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
                .initialization_script(desktop::bridge_script())
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
                    let closing_desktop = desktop.clone();
                    let closing_setup = setup.clone();
                    let closing_label = label;
                    window.on_window_event(move |event| {
                        if let tauri::WindowEvent::CloseRequested { api, .. } = event {
                            api.prevent_close();
                            if tray::enabled() {
                                let _ = closing_window.hide();
                                return;
                            }
                            closing_setup.request_close();
                            closing_desktop.clear(closing_label);
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
                    } else if persistent {
                        println!(
                            "pilot: native cookie verified; opening persistent isolated loopback UI"
                        );
                    } else {
                        println!("pilot: native cookie verified; opening temporary loopback UI");
                    }
                    initial.store(false, Ordering::SeqCst);
                    let previous = desktop.replace(desktop::Target {
                        instance: instance.clone(),
                        label: label.into(),
                        origin: origin.clone(),
                    });
                    window.navigate(if remote {
                        origin
                    } else {
                        origin.join("index.html")?
                    })?;
                    window.show()?;
                    let active = setup.active.lock().unwrap();
                    if state.stopping.load(Ordering::SeqCst) || setup.closing.load(Ordering::SeqCst)
                    {
                        return Err("startup cancelled before commit".into());
                    }
                    // Only a connection that reached the UI is worth remembering.
                    desktop
                        .remember(instance)
                        .map_err(|_| "failed to save environment")?;
                    setup.connected.store(true, Ordering::SeqCst);
                    drop(active);
                    // The product keeps a single window: drop the replaced one.
                    if let Some(previous) = previous
                        && previous.label != label
                        && let Some(window) = app.get_webview_window(&previous.label)
                    {
                        let _ = window.destroy();
                    }
                    Ok(())
                })();
                if result.is_err() {
                    desktop.clear(label);
                }
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
                } else if docker {
                    "Piloto Docker persistente isolado pronto. Dados separados do Electron."
                } else if persistent {
                    "Piloto persistente isolado pronto. Dados separados do Electron."
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
            desktop.clear(label);
            state.stop();
        }
        // A terminal frame or EOF closes the UI; never reconnect to this port.
        let normal_stop = if startup.is_ok() {
            wait_frame(&frames, &state, None, shutdown_grace(&state))
                .inspect_err(|_| transport_failed = true)
                .is_ok_and(|frame| {
                    frame["type"] == "stopped"
                        && frame["mode"] == "graceful"
                        && frame["cleanup"] == !persistent
                })
        } else {
            false
        };
        state.stop();
        if let Some(window) = app.get_webview_window(label) {
            let _ = window.hide();
        }
        if transport_failed {
            owned.force_stop()?;
        }
        let deadline =
            state.stop_at.lock().unwrap().unwrap_or_else(Instant::now) + shutdown_grace(&state);
        while !owned.has_exited()? {
            if transport_failed || Instant::now() >= deadline {
                owned.force_stop()?;
                break;
            }
            std::thread::sleep(Duration::from_millis(25));
        }
        let status = owned.finish_checked()?;
        if !status.success() || startup.is_err() || !normal_stop {
            return Err("pilot startup or shutdown failed".into());
        }
        Ok(())
    })();
    // A cancelled native worker must settle before another attempt can reuse its label.
    if let Some(pending) = pending_bootstrap {
        setup::show_failure(
            &app,
            &setup,
            "A janela anterior ainda está encerrando. Aguarde antes de tentar novamente.",
        );
        loop {
            match pending.recv_timeout(Duration::from_millis(100)) {
                Ok(_) | Err(RecvTimeoutError::Disconnected) => break,
                Err(RecvTimeoutError::Timeout) if setup.closing.load(Ordering::SeqCst) => break,
                Err(RecvTimeoutError::Timeout) => (),
            }
        }
    }
    desktop.clear(label);
    if !setup.closing.load(Ordering::SeqCst) {
        if let Some(window) = app.get_webview_window(label) {
            let _ = window.destroy();
            let deadline = Instant::now() + Duration::from_secs(2);
            while app.get_webview_window(label).is_some() && Instant::now() < deadline {
                std::thread::sleep(Duration::from_millis(25));
            }
            if app.get_webview_window(label).is_some() {
                setup::show_failure(
                    &app,
                    &setup,
                    "A janela anterior ainda está encerrando. Aguarde antes de tentar novamente.",
                );
                while app.get_webview_window(label).is_some()
                    && !setup.closing.load(Ordering::SeqCst)
                {
                    std::thread::sleep(Duration::from_millis(100));
                }
            }
        }
    }
    *state.shutdown.lock().unwrap() = Some(result.is_ok());
    if setup.finish_attempt(opened, &state) {
        if !state.superseded.load(Ordering::SeqCst) {
            setup::show_failure(&app, &setup, &failure_message);
        }
        let _ = complete.send(serde_json::json!({"ok": false, "cancelled": state.cancelled.load(Ordering::SeqCst), "error": failure_message}));
        return;
    }
    if result.is_err() {
        *setup.last_error.lock().unwrap() = Some(failure_message.clone());
        eprintln!("pilot: startup or shutdown failed; no reconnect");
    }
    // Startup checks confirm fixture cleanup and own their final exit status.
    if startup_check::enabled() {
        return;
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

fn persistent_local() -> bool {
    let args: Vec<_> = std::env::args().collect();
    args.iter().any(|arg| arg == "--persistent-local")
        && !startup_check::enabled()
        && !args.iter().any(|arg| {
            arg == "--smoke-test" || arg.ends_with("-smoke-test") || arg == "--native-death-test"
        })
}

fn main() {
    layout::configure().expect("failed to configure isolated pilot paths");
    let file_dialog_smoke =
        std::env::args().any(|arg| arg == "--maintenance-file-dialog-smoke-test");
    if file_dialog_smoke
        && (!layout::base().join("portable-test.json").is_file()
            || layout::base().join("installer.json").is_file())
    {
        eprintln!("pilot: native file dialog smoke requires an isolated portable fixture");
        std::process::exit(2);
    }
    match import_backup::requested_archive() {
        Ok(Some(archive)) => {
            if let Err(error) = import_backup::run(&archive) {
                eprintln!("pilot: {error}");
                std::process::exit(1);
            }
            return;
        }
        Ok(None) => {}
        Err(error) => {
            eprintln!("pilot: {error}");
            std::process::exit(1);
        }
    }
    #[cfg(windows)]
    let mut primary = {
        let isolated = startup_check::enabled()
            || persistence_acceptance::enabled()
            || std::env::args().any(|arg| {
                arg == "--smoke-test"
                    || arg.ends_with("-smoke-test")
                    || arg == "--native-death-test"
            });
        if isolated {
            None
        } else {
            match single_instance::Primary::acquire() {
                Ok(Some(primary)) => Some(primary),
                Ok(None) => return,
                Err(error) => {
                    eprintln!("pilot: single-instance admission failed: {error}");
                    std::process::exit(1);
                }
            }
        }
    };
    let state = Arc::new(setup::Setup::default());
    let check = startup_check::prepare().expect("failed to prepare disposable startup check");
    let desktop = Arc::new(
        check
            .as_ref()
            .map(|check| check.desktop())
            .unwrap_or_else(|| {
                let mut desktop = desktop::Desktop::default();
                if std::env::args().any(|arg| {
                    arg == "--smoke-test"
                        || arg.ends_with("-smoke-test")
                        || arg == "--native-death-test"
                }) {
                    desktop.directory =
                        layout::data().join(format!("desktop-smoke-{}", uuid::Uuid::new_v4()));
                }
                desktop
            }),
    );
    let app = tauri::Builder::default()
        .runtime(tauri_runtime_wry::Wry::default())
        .manage(state.clone())
        .manage(desktop)
        .manage(Arc::new(preparation::Preparation::default()))
        .manage(Arc::new(maintenance::Maintenance::default()))
        .invoke_handler(tauri::generate_handler![
            setup::setup_state,
            setup::setup_submit,
            setup::setup_cancel,
            setup::setup_docker,
            desktop::desktop_state,
            desktop::desktop_appearance,
            desktop::desktop_menu,
            preparation::preparation_state,
            preparation::preparation_start,
            preparation::preparation_cancel,
            preparation::desktop_prepare,
            maintenance::desktop_maintain,
            maintenance::maintenance_state,
            maintenance::maintenance_start,
            maintenance::maintenance_cancel
        ])
        .on_menu_event(|app, event| {
            let id = event.id().as_ref().to_string();
            if id == tray::OPEN_ID {
                tray::reopen(app);
                return;
            }
            if id == tray::QUIT_ID {
                app.exit(0);
                return;
            }
            desktop::menu_event(app, &id);
        })
        .setup(move |app| {
            tray::create(app.handle())?;
            let desktop = app.state::<Arc<desktop::Desktop>>().inner().clone();
            desktop.load();
            // The smokes start from a known list: a saved environment must not
            // rewrite the setup form before the test fills it.
            if std::env::args()
                .any(|arg| arg.ends_with("-smoke-test") || arg == "--native-death-test")
            {
                desktop
                    .reset()
                    .map_err(|_| "failed to reset pilot environments")?;
            }
            setup::open(app.handle())?;
            if std::env::args().any(|arg| {
                arg == "--maintenance-smoke-test" || arg == "--maintenance-file-dialog-smoke-test"
            }) {
                let handle = app.handle().clone();
                std::thread::spawn(move || maintenance_smoke::run(handle, file_dialog_smoke));
                return Ok(());
            }
            if let Some(instance) = desktop.startup_instance(std::env::args()) {
                setup::switch(app.handle(), instance);
            }
            if let Some(check) = check {
                let handle = app.handle().clone();
                let state = app.state::<Arc<setup::Setup>>().inner().clone();
                std::thread::spawn(move || check.run(handle, state));
            }
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
            if std::env::args().any(|arg| arg == "--switch-smoke-test") {
                let handle = app.handle().clone();
                let state = app.state::<Arc<setup::Setup>>().inner().clone();
                std::thread::spawn(move || switch_smoke::run(handle, state));
            }
            if persistence_acceptance::enabled() {
                let handle = app.handle().clone();
                let state = app.state::<Arc<setup::Setup>>().inner().clone();
                std::thread::spawn(move || persistence_acceptance::run(handle, state));
            }
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to run the isolated Persona Tauri pilot");
    #[cfg(windows)]
    if let Some(primary) = primary.as_mut() {
        primary.listen(app.handle().clone());
    }
    app.run(move |_, event| {
        if let tauri::RunEvent::ExitRequested { api, .. } = event {
            if state.defer_maintenance_exit() {
                api.prevent_exit();
                return;
            }
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
            let deadline = startup.then(|| Instant::now() + Duration::from_secs(5));
            assert!(wait_frame(&frames, &state, deadline, Duration::from_millis(100)).is_err());
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
                    (phase == "before").then(|| Instant::now() + Duration::from_secs(5)),
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
