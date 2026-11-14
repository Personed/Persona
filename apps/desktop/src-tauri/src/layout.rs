use std::{
    path::{Path, PathBuf},
    sync::{OnceLock, RwLock},
};

#[derive(Clone)]
struct Layout {
    resources: PathBuf,
    data: PathBuf,
}

static LAYOUT: OnceLock<RwLock<Layout>> = OnceLock::new();

fn storage() -> &'static RwLock<Layout> {
    LAYOUT.get_or_init(|| {
        RwLock::new(Layout {
            resources: development_base(),
            data: development_data(),
        })
    })
}

fn development_base() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

fn development_data() -> PathBuf {
    development_base().join("target/test-data")
}

fn executable_directory() -> Option<PathBuf> {
    std::env::current_exe()
        .ok()
        .and_then(|path| path.parent().map(Path::to_path_buf))
}

/// Separates immutable bundled resources from writable pilot data.
pub fn configure() -> Result<(), String> {
    let resource_dir = executable_directory().ok_or("Pilot executable directory unavailable")?;
    let executable_dir = executable_directory().unwrap_or_else(|| resource_dir.clone());
    let portable = executable_dir.join("portable-test.json").is_file();
    let installed = resource_dir.join("installer.json").is_file()
        && resource_dir.join("runtime/server.mjs").is_file();
    let resources = if portable {
        executable_dir
    } else if installed {
        resource_dir
    } else {
        development_base()
    };
    let data = if portable {
        resources.join("target/test-data")
    } else if installed {
        PathBuf::from(std::env::var_os("LOCALAPPDATA").ok_or("LOCALAPPDATA unavailable")?)
            .join("app.persona.desktop")
    } else {
        development_data()
    };
    *storage().write().unwrap() = Layout { resources, data };
    Ok(())
}

pub fn base() -> PathBuf {
    storage().read().unwrap().resources.clone()
}

pub fn data() -> PathBuf {
    storage().read().unwrap().data.clone()
}

pub fn node() -> PathBuf {
    let base = base();
    if base.join("node.exe").is_file() {
        base.join("node.exe")
    } else {
        PathBuf::from("node")
    }
}
