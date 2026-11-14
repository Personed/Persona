use crate::{process::OwnedProcess, setup::Setup};
use std::{
    io::{BufReader, Write},
    sync::{Arc, Mutex, atomic::Ordering, mpsc},
    time::{Duration, Instant},
};
use tauri::{Manager, Url};

#[derive(Clone, Copy, PartialEq, Eq)]
enum Status {
    Idle,
    Running,
    Cancelling,
    Success,
    Error,
    Cancelled,
}

impl Status {
    fn terminal(self) -> bool {
        matches!(self, Self::Success | Self::Error | Self::Cancelled)
    }

    fn name(self) -> &'static str {
        match self {
            Self::Idle => "idle",
            Self::Running => "running",
            Self::Cancelling => "cancelling",
            Self::Success => "success",
            Self::Error => "error",
            Self::Cancelled => "cancelled",
        }
    }
}

struct Operation {
    id: String,
    status: Status,
    message: String,
    progress: Option<serde_json::Value>,
    uncertain: bool,
    cancel: Option<mpsc::Sender<()>>,
    cancel_receiver: Option<mpsc::Receiver<()>>,
    result: Option<mpsc::Sender<serde_json::Value>>,
}

#[derive(Default)]
pub struct Preparation {
    operation: Mutex<Option<Operation>>,
}

impl Preparation {
    fn begin(&self, result: mpsc::Sender<serde_json::Value>) -> Result<String, String> {
        let mut operation = self.operation.lock().unwrap();
        if operation
            .as_ref()
            .is_some_and(|item| !item.status.terminal())
        {
            return Err("Aguarde a operação atual terminar.".into());
        }
        let (cancel, cancel_receiver) = mpsc::channel();
        let id = uuid::Uuid::new_v4().to_string();
        *operation = Some(Operation {
            id: id.clone(),
            status: Status::Idle,
            message: "Pronto para preparar as imagens do Docker.".into(),
            progress: None,
            uncertain: false,
            cancel: Some(cancel),
            cancel_receiver: Some(cancel_receiver),
            result: Some(result),
        });
        Ok(id)
    }

    fn start(&self, app: tauri::AppHandle, setup: Arc<Setup>) -> Result<(), String> {
        let (id, receiver) = {
            let mut operation = self.operation.lock().unwrap();
            let operation = operation
                .as_mut()
                .ok_or("Nenhuma preparação está pendente.")?;
            if operation.status != Status::Idle {
                return Err("A preparação já foi iniciada.".into());
            }
            operation.status = Status::Running;
            operation.message = "Preparando as imagens do Docker…".into();
            (
                operation.id.clone(),
                operation
                    .cancel_receiver
                    .take()
                    .ok_or("Cancelamento indisponível.")?,
            )
        };
        let preparation = app.state::<Arc<Preparation>>().inner().clone();
        std::thread::spawn(move || {
            let outcome = run_worker(preparation.clone(), setup, id.clone(), receiver);
            preparation.finish(
                &id,
                outcome.status,
                outcome.message,
                outcome.value,
                outcome.uncertain,
            );
        });
        Ok(())
    }

    fn cancel(&self) -> Result<(), String> {
        let mut operation = self.operation.lock().unwrap();
        let operation = operation
            .as_mut()
            .ok_or("Nenhuma preparação está pendente.")?;
        match operation.status {
            Status::Idle => {
                operation.status = Status::Cancelled;
                operation.message = "Preparação cancelada.".into();
                if let Some(result) = operation.result.take() {
                    let _ = result.send(serde_json::json!({
                        "ok": false,
                        "cancelled": true,
                        "error": "Preparação cancelada."
                    }));
                }
                Ok(())
            }
            Status::Running => {
                operation.status = Status::Cancelling;
                operation.message = "Cancelamento solicitado.".into();
                operation
                    .cancel
                    .as_ref()
                    .ok_or("Cancelamento indisponível.")?
                    .send(())
                    .map_err(|_| "A preparação já terminou.".to_string())
            }
            Status::Cancelling => Ok(()),
            Status::Success | Status::Error | Status::Cancelled => Ok(()),
        }
    }

