# Persona desktop — Tauri 3

Este é o app desktop principal. Não usa Electron.

## Executar e gerar instalador

Na raiz do repositório:

```sh
pnpm desktop
pnpm --filter @persona/desktop dist
```

Requer Node >=24, pnpm, Rust >=1.95, MSVC e WebView2 no Windows.
O instalador NSIS é gerado em
`apps/desktop/src-tauri/target/release/bundle/nsis/*-setup.exe`.
Ele inclui Node, o serviço e a interface. Instala somente para o usuário atual
e oferece instalação do WebView2 quando necessário.

## Dados e contas

O app instalado mantém os dados em `%LOCALAPPDATA%/app.persona.desktop`.
O serviço nativo persiste Bots e histórico entre execuções. Ele usa as contas,
instalações e permissões do usuário do sistema operacional. O modo Docker usa
seu próprio perfil e volumes. O menu permite alternar ambientes e abrir a
confirmação nativa para backup ou restauração.

Perfis antigos não são apagados, sobrescritos nem importados automaticamente.
Para importar um backup cifrado para um perfil vazio:

```powershell
persona.exe --import-backup C:\caminho\backup.persona
```

A senha é lida sem eco; ela não entra em argumentos ou logs. A restauração cria
um perfil novo e mantém a origem intacta. Os nomes internos `pilot-local-*` e
`pilot-docker-*` permanecem compatíveis com os arquivos do piloto anterior.

## Fronteiras e verificação

Rust controla o supervisor Node por pipes privados. Tokens de sessão não chegam
ao JavaScript da WebView. A origem HTTP é validada, o cookie é HttpOnly e cada
comando IPC verifica a janela e a origem. Setup, manutenção e preparação têm
capabilities separadas. O processo nativo termina somente seus descendentes.

Os smokes existentes usam homes e dados sintéticos. O build do instalador roda
o smoke de ciclo de vida com um `LOCALAPPDATA` temporário; ele não instala o
produto nem acessa perfis reais. Os testes de serviço, backup e persistência
ficam neste diretório. O teste de input usa uma janela Chromium dedicada, não
o navegador pessoal.

Tauri está fixado em `3.0.0-alpha.6`. O suporte de build do instalador é Windows.
Linux/macOS e migração de um perfil real exigem aceitação própria. Docker real
só deve ser testado em daemon isolado autorizado. A comunicação HTTP loopback
não promete proteção contra outro processo local hostil.
