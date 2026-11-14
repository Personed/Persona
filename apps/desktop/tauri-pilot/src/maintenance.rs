use std::{
    io::{BufRead, BufReader, Read, Write},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
        mpsc,
    },
    time::Duration,
};
use tauri::{Manager, Url};

const FAILED: &str = "A manutenção não foi confirmada. O arquivo ou a seleção de perfil pode ter mudado. Verifique o resultado antes de tentar novamente; o serviço não será reiniciado automaticamente.";

fn valid_profile_name(name: &str) -> bool {
    name == "pilot-local-v1"
        || name
            .strip_prefix("pilot-local-restore-")
            .is_some_and(|suffix| {
                uuid::Uuid::parse_str(suffix).is_ok_and(|id| {
                    id.to_string() == suffix
                        && id.get_version_num() == 4
                        && id.get_variant() == uuid::Variant::RFC4122
                })
            })
}

fn valid_docker_profile_name(name: &str) -> bool {
    name == "pilot-docker-v1"
        || name
            .strip_prefix("pilot-docker-restore-")
            .is_some_and(|suffix| {
                uuid::Uuid::parse_str(suffix).is_ok_and(|id| {
                    id.to_string() == suffix
                        && id.get_version_num() == 4
                        && id.get_variant() == uuid::Variant::RFC4122
                })
            })
}

fn selected_profile(data: &std::path::Path) -> Result<std::path::PathBuf, String> {
    let pointer = data.join("pilot-local-current.json");
    match std::fs::symlink_metadata(&pointer) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(data.join("pilot-local-v1"));
        }
        Err(_) => return Err("Seleção de perfil indisponível.".into()),
        Ok(_) => no_links(&pointer)?,
    }
    let mut bytes = Vec::new();
    std::fs::File::open(pointer)
        .map_err(|_| "Seleção de perfil indisponível.")?
        .take(257)
        .read_to_end(&mut bytes)
        .map_err(|_| "Seleção de perfil inválida.")?;
    if bytes.len() > 256 {
        return Err("Seleção de perfil inválida.".into());
    }
    let value: serde_json::Value =
        serde_json::from_slice(&bytes).map_err(|_| "Seleção de perfil inválida.")?;
    let name = value["profile"]
        .as_str()
        .filter(|name| valid_profile_name(name))
        .ok_or("Nome de perfil inválido.")?;
    if value.as_object().is_none_or(|object| object.len() != 1) {
        return Err("Seleção de perfil inválida.".into());
    }
    Ok(data.join(name))
}

fn selected_docker_profile(data: &std::path::Path) -> Result<std::path::PathBuf, String> {
    let pointer = data.join("pilot-docker-current.json");
    match std::fs::symlink_metadata(&pointer) {
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Ok(data.join("pilot-docker-v1"));
        }
        Err(_) => return Err("Seleção de perfil Docker indisponível.".into()),
        Ok(_) => no_links(&pointer)?,
    }
    let mut bytes = Vec::new();
    std::fs::File::open(pointer)
        .map_err(|_| "Seleção de perfil Docker indisponível.")?
        .take(257)
        .read_to_end(&mut bytes)
        .map_err(|_| "Seleção de perfil Docker inválida.")?;
    if bytes.len() > 256 {
        return Err("Seleção de perfil Docker inválida.".into());
    }
    let value: serde_json::Value =
        serde_json::from_slice(&bytes).map_err(|_| "Seleção de perfil Docker inválida.")?;
    let name = value["profile"]
        .as_str()
        .filter(|name| valid_docker_profile_name(name))
        .ok_or("Nome de perfil Docker inválido.")?;
    if value.as_object().is_none_or(|object| object.len() != 1) {
        return Err("Seleção de perfil Docker inválida.".into());
    }
    Ok(data.join(name))
}

