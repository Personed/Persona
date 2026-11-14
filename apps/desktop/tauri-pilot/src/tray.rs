use tauri::{Manager, menu::MenuBuilder, tray::TrayIconBuilder};

pub const OPEN_ID: &str = "tray-open";
pub const QUIT_ID: &str = "tray-quit";

pub fn enabled() -> bool {
    !crate::startup_check::enabled()
        && !crate::persistence_acceptance::enabled()
        && !std::env::args().any(|arg| arg.ends_with("-smoke-test") || arg == "--native-death-test")
}

pub fn reopen(app: &tauri::AppHandle) {
    for label in ["pilot", "remote", "setup"] {
        if let Some(window) = app.get_webview_window(label) {
            let _ = window.show();
            let _ = window.unminimize();
            let _ = window.set_focus();
            return;
        }
    }
}

#[cfg(any(windows, test))]
fn should_reopen(event: &tauri::tray::TrayIconEvent) -> bool {
    matches!(
        event,
        tauri::tray::TrayIconEvent::Click {
            button: tauri::tray::MouseButton::Left,
            button_state: tauri::tray::MouseButtonState::Up,
            ..
        }
    )
}

pub fn create(app: &tauri::AppHandle) -> tauri::Result<()> {
    if !enabled() {
        return Ok(());
    }
    let menu = MenuBuilder::new(app)
        .text(OPEN_ID, "Abrir Persona")
        .separator()
        .text(QUIT_ID, "Sair e encerrar serviço")
        .build()?;
    let mut builder = TrayIconBuilder::new().menu(&menu).tooltip("Persona");
    #[cfg(windows)]
    {
        builder = builder
            .show_menu_on_left_click(false)
            .on_tray_icon_event(|tray, event| {
                // Handle only release, not press or the separate double-click event.
                if should_reopen(&event) {
                    reopen(tray.app_handle());
                }
            });
    }
    if let Some(icon) = app.default_window_icon() {
        builder = builder.icon(icon.clone());
    }
    builder.build(app)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::should_reopen;
    use tauri::tray::{MouseButton, MouseButtonState, TrayIconEvent};

    #[test]
    fn reopens_only_on_left_release() {
        for button in [MouseButton::Left, MouseButton::Right, MouseButton::Middle] {
            for button_state in [MouseButtonState::Down, MouseButtonState::Up] {
                let event = TrayIconEvent::Click {
                    id: "synthetic-tray".into(),
                    position: Default::default(),
                    rect: Default::default(),
                    button,
                    button_state,
                };
                assert_eq!(
                    should_reopen(&event),
                    button == MouseButton::Left && button_state == MouseButtonState::Up
                );
            }
        }
        assert!(!should_reopen(&TrayIconEvent::DoubleClick {
            id: "synthetic-tray".into(),
            position: Default::default(),
            rect: Default::default(),
            button: MouseButton::Left,
        }));
        assert!(!should_reopen(&TrayIconEvent::Enter {
            id: "synthetic-tray".into(),
            position: Default::default(),
            rect: Default::default(),
        }));
    }
}
