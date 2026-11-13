use std::{
    ffi::OsString,
    io::{self, BufRead, Write},
    path::PathBuf,
    process::{Command, Stdio},
};

pub fn requested_archive() -> Result<Option<PathBuf>, String> {
    let args: Vec<_> = std::env::args_os().skip(1).collect();
    if !args.iter().any(|arg| {
        arg == "--import-backup" || arg.to_string_lossy().starts_with("--import-backup=")
    }) {
        return Ok(None);
    }
    parse_archive(args).map(Some)
}

fn parse_archive(args: Vec<OsString>) -> Result<PathBuf, String> {
    let mut archive = None;
    let mut args = args.into_iter();
    while let Some(arg) = args.next() {
        if arg == "--import-backup" {
            if archive.is_some() {
                return Err("Use apenas um backup por importação.".into());
            }
            let value = args
                .next()
                .ok_or_else(|| "Informe o arquivo de backup.".to_string())?;
            if value.is_empty() {
                return Err("Informe o arquivo de backup.".into());
            }
            archive = Some(PathBuf::from(value));
        } else if let Some(value) = arg.to_string_lossy().strip_prefix("--import-backup=") {
            if archive.is_some() || value.is_empty() {
                return Err("Informe um arquivo de backup válido.".into());
            }
            archive = Some(PathBuf::from(value));
        } else {
            return Err("Opção de importação desconhecida.".into());
        }
    }
    archive.ok_or_else(|| "Informe o arquivo de backup.".into())
}

fn read_password() -> Result<String, String> {
    print!("Senha do backup: ");
    io::stdout()
        .flush()
        .map_err(|_| "Não foi possível ler a senha do backup.".to_string())?;
    let mut password = String::new();
    read_hidden(&mut password)
        .map_err(|_| "Não foi possível ler a senha do backup.".to_string())?;
    println!();
    let password = password.trim_end_matches(['\r', '\n']).to_string();
    if !(12..=1024).contains(&password.chars().count()) {
        return Err("A senha do backup deve ter entre 12 e 1024 caracteres.".into());
    }
    Ok(password)
}

#[cfg(windows)]
fn read_hidden(password: &mut String) -> io::Result<()> {
    use windows::Win32::System::Console::{
        CONSOLE_MODE, ENABLE_ECHO_INPUT, GetConsoleMode, GetStdHandle, STD_INPUT_HANDLE,
        SetConsoleMode,
    };

    let handle = match unsafe { GetStdHandle(STD_INPUT_HANDLE) } {
        Ok(handle) => handle,
        Err(_) => return io::stdin().lock().read_line(password).map(|_| ()),
    };
    let mut mode = CONSOLE_MODE(0);
    if unsafe { GetConsoleMode(handle, &mut mode) }.is_ok() {
        unsafe { SetConsoleMode(handle, CONSOLE_MODE(mode.0 & !ENABLE_ECHO_INPUT.0))? };
        let result = io::stdin().lock().read_line(password).map(|_| ());
        let _ = unsafe { SetConsoleMode(handle, mode) };
        result
    } else {
        io::stdin().lock().read_line(password).map(|_| ())
    }
}

#[cfg(not(windows))]
fn read_hidden(password: &mut String) -> io::Result<()> {
    io::stdin().lock().read_line(password)
}

pub fn run(archive: &PathBuf) -> Result<(), String> {
    let script = crate::layout::base().join("import-backup.mjs");
    if !script.is_file() {
        return Err("O importador do piloto não está disponível.".into());
    }
    let mut password = read_password()?;
    let mut command = Command::new(crate::layout::node());
    command
        .arg(script)
        .arg(archive)
        .env_clear()
        .stdin(Stdio::piped())
        .stdout(Stdio::inherit())
        .stderr(Stdio::inherit());
    for key in [
        "SystemRoot",
        "WINDIR",
        "SystemDrive",
        "PATH",
        "COMSPEC",
        "PATHEXT",
    ] {
        if let Some(value) = std::env::var_os(key) {
            command.env(key, value);
        }
    }
    let mut child = command
        .spawn()
        .map_err(|_| "Não foi possível iniciar o importador do piloto.".to_string())?;
    let mut input = child
        .stdin
        .take()
        .ok_or_else(|| "Não foi possível enviar a senha ao importador.".to_string())?;
    if let Err(error) = input
        .write_all(password.as_bytes())
        .and_then(|_| input.flush())
    {
        password.clear();
        let _ = child.kill();
        let _ = child.wait();
        return Err(format!(
            "Não foi possível enviar a senha ao importador: {error}"
        ));
    }
    password.clear();
    drop(input);
    let status = child
        .wait()
        .map_err(|_| "O importador do piloto foi encerrado inesperadamente.".to_string())?;
    if status.success() {
        Ok(())
    } else {
        Err("A importação do backup não foi concluída.".into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn import_arguments_require_one_archive() {
        assert!(
            parse_archive(vec![
                OsString::from("--import-backup"),
                OsString::from("backup.persona")
            ])
            .is_ok()
        );
        assert!(parse_archive(vec![OsString::from("--import-backup=backup.persona")]).is_ok());
        assert!(parse_archive(vec![OsString::from("--import-backup")]).is_err());
        assert!(parse_archive(vec![OsString::from("--import-backup"), OsString::new()]).is_err());
        assert!(
            parse_archive(vec![
                OsString::from("--import-backup"),
                OsString::from("one.persona"),
                OsString::from("--import-backup"),
                OsString::from("two.persona"),
            ])
            .is_err()
        );
        assert!(
            parse_archive(vec![
                OsString::from("--import-backup"),
                OsString::from("backup.persona"),
                OsString::from("--persistent-local"),
            ])
            .is_err()
        );
    }
}