fn profile_lock(profile: &std::path::Path) -> Result<std::path::PathBuf, String> {
    let name = profile
        .file_name()
        .and_then(|name| name.to_str())
        .filter(|name| valid_profile_name(name))
        .ok_or("Nome de perfil inválido.")?;
    Ok(profile.with_file_name(format!("{name}.lock")))
}

fn docker_profile_lock(profile: &std::path::Path) -> Result<std::path::PathBuf, String> {
    let name = profile
        .file_name()
        .and_then(|name| name.to_str())
        .filter(|name| valid_docker_profile_name(name))
        .ok_or("Nome de perfil Docker inválido.")?;
    Ok(profile.with_file_name(format!("{name}.lock")))
}

fn path_absent(path: &std::path::Path) -> Result<bool, String> {
    match std::fs::symlink_metadata(path) {
        Ok(_) => Ok(false),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(true),
        Err(_) => Err("Não foi possível confirmar a ausência do caminho.".into()),
    }
}

fn no_links(path: &std::path::Path) -> Result<(), String> {
    for ancestor in path.ancestors() {
        let metadata =
            std::fs::symlink_metadata(ancestor).map_err(|_| "Caminho do piloto indisponível.")?;
        #[cfg(windows)]
        let linked = {
            use std::os::windows::fs::MetadataExt;
            metadata.file_attributes() & 0x400 != 0
        };
        #[cfg(not(windows))]
        let linked = metadata.file_type().is_symlink();
        if linked {
            return Err("Caminho do piloto contém link ou ponto de redirecionamento.".into());
        }
    }
    Ok(())
}

fn validate_profile_paths(
    base: &std::path::Path,
    data: &std::path::Path,
    expected: &std::path::Path,
) -> Result<std::path::PathBuf, String> {
    no_links(base)?;
    no_links(data)?;
    no_links(expected)?;
    if std::fs::canonicalize(data).map_err(|_| "Dados do piloto indisponíveis.")?
        != std::fs::canonicalize(expected).map_err(|_| "Destino do worker indisponível.")?
    {
        return Err("Layout do serviço e do worker não coincide; manutenção bloqueada.".into());
    }
    let profile = selected_profile(data)?;
    no_links(&profile)?;
    no_links(&profile.join("pilot-profile"))?;
    if std::fs::read(profile.join("pilot-profile")).map_err(|_| "Perfil isolado não confirmado.")?
        != b"persona-tauri-pilot-local-v1\n"
    {
        return Err("Perfil isolado não confirmado.".into());
    }
    std::fs::canonicalize(profile).map_err(|_| "Perfil isolado indisponível.".into())
}

fn validate_docker_profile_paths(
    base: &std::path::Path,
    data: &std::path::Path,
    expected: &std::path::Path,
) -> Result<std::path::PathBuf, String> {
    no_links(base)?;
    no_links(data)?;
    no_links(expected)?;
    if std::fs::canonicalize(data).map_err(|_| "Dados do piloto indisponíveis.")?
        != std::fs::canonicalize(expected).map_err(|_| "Destino do worker indisponível.")?
    {
        return Err("Layout do serviço e do worker não coincide; manutenção bloqueada.".into());
    }
    let profile = selected_docker_profile(data)?;
    no_links(&profile)?;
    no_links(&profile.join("pilot-profile"))?;
    if std::fs::read(profile.join("pilot-profile"))
        .map_err(|_| "Perfil Docker isolado não confirmado.")?
        != b"persona-tauri-pilot-docker-v1\n"
    {
        return Err("Perfil Docker isolado não confirmado.".into());
    }
    std::fs::canonicalize(profile).map_err(|_| "Perfil Docker isolado indisponível.".into())
}

pub fn validated_profile() -> Result<std::path::PathBuf, String> {
    let base = crate::layout::base();
    let expected = if base.join("installer.json").is_file() {
        std::path::PathBuf::from(
            std::env::var_os("LOCALAPPDATA").ok_or("LOCALAPPDATA indisponível.")?,
        )
        .join("app.persona.tauri-pilot")
    } else {
        base.join("target/test-data")
    };
    validate_profile_paths(&base, &crate::layout::data(), &expected)
}

