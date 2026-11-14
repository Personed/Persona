use tauri::Url;

pub fn valid_origin(url: &Url) -> bool {
    (url.scheme() == "https"
        || (url.scheme() == "http"
            && matches!(url.host_str(), Some("localhost" | "127.0.0.1" | "[::1]"))))
        && url.path() == "/"
        && url.query().is_none()
        && url.fragment().is_none()
        && url.username().is_empty()
        && url.password().is_none()
}

pub fn external_allowed(url: &Url) -> bool {
    url.scheme() == "https" && url.username().is_empty() && url.password().is_none()
}

pub fn open_external(url: Url) {
    if !external_allowed(&url) {
        return;
    }
    std::thread::spawn(move || {
        #[cfg(windows)]
        unsafe {
            use windows::{
                Win32::UI::{Shell::ShellExecuteW, WindowsAndMessaging::SW_SHOWNORMAL},
                core::{HSTRING, w},
            };
            // Pass one URL to the OS association, never through a command shell.
            let _ = ShellExecuteW(
                None,
                w!("open"),
                &HSTRING::from(url.as_str()),
                None,
                None,
                SW_SHOWNORMAL,
            );
        }
        #[cfg(not(windows))]
        {
            #[cfg(target_os = "macos")]
            let program = "open";
            #[cfg(not(target_os = "macos"))]
            let program = "xdg-open";
            let _ = std::process::Command::new(program)
                .arg(url.as_str())
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status();
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn remote_origins_and_external_popups_keep_the_shared_policy() {
        for value in [
            "https://example.invalid/",
            "https://example.invalid:8443/",
            "http://localhost/",
            "http://127.0.0.1:1234/",
            "http://[::1]:1234/",
        ] {
            assert!(valid_origin(&Url::parse(value).unwrap()), "{value}");
        }
        for value in [
            "http://example.invalid/",
            "https://user:pass@example.invalid/",
            "https://example.invalid/path",
            "https://example.invalid/?q=x",
            "https://example.invalid/#x",
            "file:///index.html",
        ] {
            assert!(!valid_origin(&Url::parse(value).unwrap()), "{value}");
        }
        assert!(external_allowed(
            &Url::parse("https://example.invalid/path?q=x#part").unwrap()
        ));
        for value in [
            "http://127.0.0.1/",
            "https://user@example.invalid/",
            "javascript:alert(1)",
            "file:///index.html",
        ] {
            assert!(!external_allowed(&Url::parse(value).unwrap()));
        }
    }
}
