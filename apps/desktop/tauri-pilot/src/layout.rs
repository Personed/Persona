use std::path::PathBuf;

pub fn base() -> PathBuf {
    #[cfg(windows)]
    if let Ok(executable) = std::env::current_exe()
        && let Some(directory) = executable.parent()
        && directory.join("portable-test.json").is_file()
    {
        return directory.to_path_buf();
    }
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

pub fn node() -> PathBuf {
    let base = base();
    if base.join("portable-test.json").is_file() {
        base.join("node.exe")
    } else {
        PathBuf::from("node")
    }
}