pub fn validated_docker_profile() -> Result<std::path::PathBuf, String> {
    let base = crate::layout::base();
    let expected = if base.join("installer.json").is_file() {
        std::path::PathBuf::from(
            std::env::var_os("LOCALAPPDATA").ok_or("LOCALAPPDATA indisponível.")?,
        )
        .join("app.persona.tauri-pilot")
    } else {
        base.join("target/test-data")
    };
    validate_docker_profile_paths(&base, &crate::layout::data(), &expected)
}

fn worker_frame(reader: &mut impl BufRead) -> Result<Option<serde_json::Value>, ()> {
    let mut bytes = Vec::new();
    reader
        .take(16385)
        .read_until(b'\n', &mut bytes)
        .map_err(|_| ())?;
    if bytes.is_empty() {
        return Ok(None);
    }
    if bytes.len() > 16384 || bytes.last() != Some(&b'\n') {
        return Err(());
    }
    serde_json::from_slice(&bytes).map(Some).map_err(|_| ())
}

pub struct Maintenance {
    state: Mutex<serde_json::Value>,
    cancel: AtomicBool,
}

impl Default for Maintenance {
    fn default() -> Self {
        Self {
            state: Mutex::new(
                serde_json::json!({"status":"idle", "message":"Somente piloto persistente isolado no Windows. A restauração usa um novo perfil e conserva o perfil atual. Nenhuma operação começa antes da confirmação, escolha nativa do arquivo e parada verificada do serviço."}),
            ),
            cancel: AtomicBool::new(false),
        }
    }
}

impl Maintenance {
    fn update(&self, status: &str, message: &str) {
        *self.state.lock().unwrap() = serde_json::json!({"status":status,"message":message});
    }
}

fn start_frame(
    action: &str,
    archive: &std::path::Path,
    password: &str,
    runtime: &str,
) -> Result<Vec<u8>, String> {
    if !matches!(action, "backup" | "restore")
        || !matches!(runtime, "native" | "docker")
        || password.encode_utf16().count() < 12
        || !archive.is_absolute()
        || archive
            .as_os_str()
            .to_str()
            .is_none_or(|path| path.contains('\0'))
    {
        return Err("Parâmetros de manutenção inválidos.".into());
    }
    let mut bytes = serde_json::to_vec(
        &serde_json::json!({"type":"start","action":action,"archive":archive,"password":password,"runtime":runtime}),
    )
    .map_err(|_| "Parâmetros inválidos.")?;
    bytes.push(b'\n');
    if bytes.len() > 65536 {
        bytes.fill(0);
        return Err("Solicitação de manutenção muito grande.".into());
    }
    Ok(bytes)
}

#[cfg(windows)]
pub(crate) fn choose_archive(
    action: &str,
    owner: isize,
) -> Result<Option<std::path::PathBuf>, String> {
    use windows::{Win32::UI::Controls::Dialogs::*, core::PWSTR};
    let mut buffer = vec![0u16; 32768];
    let mut dialog = OPENFILENAMEW {
        lStructSize: std::mem::size_of::<OPENFILENAMEW>() as u32,
        hwndOwner: windows::Win32::Foundation::HWND(owner as *mut _),
        lpstrFile: PWSTR(buffer.as_mut_ptr()),
        nMaxFile: buffer.len() as u32,
        Flags: OFN_EXPLORER
            | OFN_NOCHANGEDIR
            | OFN_PATHMUSTEXIST
            | if action == "restore" {
                OFN_FILEMUSTEXIST
            } else {
                OFN_OVERWRITEPROMPT
            },
        ..Default::default()
    };
    let selected = unsafe {
        if action == "restore" {
            GetOpenFileNameW(&mut dialog)
        } else {
            GetSaveFileNameW(&mut dialog)
        }
    };
    if !selected.as_bool() {
        return if unsafe { CommDlgExtendedError() }.0 == 0 {
            Ok(None)
        } else {
            Err("O diálogo nativo falhou.".into())
        };
    }
    let end = buffer
        .iter()
        .position(|ch| *ch == 0)
        .ok_or("Caminho inválido.")?;
    let path = std::path::PathBuf::from(
        String::from_utf16(&buffer[..end]).map_err(|_| "Caminho inválido.")?,
    );
    if action == "backup" && !path_absent(&path)? {
        return Err("Escolha um arquivo novo. Backups existentes não serão substituídos.".into());
    }
    Ok(Some(path))
}

