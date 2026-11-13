fn main() {
    let output = std::path::Path::new("target/setup-assets");
    std::fs::create_dir_all(output).unwrap();
    for name in ["setup.css", "setup.js"] {
        println!("cargo:rerun-if-changed=../src/{name}");
        std::fs::copy(format!("../src/{name}"), output.join(name)).unwrap();
    }
    for (source, name) in [
        ("../../../packages/ui/src/web/controls.css", "controls.css"),
        ("../../../packages/ui/src/web/setup.css", "setup-layout.css"),
        ("../../../packages/ui/assets/icon.png", "icon.png"),
    ] {
        println!("cargo:rerun-if-changed={source}");
        std::fs::copy(source, output.join(name)).unwrap();
    }
    println!("cargo:rerun-if-changed=../../../packages/ui/src/tokens.ts");
    let theme = std::process::Command::new("node").args(["--input-type=module", "-e", "import {metrics,themes} from '../../../packages/ui/src/tokens.ts'; const vars=(v,p='')=>Object.entries(v).map(([k,v])=>`--${p}${k}:${v};`).join(''); process.stdout.write(JSON.stringify({css:`:root{${vars(metrics)}${vars(themes.light,'color-')}}@media(prefers-color-scheme:dark){:root{${vars(themes.dark,'color-')}}`,light:themes.light.frame,dark:themes.dark.frame}))"]).output().unwrap();
    assert!(theme.status.success(), "theme generation failed");
    let theme: serde_json::Value = serde_json::from_slice(&theme.stdout).unwrap();
    std::fs::write(
        output.join("setup-theme.css"),
        theme["css"].as_str().unwrap(),
    )
    .unwrap();
    println!(
        "cargo:rustc-env=PERSONA_FRAME_LIGHT={}",
        theme["light"].as_str().unwrap()
    );
    println!(
        "cargo:rustc-env=PERSONA_FRAME_DARK={}",
        theme["dark"].as_str().unwrap()
    );
    println!("cargo:rerun-if-changed=../src/setup.html");
    let source = std::fs::read_to_string("../src/setup.html").unwrap();
    assert!(
        source.contains("default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'"),
        "setup CSP changed; review the pilot transport policy"
    );
    assert!(
        source.contains("<script src=\"setup.js\"></script>"),
        "setup script entry changed"
    );
    let html = source
        .replace("default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src ipc: http://ipc.localhost; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'")
        .replace("Seus Bots e dados ficam salvos aqui.", "Piloto descartável: não use credenciais reais. Falhas podem reter dados de teste.")
        .replace("<script src=\"setup.js\"></script>", "<script src=\"setup-bridge.js\"></script><script src=\"setup.js\"></script>");
    std::fs::write(output.join("setup.html"), html).unwrap();
    let platform = match std::env::var("CARGO_CFG_TARGET_OS").unwrap().as_str() {
        "windows" => "win32",
        "macos" => "darwin",
        _ => "linux",
    };
    std::fs::write(
        output.join("setup-bridge.js"),
        std::fs::read_to_string("setup-bridge.js")
            .unwrap()
            .replace("__PLATFORM__", platform),
    )
    .unwrap();
    println!("cargo:rerun-if-changed=setup-bridge.js");
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "setup_state",
            "setup_submit",
            "setup_cancel",
            "setup_docker",
            "desktop_state",
            "desktop_appearance",
            "desktop_menu",
        ]),
    ))
    .unwrap();
}
