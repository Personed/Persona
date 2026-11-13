use std::{io::Write, path::PathBuf, str::FromStr, sync::Mutex};

use tauri::{
    Manager,
    ipc::CapabilityBuilder,
    menu::{CheckMenuItem, MenuBuilder, PredefinedMenuItem, Submenu, SubmenuBuilder},
    utils::{acl::RemoteUrlPattern, config::Color},
};

/// The local environment is a singleton in the pilot, so its id is stable.
pub const LOCAL_ID: &str = "00000000-0000-4000-8000-000000000000";
pub const LOCAL_NAME: &str = "Este computador (temporário)";

#[derive(Clone, PartialEq, Debug)]
pub struct Instance {
    pub id: String,
    pub name: String,
    pub mode: String,
    pub runtime: String,
    pub url: String,
}

impl Instance {
    pub fn json(&self) -> serde_json::Value {
        serde_json::json!({
            "id": self.id,
            "name": self.name,
            "mode": self.mode,
            "runtime": self.runtime,
            "url": self.url,
        })
    }

    fn from_json(value: &serde_json::Value) -> Option<Self> {
        let id = value["id"].as_str()?.trim();
        let name = value["name"].as_str()?.trim();
        let mode = value["mode"].as_str()?;
        let runtime = value["runtime"].as_str()?;
        let url = value["url"].as_str()?;
        if id.is_empty() || id.len() > 64 || name.is_empty() || name.len() > 80 {
            return None;
        }
        if runtime != "native" {
            return None;
        }
        let parsed = tauri::Url::parse(url).ok()?;
        let valid = match mode {
            "local" => {
                parsed.scheme() == "http"
                    && parsed.host_str() == Some("127.0.0.1")
                    && parsed.port().is_some()
            }
            "existing" => crate::remote::valid_origin(&parsed),
            _ => false,
        };
        if !valid {
            return None;
        }
        Some(Self {
            id: id.into(),
            name: name.into(),
            mode: mode.into(),
            runtime: runtime.into(),
            url: url.into(),
        })
    }
}

#[derive(Clone)]
pub struct Target {
    pub instance: Instance,
    pub label: String,
    pub origin: tauri::Url,
}

pub struct Desktop {
    current: Mutex<Option<Target>>,
    instances: Mutex<Vec<Instance>>,
    selected: Mutex<Option<String>>,
    directory: PathBuf,
}

impl Default for Desktop {
    fn default() -> Self {
        Self {
            current: Mutex::new(None),
            instances: Mutex::new(Vec::new()),
            selected: Mutex::new(None),
            directory: PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("target/test-data/desktop"),
        }
    }
}

impl Desktop {
    /// Reads the disposable instance list. A malformed file is ignored instead of
    /// failing the launch; the pilot never owns real user configuration.
    pub fn load(&self) {
        let Ok(text) = std::fs::read_to_string(self.path()) else {
            return;
        };
        let Ok(value) = serde_json::from_str::<serde_json::Value>(&text) else {
            return;
        };
        let instances = value["instances"]
            .as_array()
            .map(|items| {
                items
                    .iter()
                    .filter_map(Instance::from_json)
                    .collect::<Vec<Instance>>()
            })
            .unwrap_or_default();
        let selected = value["selected"]
            .as_str()
            .filter(|id| instances.iter().any(|item| &item.id == id))
            .map(str::to_string);
        *self.instances.lock().unwrap() = instances;
        *self.selected.lock().unwrap() = selected;
    }

    fn path(&self) -> PathBuf {
        self.directory.join("instances.json")
    }

    pub fn save(&self) -> std::io::Result<()> {
        let instances = self.instances.lock().unwrap();
        let selected = self.selected.lock().unwrap();
        self.persist(&instances, selected.as_deref())
    }

    fn persist(&self, instances: &[Instance], selected: Option<&str>) -> std::io::Result<()> {
        let value = serde_json::json!({
            "instances": instances.iter().map(Instance::json).collect::<Vec<_>>(),
            "selected": selected,
        });
        std::fs::create_dir_all(&self.directory)?;
        let pending = self.path().with_extension("json.pending");
        write_private(&pending, &value.to_string())?;
        // rename replaces existing files on Windows too; never remove the saved file first.
        std::fs::rename(&pending, self.path())
    }

    pub fn remember(&self, instance: Instance) -> std::io::Result<()> {
        let id = instance.id.clone();
        let mut instances = self.instances.lock().unwrap();
        let mut selected = self.selected.lock().unwrap();
        let mut updated = instances.clone();
        match updated.iter_mut().find(|item| item.id == id) {
            Some(item) => *item = instance,
            None => updated.push(instance),
        }
        self.persist(&updated, Some(&id))?;
        *instances = updated;
        *selected = Some(id);
        Ok(())
    }

    pub fn list(&self) -> (Vec<Instance>, Option<String>) {
        (
            self.instances.lock().unwrap().clone(),
            self.selected.lock().unwrap().clone(),
        )
    }

    /// The selected instance, as the setup page expects it.
    pub fn selected_instance(&self) -> Option<Instance> {
        let selected = self.selected.lock().unwrap().clone()?;
        self.find(&selected)
    }

    pub fn find(&self, id: &str) -> Option<Instance> {
        self.instances
            .lock()
            .unwrap()
            .iter()
            .find(|item| item.id == id)
            .cloned()
    }

    /// Reuses the instance that already points at this origin, like the product does.
    pub fn by_url(&self, url: &str) -> Option<Instance> {
        self.instances
            .lock()
            .unwrap()
            .iter()
            .find(|item| item.url == url)
            .cloned()
    }