#[cfg(not(windows))]
pub(crate) fn choose_archive(_: &str, _: isize) -> Result<Option<std::path::PathBuf>, String> {
    Err("Manutenção disponível somente no piloto Windows; encerramento de descendentes não verificado nesta plataforma.".into())
}

fn worker(
    frame: &mut [u8],
    state: &Maintenance,
    setup: &crate::setup::Setup,
) -> Result<(), String> {
    let mut owned =
        crate::process::OwnedProcess::spawn(&crate::layout::base().join("maintenance-worker.mjs"))
            .map_err(|_| FAILED)?;
    let mut input = owned.child.stdin.take().ok_or(FAILED)?;
    let output = owned.child.stderr.take().ok_or(FAILED)?;
    let (send, frames) = mpsc::sync_channel(8);
    let reader = std::thread::spawn(move || {
        let mut reader = BufReader::new(output);
        loop {
            let frame = worker_frame(&mut reader);
            let failed = !matches!(frame, Ok(Some(_)));
            if send.send(frame).is_err() || failed {
                break;
            }
        }
    });
    if !matches!(frames.recv_timeout(Duration::from_secs(10)), Ok(Ok(Some(frame))) if frame == serde_json::json!({"type":"ready"}))
    {
        return Err(FAILED.into());
    }
    if state.cancel.load(Ordering::SeqCst) || setup.maintenance_exit_requested() {
        return Err("Manutenção cancelada antes do início.".into());
    }
    state.update("running", "Manutenção iniciada. Cancelamento indisponível; aguardando resultado e saída reais do worker.");
    // A partial write may already dispatch work: retain ownership until actual exit even on I/O failure.
    let sent = input.write_all(frame).is_ok();
    frame.fill(0);
    drop(input);
    let mut result = None;
    let mut valid = sent;
    loop {
        match frames.recv_timeout(Duration::from_millis(25)) {
            Ok(Ok(Some(frame)))
                if frame["type"] == "result" && result.is_none() && frame["ok"].is_boolean() =>
            {
                result = Some(frame["ok"] == true)
            }
            Ok(Ok(None)) => {}
            Ok(_) => valid = false,
            Err(mpsc::RecvTimeoutError::Disconnected) => {}
            Err(mpsc::RecvTimeoutError::Timeout) => {}
        }
        // Never drop or kill a dispatched worker on timeout, cancellation, or protocol failure.
        match owned.has_finished_without_termination() {
            Ok(true) => break,
            Ok(false) | Err(_) => std::thread::sleep(Duration::from_millis(25)),
        }
    }
    // Drain while the bounded reader settles, so a full queue cannot deadlock join.
    while !reader.is_finished() {
        match frames.recv_timeout(Duration::from_millis(25)) {
            Ok(Ok(Some(frame)))
                if frame["type"] == "result" && result.is_none() && frame["ok"].is_boolean() =>
            {
                result = Some(frame["ok"] == true)
            }
            Ok(Ok(None))
            | Err(mpsc::RecvTimeoutError::Timeout)
            | Err(mpsc::RecvTimeoutError::Disconnected) => {}
            _ => valid = false,
        }
    }
    if reader.join().is_err() {
        valid = false;
    }
    while let Ok(frame) = frames.try_recv() {
        if let Ok(Some(frame)) = frame {
            if frame["type"] == "result" && result.is_none() {
                result = Some(frame["ok"] == true);
            } else {
                valid = false;
            }
        } else if frame.is_err() {
            valid = false;
        }
    }
    let status = owned.finish_checked().map_err(|_| FAILED)?;
    if valid && result == Some(true) && status.success() {
        Ok(())
    } else {
        Err(FAILED.into())
    }
}