    fn progress(&self, id: &str, phase: &str, message: &str) {
        let mut operation = self.operation.lock().unwrap();
        let Some(operation) = operation.as_mut().filter(|item| item.id == id) else {
            return;
        };
        operation.progress = Some(serde_json::json!({
            "phase": phase,
            "message": message
        }));
        operation.message = message.into();
    }

    fn mark_cancelling(&self, id: &str) {
        let mut operation = self.operation.lock().unwrap();
        let Some(operation) = operation.as_mut().filter(|item| item.id == id) else {
            return;
        };
        operation.status = Status::Cancelling;
        operation.message = "Cancelamento solicitado.".into();
    }

    fn finish(
        &self,
        id: &str,
        status: Status,
        message: impl Into<String>,
        value: serde_json::Value,
        uncertain: bool,
    ) {
        let mut operation = self.operation.lock().unwrap();
        let Some(operation) = operation.as_mut().filter(|item| item.id == id) else {
            return;
        };
        operation.status = status;
        operation.message = message.into();
        operation.uncertain = uncertain;
        if let Some(result) = operation.result.take() {
            let _ = result.send(value);
        }
    }

    fn snapshot(&self) -> serde_json::Value {
        let operation = self.operation.lock().unwrap();
        let value = operation.as_ref().map(|item| {
            serde_json::json!({
                "status": item.status.name(),
                "message": item.message,
                "progress": item.progress,
                "uncertain": item.uncertain
            })
        });
        serde_json::json!({
            "ok": true,
            "value": value.unwrap_or_else(|| serde_json::json!({
                "status": "idle",
                "message": "Pronto para preparar as imagens do Docker."
            }))
        })
    }

    fn uncertain(&self, id: &str) -> bool {
        self.operation
            .lock()
            .unwrap()
            .as_ref()
            .is_some_and(|item| item.id == id && item.uncertain)
    }
}

struct Outcome {
    status: Status,
    message: String,
    value: serde_json::Value,
    uncertain: bool,
}

fn failure(message: impl Into<String>, uncertain: bool) -> Outcome {
    let message = message.into();
    let mut value = serde_json::json!({"ok": false, "error": message.clone()});
    if uncertain {
        value["uncertain"] = serde_json::Value::Bool(true);
    }
    Outcome {
        status: Status::Error,
        message: message.clone(),
        value,
        uncertain,
    }
}

fn cancelled(uncertain: bool) -> Outcome {
    let mut value = serde_json::json!({
        "ok": false,
        "cancelled": true,
        "error": "Preparação cancelada."
    });
    if uncertain {
        value["uncertain"] = serde_json::Value::Bool(true);
    }
    Outcome {
        status: Status::Cancelled,
        message: "Preparação cancelada.".into(),
        value,
        uncertain,
    }
}

fn public_result(mut frame: serde_json::Value) -> serde_json::Value {
    if let Some(value) = frame.as_object_mut() {
        value.remove("type");
    }
    frame
}

fn start_worker(
    input: &mut impl Write,
    cancel: &mpsc::Receiver<()>,
    closing: &std::sync::atomic::AtomicBool,
    force_stop: impl FnOnce() -> bool,
) -> Result<(), Outcome> {
    if cancel.try_recv().is_ok() || closing.load(Ordering::SeqCst) {
        return Err(cancelled(force_stop()));
    }
    if writeln!(input, "{{\"type\":\"start\"}}").is_err() {
        return Err(failure(
            "Não foi possível iniciar a preparação do Docker.",
            force_stop(),
        ));
    }
    Ok(())
}

