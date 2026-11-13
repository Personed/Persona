use std::{str::FromStr, sync::Mutex};
use tauri::{
    Manager,
    ipc::CapabilityBuilder,
    menu::{MenuBuilder, PredefinedMenuItem, SubmenuBuilder},
    utils::{acl::RemoteUrlPattern, config::Color},
};

#[derive(Clone)]
pub struct Target {
    pub id: String,
    pub name: String,
    pub mode: String,
    pub runtime: String,
    pub url: String,
    pub label: String,
    pub origin: tauri::Url,
}

#[derive(Default)]
pub struct Desktop {
    current: Mutex<Option<Target>>,
}

impl Desktop {
    pub fn set(&self, target: Target) {
        *self.current.lock().unwrap() = Some(target);
    }

    pub fn clear(&self, label: &str) {
        let mut current = self.current.lock().unwrap();
        if current.as_ref().is_some_and(|target| target.label == label) {
            *current = None;
        }
    }

    fn check(&self, window: &tauri::WebviewWindow) -> Result<Target, String> {
        let current = self
            .current
            .lock()
            .unwrap()
            .clone()
            .ok_or("Origem da requisição inválida.")?;
        let url = window.url().map_err(|_| "Origem da requisição inválida.")?;
        if window.label() != current.label || !crate::allows_navigation(&current.origin, &url) {
            return Err("Origem da requisição inválida.".into());
        }
        Ok(current)
    }
}

pub fn bridge_script() -> String {
    let platform = if cfg!(target_os = "windows") {
        "win32"
    } else if cfg!(target_os = "macos") {
        "darwin"
    } else {
        "linux"
    };
    include_str!("../desktop-bridge.js").replace("__PLATFORM__", platform)
}

/// Grants the desktop commands to one webview, scoped to the exact validated
/// origin. The pilot windows load from loopback HTTP, which Tauri treats as a
/// remote origin, so a static local capability never applies to them.
pub fn grant(app: &tauri::AppHandle, label: &str, origin: &tauri::Url) -> Result<(), String> {
    let pattern = origin.origin().ascii_serialization();
    // Validate before handing the pattern to Tauri: an invalid pattern panics while resolving.
    pattern
        .parse::<RemoteUrlPattern>()
        .map_err(|_| "Origem inválida.".to_string())?;
    app.add_capability(
        CapabilityBuilder::new(format!("desktop-ui-{label}"))
            .local(false)
            .remote(pattern)
            .webview(label)
            .permission("allow-desktop-state")
            .permission("allow-desktop-appearance")
            .permission("allow-desktop-menu"),
    )
    .map_err(|_| "Não foi possível conceder acesso à interface local.".to_string())
}

#[tauri::command]
pub fn desktop_state(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, std::sync::Arc<Desktop>>,
) -> Result<serde_json::Value, String> {
    let target = state.check(&window)?;
    Ok(serde_json::json!({
        "ok": true,
        "value": {
            "instances": [{
                "id": target.id,
                "name": target.name,
                "mode": target.mode,
                "url": target.url,
            }],
            "selected": target.id,
            "running": target.mode == "local",
            "mode": target.mode,
            "runtime": target.runtime,
        }
    }))
}

#[tauri::command]
pub fn desktop_appearance(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, std::sync::Arc<Desktop>>,
    theme: String,
) -> Result<serde_json::Value, String> {
    state.check(&window)?;
    let color = match theme.as_str() {
        "light" => env!("PERSONA_FRAME_LIGHT"),
        "dark" => env!("PERSONA_FRAME_DARK"),
        _ => return Err("Tema inválido.".into()),
    };
    let color = Color::from_str(color).map_err(|_| "Tema inválido.")?;
    window
        .set_background_color(Some(color))
        .map_err(|_| "Não foi possível aplicar o tema.")?;
    Ok(serde_json::json!({"ok": true}))
}

#[tauri::command]
pub fn desktop_menu(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, std::sync::Arc<Desktop>>,
    scope: Option<String>,
) -> Result<serde_json::Value, String> {
    let target = state.check(&window)?;
    if scope.as_deref().is_some_and(|scope| scope != "environment") {
        return Err("Menu inválido.".into());
    }
    let app = window.app_handle();
    let persona = SubmenuBuilder::new(app, "Persona")
        .check("current-instance", &target.name)
        .separator()
        .text("quit", "Sair do Persona")
        .build()
        .map_err(|_| "Não foi possível abrir o menu.")?;
    if scope.as_deref() == Some("environment") {
        window
            .popup_menu(&persona)
            .map_err(|_| "Não foi possível abrir o menu.")?;
        return Ok(serde_json::json!({"ok": true}));
    }
    let edit = SubmenuBuilder::new(app, "Editar")
        .undo()
        .redo()
        .cut()
        .copy()
        .paste()
        .select_all()
        .build()
        .map_err(|_| "Não foi possível abrir o menu.")?;
    let fullscreen =
        PredefinedMenuItem::fullscreen(app, None).map_err(|_| "Não foi possível abrir o menu.")?;
    let view = SubmenuBuilder::new(app, "Visualizar")
        .item(&fullscreen)
        .build()
        .map_err(|_| "Não foi possível abrir o menu.")?;
    let menu = MenuBuilder::new(app)
        .item(&persona)
        .item(&edit)
        .item(&view)
        .build()
        .map_err(|_| "Não foi possível abrir o menu.")?;
    window
        .popup_menu(&menu)
        .map_err(|_| "Não foi possível abrir o menu.")?;
    Ok(serde_json::json!({"ok": true}))
}