fn url() -> Url {
    #[cfg(windows)]
    let origin = "http://tauri.localhost/maintenance.html";
    #[cfg(not(windows))]
    let origin = "tauri://localhost/maintenance.html";
    Url::parse(origin).unwrap()
}

fn trusted(label: &str, origin: &Url) -> bool {
    label == "maintenance" && origin == &url()
}

fn check(window: &tauri::WebviewWindow) -> Result<(), String> {
    if !trusted(
        window.label(),
        &window.url().map_err(|_| "Origem inválida.")?,
    ) {
        return Err("Origem de manutenção inválida.".into());
    }
    Ok(())
}

pub fn open(app: &tauri::AppHandle) -> Result<(), String> {
    if let Some(window) = app.get_webview_window("maintenance") {
        window
            .show()
            .map_err(|_| "Não foi possível abrir a manutenção.")?;
        window.set_focus().ok();
        return Ok(());
    }
    let expected = url();
    let state = app.state::<Arc<Maintenance>>().inner().clone();
    let window = tauri::WebviewWindowBuilder::new(
        app,
        "maintenance",
        tauri::WebviewUrl::App("maintenance.html".into()),
    )
    .title("Backup e restauração — piloto isolado")
    .inner_size(620.0, 760.0)
    .min_inner_size(360.0, 540.0)
    .incognito(true)
    .disable_drag_drop_handler()
    .on_navigation(move |target| target == &expected)
    .on_new_window(|_, _| tauri::webview::NewWindowResponse::Deny)
    .on_permission_request(|_, _| tauri::webview::PermissionResponse::Deny)
    .build()
    .map_err(|_| "Não foi possível abrir a manutenção.".to_string())?;
    window.on_window_event(move |event| {
        if let tauri::WindowEvent::CloseRequested { .. } = event {
            state.cancel.store(true, Ordering::SeqCst);
        }
    });
    Ok(())
}

#[tauri::command]
pub fn maintenance_state(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, Arc<Maintenance>>,
) -> Result<serde_json::Value, String> {
    check(&window)?;
    Ok(serde_json::json!({"ok": true, "value": state.state.lock().unwrap().clone()}))
}