fn run_worker(
    preparation: Arc<Preparation>,
    setup: Arc<Setup>,
    id: String,
    cancel: mpsc::Receiver<()>,
) -> Outcome {
    let script = crate::layout::base().join("prepare-docker.mjs");
    let mut owned = match OwnedProcess::spawn(&script) {
        Ok(owned) => owned,
        Err(_) => return failure("Não foi possível iniciar o processo de preparação.", false),
    };
    let Some(mut input) = owned.child.stdin.take() else {
        let uncertain = owned.force_stop().is_err();
        return failure(
            "Não foi possível confirmar o encerramento da preparação.",
            uncertain,
        );
    };
    let Some(stderr) = owned.child.stderr.take() else {
        let uncertain = owned.force_stop().is_err();
        return failure(
            "Não foi possível confirmar o encerramento da preparação.",
            uncertain,
        );
    };
    let (send, frames) = mpsc::channel();
    std::thread::spawn(move || {
        let mut reader = BufReader::new(stderr);
        loop {
            let frame = crate::read_frame(&mut reader).map_err(|_| ());
            let failed = frame.is_err();
            if send.send(frame).is_err() || failed {
                break;
            }
        }
    });
    let ready = match frames.recv_timeout(Duration::from_secs(10)) {
        Ok(Ok(frame)) if frame["type"] == "ready" => true,
        _ => false,
    };
    if !ready {
        let uncertain = owned.force_stop().is_err();
        if cancel.try_recv().is_ok() || setup.closing.load(Ordering::SeqCst) {
            return cancelled(uncertain);
        }
        return failure(
            "O processo de preparação não iniciou corretamente.",
            uncertain,
        );
    }
    if let Err(outcome) = start_worker(&mut input, &cancel, &setup.closing, || {
        owned.force_stop().is_err()
    }) {
        return outcome;
    }
    let mut cancel_requested = false;
    let mut cancel_deadline = None;
    let result;
    let deadline = Instant::now() + Duration::from_secs(30 * 60);
    loop {
        if !cancel_requested && (cancel.try_recv().is_ok() || setup.closing.load(Ordering::SeqCst))
        {
            cancel_requested = true;
            cancel_deadline = Some(Instant::now() + Duration::from_secs(10));
            preparation.mark_cancelling(&id);
            let _ = writeln!(input, "{{\"type\":\"cancel\"}}");
        }
        if cancel_requested && cancel_deadline.is_some_and(|deadline| Instant::now() >= deadline) {
            let uncertain = owned.force_stop().is_err();
            return cancelled(uncertain);
        }
        if Instant::now() >= deadline {
            let uncertain = owned.force_stop().is_err();
            return failure(
                "A preparação excedeu o tempo limite. O Docker pode manter trabalho já enviado.",
                uncertain,
            );
        }
        match frames.recv_timeout(Duration::from_millis(25)) {
            Ok(Ok(frame)) if frame["type"] == "progress" => {
                preparation.progress(
                    &id,
                    frame["phase"].as_str().unwrap_or("working"),
                    frame["message"]
                        .as_str()
                        .unwrap_or("Preparando as imagens do Docker…"),
                );
            }
            Ok(Ok(frame)) if frame["type"] == "result" => {
                result = frame;
                break;
            }
            Ok(Ok(_)) => {}
            Ok(Err(())) | Err(mpsc::RecvTimeoutError::Disconnected) => {
                let uncertain = owned.force_stop().is_err();
                return failure("A preparação terminou sem resultado confirmado.", uncertain);
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {}
        }
    }
    let frame = result;
    if frame["ok"] != true || cancel_requested {
        let uncertain = owned.force_stop().is_err();
        if cancel_requested {
            return cancelled(uncertain);
        }
        return failure(
            frame["error"]
                .as_str()
                .unwrap_or("Não foi possível preparar as imagens do Docker."),
            uncertain,
        );
    }
    let deadline = Instant::now() + Duration::from_secs(10);
    while !owned.has_exited().unwrap_or(true) {
        if Instant::now() >= deadline {
            let uncertain = owned.force_stop().is_err();
            return failure(
                "O encerramento da preparação não foi confirmado.",
                uncertain,
            );
        }
        std::thread::sleep(Duration::from_millis(25));
    }
    match owned.finish_checked() {
        Ok(status) if status.success() => Outcome {
            status: Status::Success,
            message: "Imagens do Docker preparadas.".into(),
            value: public_result(frame),
            uncertain: false,
        },
        Ok(_) => failure("A preparação do Docker não terminou com sucesso.", false),
        Err(_) => failure("O encerramento da preparação não foi confirmado.", true),
    }
}

fn check(window: &tauri::WebviewWindow) -> Result<(), String> {
    if window.label() != "preparation"
        || window
            .url()
            .map_err(|_| "Origem de preparação inválida.".to_string())?
            != url()
    {
        return Err("Origem de preparação inválida.".into());
    }
    Ok(())
}

fn url() -> Url {
    #[cfg(windows)]
    let origin = "http://tauri.localhost/preparation.html";
    #[cfg(not(windows))]
    let origin = "tauri://localhost/preparation.html";
    Url::parse(origin).unwrap()
}

fn open(app: &tauri::AppHandle) -> Result<(), String> {
    let expected = url();
    if let Some(window) = app.get_webview_window("preparation") {
        window
            .show()
            .map_err(|_| "Não foi possível abrir a preparação.".to_string())?;
        window.set_focus().ok();
        window
            .eval("location.reload()")
            .map_err(|_| "Não foi possível reiniciar a confirmação.".to_string())?;
        return Ok(());
    }
    let preparation = app.state::<Arc<Preparation>>().inner().clone();
    let window = tauri::WebviewWindowBuilder::new(
        app,
        "preparation",
        tauri::WebviewUrl::App("preparation.html".into()),
    )
    .title("Preparar imagens do Docker")
    .inner_size(620.0, 620.0)
    .min_inner_size(360.0, 480.0)
    .incognito(true)
    .disable_drag_drop_handler()
    .on_navigation(move |target| target == &expected)
    .on_new_window(|_, _| tauri::webview::NewWindowResponse::Deny)
    .on_permission_request(|_, _| tauri::webview::PermissionResponse::Deny)
    .build()
    .map_err(|_| "Não foi possível abrir a confirmação nativa.".to_string())?;
    let closing_window = window.clone();
    window.on_window_event(move |event| {
        if let tauri::WindowEvent::CloseRequested { api, .. } = event {
            api.prevent_close();
            let _ = preparation.cancel();
            let _ = closing_window.destroy();
        }
    });
    Ok(())
}

#[tauri::command]
pub fn preparation_state(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, Arc<Preparation>>,
) -> Result<serde_json::Value, String> {
    check(&window)?;
    Ok(state.snapshot())
}

#[tauri::command]
pub fn preparation_start(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, Arc<Preparation>>,
    setup: tauri::State<'_, Arc<Setup>>,
) -> Result<serde_json::Value, String> {
    check(&window)?;
    state.start(window.app_handle().clone(), setup.inner().clone())?;
    Ok(serde_json::json!({"ok": true}))
}

#[tauri::command]
pub fn preparation_cancel(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, Arc<Preparation>>,
) -> Result<serde_json::Value, String> {
    check(&window)?;
    state.cancel()?;
    Ok(serde_json::json!({"ok": true}))
}

#[tauri::command]
pub async fn desktop_prepare(
    window: tauri::WebviewWindow,
    desktop: tauri::State<'_, Arc<crate::desktop::Desktop>>,
    setup: tauri::State<'_, Arc<Setup>>,
    preparation: tauri::State<'_, Arc<Preparation>>,
) -> Result<serde_json::Value, String> {
    let target = desktop.check(&window)?;
    if target.instance.mode != "local" {
        return Ok(serde_json::json!({
            "ok": false,
            "error": "Abra o Persona local antes de preparar o Docker."
        }));
    }
    if !setup.reserve_operation() {
        return Ok(serde_json::json!({
            "ok": false,
            "error": "Aguarde a operação atual terminar."
        }));
    }
    let (send, receive) = mpsc::channel();
    let id = match preparation.begin(send) {
        Ok(id) => id,
        Err(error) => {
            setup.finish_operation();
            return Ok(serde_json::json!({"ok": false, "error": error}));
        }
    };
    if let Err(error) = open(window.app_handle()) {
        preparation.finish(
            &id,
            Status::Error,
            error.clone(),
            serde_json::json!({"ok": false, "error": error}),
            false,
        );
        setup.finish_operation();
        return Ok(serde_json::json!({"ok": false, "error": error}));
    }
    let preparation = preparation.inner().clone();
    let setup = setup.inner().clone();
    let waiting_preparation = preparation.clone();
    let waiting_setup = setup.clone();
    let value = tauri::async_runtime::spawn_blocking(move || {
        loop {
            match receive.recv_timeout(Duration::from_millis(25)) {
                Ok(value) => break value,
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    break serde_json::json!({
                        "ok": false,
                        "error": "A preparação terminou sem resultado confirmado."
                    });
                }
                Err(mpsc::RecvTimeoutError::Timeout) => {
                    if waiting_setup.closing.load(Ordering::SeqCst) {
                        let _ = waiting_preparation.cancel();
                    }
                }
            }
        }
    })
    .await
    .map_err(|error| error.to_string())?;
    if !preparation.uncertain(&id) {
        setup.finish_operation();
    }
    Ok(value)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cancellation_while_waiting_for_ready_never_sends_start() {
        for closing_requested in [false, true] {
            for uncertain in [false, true] {
                let (send, cancel) = mpsc::channel();
                let closing = std::sync::atomic::AtomicBool::new(false);
                let (ready_send, ready) = mpsc::channel();
                std::thread::scope(|scope| {
                    scope.spawn(|| {
                        if closing_requested {
                            closing.store(true, Ordering::SeqCst);
                        } else {
                            send.send(()).unwrap();
                        }
                        ready_send.send(()).unwrap();
                    });
                    ready.recv_timeout(Duration::from_secs(1)).unwrap();
                    let mut input = Vec::new();
                    let mut stopped = false;
                    let outcome = start_worker(&mut input, &cancel, &closing, || {
                        stopped = true;
                        uncertain
                    })
                    .err()
                    .expect("cancellation must prevent start");
                    assert!(input.is_empty());
                    assert!(stopped);
                    assert!(outcome.status == Status::Cancelled);
                    assert_eq!(outcome.uncertain, uncertain);
                    assert_eq!(outcome.value["cancelled"], true);
                    assert_eq!(outcome.value.get("uncertain").is_some(), uncertain);
                });
            }
        }
    }

    #[test]
    fn cancelling_before_start_returns_a_distinct_terminal_result() {
        let preparation = Preparation::default();
        let (send, receive) = mpsc::channel();
        preparation.begin(send).unwrap();
        assert_eq!(preparation.snapshot()["value"]["status"], "idle");
        preparation.cancel().unwrap();
        let value = receive.recv_timeout(Duration::from_secs(1)).unwrap();
        assert_eq!(value["ok"], false);
        assert_eq!(value["cancelled"], true);
        assert_eq!(preparation.snapshot()["value"]["status"], "cancelled");
    }

    #[test]
    fn a_finished_operation_can_be_replaced_but_an_active_one_cannot() {
        let preparation = Preparation::default();
        let (first, first_result) = mpsc::channel();
        preparation.begin(first).unwrap();
        preparation.cancel().unwrap();
        first_result.recv_timeout(Duration::from_secs(1)).unwrap();
        let (second, _second_result) = mpsc::channel();
        preparation.begin(second).unwrap();
        let (third, _third_result) = mpsc::channel();
        assert!(preparation.begin(third).is_err());
    }
}