    /// Replaces the active target and returns the previous one.
    pub fn replace(&self, target: Target) -> Option<Target> {
        self.current.lock().unwrap().replace(target)
    }

    /// Drops the disposable list. The smokes use it to start from a known state.
    pub fn reset(&self) -> std::io::Result<()> {
        self.instances.lock().unwrap().clear();
        *self.selected.lock().unwrap() = None;
        self.save()
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

fn write_private(path: &std::path::Path, text: &str) -> std::io::Result<()> {
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    file.write_all(text.as_bytes())?;
    file.sync_all()
}

#[cfg(test)]
mod persistence_tests {
    use super::*;

    #[test]
    fn two_writes_then_reload() {
        let directory =
            std::env::temp_dir().join(format!("persona-desktop-{}", uuid::Uuid::new_v4()));
        let desktop = Desktop {
            directory: directory.clone(),
            ..Desktop::default()
        };
        let mut instance = Instance {
            id: LOCAL_ID.into(),
            name: "First".into(),
            mode: "local".into(),
            runtime: "native".into(),
            url: "http://127.0.0.1:12345".into(),
        };
        desktop.remember(instance.clone()).unwrap();
        instance.name = "Second".into();
        desktop.remember(instance.clone()).unwrap();
        let restored = Desktop {
            directory: directory.clone(),
            ..Desktop::default()
        };
        restored.load();
        assert_eq!(restored.list(), (vec![instance], Some(LOCAL_ID.into())));
        std::fs::remove_dir_all(directory).unwrap();
    }

    #[test]
    fn save_reports_directory_error() {
        let directory =
            std::env::temp_dir().join(format!("persona-desktop-{}", uuid::Uuid::new_v4()));
        std::fs::write(&directory, "not a directory").unwrap();
        let desktop = Desktop {
            directory: directory.clone(),
            ..Desktop::default()
        };
        assert!(desktop.save().is_err());
        std::fs::remove_file(directory).unwrap();
    }

    #[test]
    fn failed_remember_preserves_instances_and_selection() {
        let directory =
            std::env::temp_dir().join(format!("persona-desktop-{}", uuid::Uuid::new_v4()));
        let desktop = Desktop {
            directory: directory.clone(),
            ..Desktop::default()
        };
        let instance = Instance {
            id: LOCAL_ID.into(),
            name: "Saved".into(),
            mode: "local".into(),
            runtime: "native".into(),
            url: "http://127.0.0.1:12345".into(),
        };
        desktop.remember(instance.clone()).unwrap();
        let before = desktop.list();
        std::fs::remove_dir_all(&directory).unwrap();
        std::fs::write(&directory, "not a directory").unwrap();

        let mut changed = instance.clone();
        changed.name = "Not saved".into();
        assert!(desktop.remember(changed.clone()).is_err());
        assert_eq!(desktop.list(), before);
        changed.id = "another-instance".into();
        assert!(desktop.remember(changed).is_err());
        assert_eq!(desktop.list(), before);

        std::fs::remove_file(directory).unwrap();
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
    let (instances, selected) = state.list();
    Ok(serde_json::json!({
        "ok": true,
        "value": {
            "instances": instances.iter().map(Instance::json).collect::<Vec<_>>(),
            "selected": selected,
            "running": target.instance.mode == "local",
            "mode": target.instance.mode,
            "runtime": target.instance.runtime,
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

fn persona_menu<R: tauri::Runtime>(
    app: &tauri::AppHandle<R>,
    instances: &[Instance],
    selected: Option<&str>,
) -> Result<Submenu<R>, String> {
    let mut builder = SubmenuBuilder::new(app, "Persona")
        .text("open-local", "Abrir Persona Local")
        .text("connect", "Conectar a outro Persona…");
    for instance in instances {
        let item = CheckMenuItem::with_id(
            app,
            format!("instance:{}", instance.id),
            &instance.name,
            true,
            selected == Some(instance.id.as_str()),
            None::<&str>,
        )
        .map_err(|_| "Não foi possível abrir o menu.")?;
        builder = builder.item(&item);
    }
    builder
        .separator()
        .text("quit", "Sair do Persona")
        .build()
        .map_err(|_| "Não foi possível abrir o menu.".to_string())
}

#[tauri::command]
pub fn desktop_menu(
    window: tauri::WebviewWindow,
    state: tauri::State<'_, std::sync::Arc<Desktop>>,
    scope: Option<String>,
) -> Result<serde_json::Value, String> {
    state.check(&window)?;
    if scope.as_deref().is_some_and(|scope| scope != "environment") {
        return Err("Menu inválido.".into());
    }
    let app = window.app_handle();
    let (instances, selected) = state.list();
    let persona = persona_menu(app, &instances, selected.as_deref())?;
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

/// Handles the native menu items that change the environment.
pub fn menu_event(app: &tauri::AppHandle, id: &str) {
    if id == "quit" {
        app.exit(0);
        return;
    }
    if id == "connect" {
        if let Some(window) = app.get_webview_window("setup") {
            let _ = window.show();
            let _ = window.set_focus();
        }
        return;
    }
    let desktop = app.state::<std::sync::Arc<Desktop>>().inner().clone();
    let instance = if id == "open-local" {
        Some(desktop.find(LOCAL_ID).unwrap_or(Instance {
            id: LOCAL_ID.into(),
            name: LOCAL_NAME.into(),
            mode: "local".into(),
            runtime: "native".into(),
            url: String::new(),
        }))
    } else {
        id.strip_prefix("instance:")
            .and_then(|instance_id| desktop.find(instance_id))
    };
    if let Some(instance) = instance {
        crate::setup::switch(app, instance);
    }
}