#[tauri::command]
pub fn maintenance_start(
    window: tauri::WebviewWindow,
    password: String,
    action: String,
    state: tauri::State<'_, Arc<Maintenance>>,
    setup: tauri::State<'_, Arc<crate::setup::Setup>>,
) -> Result<serde_json::Value, String> {
    check(&window)?;
    if !cfg!(windows)
        || !matches!(action.as_str(), "backup" | "restore")
        || password.encode_utf16().count() < 12
    {
        return Err(
            "Manutenção exige Windows, ação válida e senha de pelo menos 12 caracteres.".into(),
        );
    }
    #[cfg(windows)]
    let owner = window
        .hwnd()
        .map_err(|_| "Janela de confirmação indisponível.")?
        .0 as isize;
    #[cfg(not(windows))]
    let owner = 0;
    let supervisor = setup.reserve_maintenance()?;
    let docker = supervisor.docker.load(Ordering::SeqCst);
    let profile = match (if docker {
        validated_docker_profile()
    } else {
        validated_profile()
    })
    .and_then(|profile| {
        let lock = if docker {
            docker_profile_lock(&profile)?
        } else {
            profile_lock(&profile)?
        };
        no_links(&lock)?;
        if !lock.is_dir() {
            return Err("Lock do serviço persistente não confirmado.".into());
        }
        Ok(profile)
    }) {
        Ok(profile) if supervisor.profile.lock().unwrap().as_ref() == Some(&profile) => profile,
        _ => {
            supervisor.superseded.store(false, Ordering::SeqCst);
            if setup.finish_maintenance() {
                window.app_handle().exit(1);
            }
            return Err("Perfil persistente ou layout do worker não confirmado. Nenhuma manutenção foi iniciada.".into());
        }
    };
    state.cancel.store(false, Ordering::SeqCst);
    state.update(
        "awaiting_file",
        "Escolha um arquivo no diálogo nativo. Nenhum perfil existente será substituído.",
    );
    let app = window.app_handle().clone();
    let state = state.inner().clone();
    let setup = setup.inner().clone();
    std::thread::spawn(move || {
        let outcome = (|| -> Result<(), String> {
            let archive = choose_archive(&action, owner)?
                .ok_or("Seleção cancelada; nenhum arquivo alterado.")?;
            let archive_parent = archive.parent().ok_or("Caminho de backup inválido.")?;
            no_links(archive_parent)?;
            if std::fs::canonicalize(archive_parent)
                .map_err(|_| "Destino indisponível.")?
                .starts_with(&profile)
            {
                return Err("Salve o backup fora do perfil do piloto.".into());
            }
            let mut frame = start_frame(
                &action,
                &archive,
                &password,
                if docker { "docker" } else { "native" },
            )?;
            drop(password);
            if state.cancel.load(Ordering::SeqCst) || setup.maintenance_exit_requested() {
                frame.fill(0);
                return Err("Manutenção cancelada antes do início.".into());
            }
            state.update(
                "running",
                "Encerrando serviço normalmente antes de acessar o perfil…",
            );
            supervisor.stop();
            if !setup.wait_idle(Duration::from_secs(if docker { 100 } else { 50 })) {
                frame.fill(0);
                state.update("running", "Encerramento ainda não confirmado. Aguardando o serviço; nenhum worker foi iniciado.");
                while !setup.wait_idle(Duration::from_secs(1)) {}
                return Err(
                    "O serviço excedeu o prazo de encerramento; nenhuma manutenção foi iniciada."
                        .into(),
                );
            }
            if *supervisor.shutdown.lock().unwrap() != Some(true) {
                frame.fill(0);
                return Err(
                    "Shutdown normal não confirmado. Nenhum worker de manutenção foi iniciado."
                        .into(),
                );
            }
            let current_profile = if docker {
                validated_docker_profile()?
            } else {
                validated_profile()?
            };
            let lock = if docker {
                docker_profile_lock(&profile)?
            } else {
                profile_lock(&profile)?
            };
            if current_profile != profile || !path_absent(&lock)? {
                frame.fill(0);
                return Err(
                    "Perfil mudou ou lock permanece após shutdown; manutenção bloqueada.".into(),
                );
            }
            if state.cancel.load(Ordering::SeqCst) || setup.maintenance_exit_requested() {
                frame.fill(0);
                return Err("Manutenção cancelada antes do início.".into());
            }
            let outcome = worker(&mut frame, &state, &setup);
            frame.fill(0);
            outcome
        })();
        let succeeded = outcome.is_ok();
        match outcome {
            Ok(()) => state.update(
                "success",
                "Manutenção concluída. Serviço parado; abra o piloto local novamente para iniciar.",
            ),
            Err(message) => state.update("error", &message),
        }
        if !supervisor.stopping.load(Ordering::SeqCst) {
            supervisor.superseded.store(false, Ordering::SeqCst);
        }
        if supervisor.shutdown.lock().unwrap().is_some() {
            if let Some(window) = app.get_webview_window("setup") {
                let _ = window.show();
            }
        }
        if setup.finish_maintenance() {
            app.exit(if succeeded { 0 } else { 1 });
        }
    });
    Ok(serde_json::json!({"ok": true}))
}

#[tauri::command]
pub fn maintenance_cancel(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, Arc<Maintenance>>,
) -> Result<serde_json::Value, String> {
    check(&window)?;
    state.cancel.store(true, Ordering::SeqCst);
    state.state.lock().unwrap()["message"] = serde_json::json!(
        "Cancelamento solicitado somente antes do envio. Se o worker já iniciou, aguarde seu resultado real; ele não será interrompido."
    );
    Ok(serde_json::json!({"ok": true}))
}

#[tauri::command]
pub async fn desktop_maintain(
    window: tauri::WebviewWindow,
    desktop: tauri::State<'_, std::sync::Arc<crate::desktop::Desktop>>,
) -> Result<serde_json::Value, String> {
    let target = desktop.check(&window)?;
    if target.instance.mode != "local" {
        return Err("Manutenção disponível somente para o piloto local.".into());
    }
    open(window.app_handle())?;
    Ok(serde_json::json!({"ok": true, "value": {"opened": true, "started": false}}))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn profile_layout_requires_same_worker_destination_and_pilot_marker() {
        let base = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target/test-data")
            .join(format!("maintenance-paths-{}", uuid::Uuid::new_v4()));
        let data = base.join("target/test-data");
        let profile = data.join("pilot-local-v1");
        std::fs::create_dir_all(&profile).unwrap();
        assert!(!path_absent(&profile).unwrap());
        assert!(path_absent(&profile_lock(&profile).unwrap()).unwrap());
        assert!(validate_profile_paths(&base, &data, &data).is_err());
        std::fs::write(profile.join("pilot-profile"), b"not a pilot").unwrap();
        assert!(validate_profile_paths(&base, &data, &data).is_err());
        std::fs::write(
            profile.join("pilot-profile"),
            b"persona-tauri-pilot-local-v1\n",
        )
        .unwrap();
        assert_eq!(
            validate_profile_paths(&base, &data, &data).unwrap(),
            std::fs::canonicalize(&profile).unwrap()
        );
        let mismatch = base.join("different-worker-data");
        std::fs::create_dir(&mismatch).unwrap();
        assert!(validate_profile_paths(&base, &data, &mismatch).is_err());
        let name = format!("pilot-local-restore-{}", uuid::Uuid::new_v4());
        let restored = data.join(&name);
        std::fs::create_dir(&restored).unwrap();
        std::fs::write(
            restored.join("pilot-profile"),
            b"persona-tauri-pilot-local-v1\n",
        )
        .unwrap();
        let pointer = data.join("pilot-local-current.json");
        std::fs::write(&pointer, serde_json::json!({"profile":name}).to_string()).unwrap();
        assert_eq!(
            validate_profile_paths(&base, &data, &data).unwrap(),
            std::fs::canonicalize(&restored).unwrap()
        );
        assert_eq!(
            profile_lock(&restored).unwrap(),
            data.join(format!("{name}.lock"))
        );
        assert!(profile.exists());
        for invalid in [
            "../pilot-local-v1",
            "pilot-local-restore-00000000-0000-1000-8000-000000000000",
            "pilot-local-restore-00000000-0000-4000-0000-000000000000",
            "pilot-local-restore-AAAAAAAA-0000-4000-8000-000000000000",
            "pilot-local-restore-00000000000040008000000000000000",
        ] {
            std::fs::write(&pointer, serde_json::json!({"profile":invalid}).to_string()).unwrap();
            assert!(
                validate_profile_paths(&base, &data, &data).is_err(),
                "{invalid}"
            );
        }
        std::fs::write(&pointer, b"invalid json").unwrap();
        assert!(validate_profile_paths(&base, &data, &data).is_err());
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn docker_profile_selection_uses_its_own_pointer_names_and_marker() {
        let base = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("target/test-data")
            .join(format!("docker-maintenance-paths-{}", uuid::Uuid::new_v4()));
        let data = base.join("data");
        let profile = data.join("pilot-docker-v1");
        std::fs::create_dir_all(&profile).unwrap();
        std::fs::write(
            profile.join("pilot-profile"),
            b"persona-tauri-pilot-docker-v1\n",
        )
        .unwrap();
        assert_eq!(
            validate_docker_profile_paths(&base, &data, &data).unwrap(),
            std::fs::canonicalize(&profile).unwrap()
        );
        assert!(path_absent(&docker_profile_lock(&profile).unwrap()).unwrap());

        let name = format!("pilot-docker-restore-{}", uuid::Uuid::new_v4());
        let restored = data.join(&name);
        std::fs::create_dir(&restored).unwrap();
        std::fs::write(
            restored.join("pilot-profile"),
            b"persona-tauri-pilot-docker-v1\n",
        )
        .unwrap();
        let pointer = data.join("pilot-docker-current.json");
        std::fs::write(&pointer, serde_json::json!({"profile":name}).to_string()).unwrap();
        assert_eq!(
            validate_docker_profile_paths(&base, &data, &data).unwrap(),
            std::fs::canonicalize(&restored).unwrap()
        );
        assert_eq!(
            docker_profile_lock(&restored).unwrap(),
            data.join(format!("{name}.lock"))
        );
        for invalid in [
            "pilot-local-v1",
            "../pilot-docker-v1",
            "pilot-docker-restore-00000000-0000-1000-8000-000000000000",
        ] {
            std::fs::write(&pointer, serde_json::json!({"profile":invalid}).to_string()).unwrap();
            assert!(validate_docker_profile_paths(&base, &data, &data).is_err());
        }
        std::fs::remove_dir_all(base).unwrap();
    }

    #[test]
    fn worker_eof_is_distinct_from_malformed_or_truncated_output() {
        assert!(matches!(
            worker_frame(&mut BufReader::new(&b""[..])),
            Ok(None)
        ));
        for bytes in [b"bad\n".as_slice(), b"{}".as_slice()] {
            assert!(worker_frame(&mut BufReader::new(bytes)).is_err());
        }
        assert!(worker_frame(&mut BufReader::new(&vec![b' '; 16385][..])).is_err());
        let mut reader = BufReader::new(&b"{\"type\":\"result\",\"ok\":true}\ninvalid\n"[..]);
        assert_eq!(worker_frame(&mut reader).unwrap().unwrap()["ok"], true);
        assert!(worker_frame(&mut reader).is_err());
    }

    #[test]
    fn private_start_frame_is_bounded_and_counts_utf16_like_worker() {
        let archive = std::env::current_dir().unwrap().join("synthetic-backup");
        let bytes = start_frame("backup", &archive, "🔒🔒🔒🔒🔒🔒", "native").unwrap();
        assert_eq!(bytes.last(), Some(&b'\n'));
        let frame: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        assert_eq!(frame.as_object().unwrap().len(), 5);
        assert_eq!(frame["type"], "start");
        assert_eq!(frame["archive"], archive.to_str().unwrap());
        assert_eq!(frame["runtime"], "native");
        assert!(start_frame("restore", &archive, "123456789012", "docker").is_ok());
        assert!(start_frame("backup", &archive, "123456789012", "remote").is_err());
        assert!(start_frame("backup", &archive, "short", "native").is_err());
        assert!(start_frame("delete", &archive, "123456789012", "native").is_err());
        assert!(
            start_frame(
                "restore",
                std::path::Path::new("relative"),
                "123456789012",
                "native"
            )
            .is_err()
        );
        assert!(start_frame("backup", &archive, &"a".repeat(65536), "native").is_err());
    }

    #[test]
    fn maintenance_requires_packaged_label_and_exact_document() {
        assert!(trusted("maintenance", &url()));
        assert!(!trusted("pilot", &url()));
        for origin in [
            "http://127.0.0.1:1234/maintenance.html",
            "https://example.com/maintenance.html",
            "http://tauri.localhost/maintenance.html?start=true",
            "tauri://localhost/setup.html",
        ] {
            assert!(!trusted("maintenance", &Url::parse(origin).unwrap()));
        }
        assert!(!trusted("maintenance", &url().join("setup.html").unwrap()));
    }
}
