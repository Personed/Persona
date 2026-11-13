# Persona Tauri pilot — integração temporária

Piloto de desenvolvimento isolado. Não substitui Electron, não migra instalações
e não promete paridade. Abre o setup existente em uma WebView local separada.
O submit local temporário usa a UI existente de `apps/web/dist`, servida pelo
runtime Node real com dados descartáveis. O submit remoto abre essa UI em uma
WebView HTTP separada, depois de validar o servidor. A UI real recebe a ponte
`window.personaDesktop` (estado do ambiente, tema e menu). Salva conexões apenas
em dados descartáveis do piloto; não instala runtimes.

## Executar

Requer dependências do repositório já instaladas, artefatos `apps/web/dist`, Node
>= 24 no PATH, Rust >= 1.95 e pré-requisitos nativos Wry (MSVC/WebView2 no Windows,
GTK3/WebKitGTK no Linux, Xcode tools no macOS). Neste diretório:

```sh
cargo check --locked
cargo test --locked
cargo fmt --check
cargo run --locked
```

Smoke nativo, que verifica cookie, navega, fecha e encerra o serviço:

```sh
cargo build --locked
./target/debug/persona-tauri-pilot.exe --smoke-test
./target/debug/persona-tauri-pilot.exe --setup-smoke-test
./target/debug/persona-tauri-pilot.exe --remote-smoke-test
./target/debug/persona-tauri-pilot.exe --switch-smoke-test
./target/debug/persona-tauri-pilot.exe --startup-check=restore
./target/debug/persona-tauri-pilot.exe --startup-check=retry
./native-death.test.ps1
```

Na raiz do repositório:

```sh
node --test --test-concurrency=1 apps/desktop/tauri-pilot/local-session.test.mjs apps/desktop/tauri-pilot/stop-child.test.mjs apps/desktop/tauri-pilot/supervisor.test.mjs apps/desktop/tauri-pilot/service.test.mjs apps/desktop/tauri-pilot/service-transport.test.mjs apps/desktop/tauri-pilot/remote-probe.test.mjs
pnpm exec biome check apps/desktop/tauri-pilot/local-session.mjs apps/desktop/tauri-pilot/local-session.fixture.mjs apps/desktop/tauri-pilot/local-session.test.mjs apps/desktop/tauri-pilot/stop-child.mjs apps/desktop/tauri-pilot/stop-child.test.mjs apps/desktop/tauri-pilot/service.mjs apps/desktop/tauri-pilot/supervisor.mjs apps/desktop/tauri-pilot/silent-supervisor.fixture.mjs apps/desktop/tauri-pilot/remote-probe.mjs apps/desktop/tauri-pilot/remote-probe.test.mjs apps/desktop/tauri-pilot/remote-server.fixture.mjs
```

O executável de desenvolvimento depende das fontes Node deste checkout. Bundles
e instaladores Tauri continuam desativados.

### Pacote portátil de teste para Windows

Com Node 24 ou superior, pnpm e as dependências Cargo e pnpm em cache, execute
na raiz:

```sh
node apps/desktop/tauri-pilot/package-portable.mjs
```

O script não reconstrói nem altera o runtime Electron. Ele compila o executável,
prepara um runtime e a interface a partir do checkout atual sem downloads, e copia
o Node usado na execução para um novo diretório em `target/portable-test-*`.
Requer também os binários Cua correspondentes já instalados no cache local; não
faz download deles. O empacotamento testa o serviço, o probe remoto, o worker e
uma tarefa sintética, além da descoberta das ferramentas de computador. O
pacote pode ser movido, requer WebView2 instalado e mantém dados descartáveis
junto ao executável. É somente para testes, não é instalador nem migra dados reais.
Não conecte credenciais ou dados de produção. O teste do pacote foi feito com Node
26; compatibilidade de empacotamento com Node 24 ainda não foi verificada.

Para experimentar persistência local isolada, inicie o piloto com
`--persistent-local`. Os dados ficam em `target/test-data/pilot-local-v1`, junto
ao executável portátil ou no diretório do piloto de desenvolvimento. Fechar o
serviço normalmente preserva esses dados. Esse perfil não acessa nem migra o
perfil Electron e ainda não substitui backup ou uma instalação de produção.
Uma interrupção incerta mantém um bloqueio para evitar reutilização simultânea;
não o remova sem confirmar que todos os processos responsáveis terminaram.

## Fluxo real e fronteiras

- Rust inicia um supervisor Node próprio. stdin e stderr são **pipes privados
  herdados**, com frames JSON delimitados por newline; stderr é exclusivamente
  transporte, não log. O supervisor só inicia após submit do setup. stdout é descartado. Não rode
  `supervisor.mjs` manualmente redirecionando o protocolo para um terminal/log.
- `supervisor.mjs` reutiliza `service.mjs`. Startup e stop são idempotentes.
  O serviço usa uma configuração exclusiva em `target/test-data/service-*`,
  dados/homes temporários, apenas 127.0.0.1 e porta efêmera. O ambiente do serviço
  não herda configurações Persona, chaves ou homes reais. Rust limpa todo o ambiente
  do supervisor e repassa somente PATH, SystemRoot, WINDIR e SystemDrive quando
  presentes. Não consulta valores de credenciais ou proxies herdados.
- `service-child.mjs` adapta o IPC privado para o `parentPort` esperado e importa
  `apps/api/src/main.ts`. Runtime, autenticação, UI e pairing são os módulos reais.
  Readiness exige anúncio de listen do filho próprio, pairing IPC e uma sessão
  admin autenticada via HTTP; não usa `/health` como prova de identidade.
- A verificação autenticada usa `node:http` com Agent exclusivo e `proxyEnv: {}`,
  nunca fetch/globalAgent. Não segue redirects, limita a resposta a 64 KiB e tem
  timeout de 10 s. Mesmo quando iniciado fora de Rust com NODE_USE_ENV_PROXY e
  proxies configurados, esse pedido permanece direto em 127.0.0.1.
- A credencial passa somente nos canais privados e no cookie nativo. Não vai
  para argv, ambiente, stdout, logs ou JavaScript da WebView. Os scripts Node
  necessariamente a manipulam em memória; isso não injeta credenciais na página.
- A janela HTTP começa oculta em `about:blank`. Rust grava `persona_session` pela API
  nativa: domínio 127.0.0.1, path `/`, HttpOnly, SameSite Strict, sessão sem
  persistência intencional. `set_cookie` apenas enfileira: uma thread separada
  lê `cookies_for_url` e confere valor/HttpOnly/SameSite/path antes de navegar ou
  mostrar. Falha de leitura encerra startup, sem navegação ao serviço.
  Criação, cookies e navegação rodam em worker separado do dono de OwnedProcess.
  O dono monitora bootstrap por canal, com orçamento total de 20 s, inclusive se
  uma chamada nativa não retornar. Cancelamento bloqueia navegações futuras.
- A navegação fica restrita ao esquema, host e porta confirmados; novas janelas
  e pedidos de permissão WebView retornam Deny. `about:blank` só é permitido
  durante bootstrap. A ACL remota concede somente os comandos de desktop descritos
  abaixo; os comandos de setup não são concedidos à janela HTTP.
  ACL IPC vazia não substitui a política de permissões do navegador. Na conexão
  remota, popups HTTPS sem userinfo são abertos pelo sistema e negados como
  janelas internas; os demais popups são negados.
- Fechar a janela oculta a UI e solicita shutdown privado. O transporte entrega
  shutdown uma única vez, mesmo com repetição/disconnect durante inicialização.
  O supervisor aguarda o filho sair antes de limpar seus dados temporários.
  Após 30 s envia SIGTERM ao filho; após outros 5 s envia SIGKILL e aguarda saída.
  Só remove dados após saída normal 0 sem escalada. Saída forçada/anormal retém
  evidência descartável, pois não comprova cleanup do runtime.
  `service.stop()` propaga `{ graceful, cleanup }`. O protocolo terminal inclui
  `mode: graceful|forced` e `cleanup`; parada forçada sai com código 1, nunca 0.
  Rust só aceita término normal com mode graceful e cleanup true.
- A leitura dos pipes roda separadamente. Um deadline independente limita startup
  a 120 s e stop a 40 s, mesmo se nenhum frame/newline chegar. Windows usa um Job
  Object próprio, sem breakaway, atribuído antes do comando start: fallback termina
  supervisor e descendentes, aguardando ActiveProcesses=0. Unix cria grupo próprio
  e envia SIGKILL ao grupo no fallback; aguarda o supervisor. `waitid(WNOWAIT)`
  observa a morte do líder sem reap, preservando a identidade do grupo. finish e
  Drop terminam o grupo antes de reap, inclusive se o líder já morreu. Não
  executamos essa variante Unix nem comprovamos reaping dos descendentes nela.
  Rust não apaga
  diretórios de dados no fallback. Kill-on-job-close limita órfãos no Windows.
- O dono não chama hide/creation/cookies bloqueantes. Após settle dos processos
  próprios, solicita saída e arma um fallback de 2 s para terminar somente o
  próprio processo Tauri se o event loop nativo não responder. Não faz join de
  worker nativo travado nem apresenta esse término como cleanup nativo completo.
- Morte do serviço/supervisor é terminal: Rust oculta/encerra a janela e sai,
  sem restart ou reconexão àquela porta. EOF de Rust pede shutdown do serviço.

## Setup local e IPC

`build.rs` lê `../src/setup.html`, `setup.js`, `setup.css` e os estilos, tokens e
ícone reais de `packages/ui`. Gera somente artefatos ignorados em
`target/setup-assets`; não mantém uma cópia do design. O HTML gerado adapta CSP,
insere `setup-bridge.js` e informa que os dados são descartáveis. O bridge mantém
`window.personaSetup`: state, submit, cancel, docker e progress. Docker retorna
erro explícito. A conexão remota usa o mesmo formulário e contrato de submit,
mas seu probe e sua WebView são separados da janela local de setup.

`AppManifest` enumera sete comandos: quatro de setup e três de desktop. A
capability local concede somente os quatro de setup ao label `setup`, sem
concessão remota. Cada handler também verifica em
Rust o label e a URL nativa exata de `setup.html`. Submit aceita somente o objeto
`{ mode: "local", runtime: "native" }`, sem campos extras. Navegação para qualquer
outra URL, novas janelas e permissões são negadas. CSP usa `frame-src 'none'` e
permite somente `ipc:`/`http://ipc.localhost` em `connect-src` para o transporte
Tauri. A meta CSP original também é adaptada; não depende apenas da config.

O documento local empacotado é **uma unidade de confiança**. Não prometemos
isolar código same-origin que consiga chamar closures do pai. A defesa Tauri
contra iframes usa chave de invoke e bootstrap no frame principal, mas Wry no
Windows injeta scripts em subframes. Veja o
[advisory oficial](https://github.com/tauri-apps/tauri/security/advisories/GHSA-57fm-592m-34r7).
A chave não substitui ACL nem validação nativa de label/URL.

Progresso corresponde a startup real, sessão verificada e janela pronta. O comando
cancel solicita stop ao supervisor próprio e retorna; submit continua pendente
até settle dos processos e então entrega o resultado de cancelamento.
Falhas/cancelamento permitem outra tentativa com novo serviço/diretório temporário.
Se o worker nativo não terminar após cancelamento, o piloto mantém a reserva do
label até que ele termine ou o usuário encerre o aplicativo. Fechar o setup durante
uma conexão ativa apenas oculta a janela.

## Ambientes salvos

O piloto mantém a lista e a seleção em `target/test-data/desktop/instances.json`,
sem ler ou alterar a configuração real do Electron. O menu oferece conexão local,
conexão a outro servidor e seleção de ambientes salvos. A troca encerra a conexão
anterior antes de iniciar a próxima; se a nova conexão falhar, a seleção salva é
preservada, mas a conexão anterior não é reiniciada automaticamente. Os smokes
reinicializam essa lista descartável ao iniciar.

Na inicialização normal, uma conexão remota selecionada é novamente verificada e
aberta. Uma seleção local ou ausente abre o setup. Se a conexão remota falhar, o
setup mostra o erro e permite tentar novamente. Os checks `--startup-check` usam
suas próprias instâncias descartáveis e verificam restauração e retry no Windows.

`--switch-smoke-test` chama o handler do menu para trocar entre ambientes, verifica
o fechamento da janela anterior e lê o arquivo persistido. Não verifica cliques no
menu nativo nem a recuperação visual após falhas.

No Windows, uma segunda execução normal sinaliza a primeira e encerra antes de
carregar ou alterar a lista de ambientes. A primeira solicita que a janela atual
ou o setup seja exibido e receba foco; a ativação efetiva depende das regras do
Windows. Os smokes usam diretórios separados e não participam desse bloqueio.
O teste automatizado verifica a sinalização entre processos e a liberação do
bloqueio, não a ativação visual da janela.

`--setup-smoke-test` executa no WebView2 real: lê DOM/CSS, exige `inline-flex` na
classe `.button` em um host block temporário e token/radius `999px` reais (itens
grid/flex são blockificados), observa negações de
navegação no callback Rust e violação de `frame-src`, verifica erro de argumento
e cancelamento renderizados pelo `setup.js` original, tenta IPC por outra janela
local e pela janela HTTP real e confere a rejeição final e que o contador de
chamadas aceitas pelo validador Rust não mudou. O contador incrementa depois do
check; não distingue rejeição por ACL de entrada no handler seguida de rejeição.
Também chama diretamente o validador nativo com essas janelas. Confirma que o
documento de setup não recebe `window.personaDesktop` e que `desktop_state` é
recusado por ACL nessa janela, e que `desktop_state`/`appearance` respondem na
janela local do app. Depois faz submit
pelo formulário e fecha o serviço próprio. Isso não é teste de input físico nem
revisão visual de pixels. `--smoke-test` e `--native-death-test` fazem submit
automático apenas para os checks anteriores; o fluxo normal aguarda o usuário.

Windows: build, 7 testes Rust e smokes local/setup/remoto passaram. O WebView2 ainda emite
erro 1412 no teardown. Linux/macOS e isolamento contra processo local hostil não
foram verificados. Typecheck e lint globais passaram; o lint manteve sete avisos
preexistentes fora do piloto.

A decisão final de retry/exit e a remoção de active usam o mesmo mutex consultado
pelo fechamento. CloseRequested e ExitRequested compartilham esse protocolo:
ou o evento encontra o worker e ele assume a saída, ou encontra idle e permite
saída imediata. Uma regressão com barreiras força as duas ordens após a leitura
antecipada de closing=false, sem usar sleeps como sincronização.

## Ponte desktop com a UI real

`desktop-bridge.js` é injetado como initialization script nas janelas `pilot` e
`remote` e define `window.personaDesktop` apenas no frame principal
(`window.top === window`). Mantém o contrato que `apps/web` já consome:
`platform`, `state`, `appearance` e `menu`. `prepare` e `maintain` não invocam
comando privilegiado nenhum: devolvem `ok: false` com erro explícito, porque o
piloto não gerencia imagens de Docker nem backup/restauração.

Rust implementa `desktop_state`, `desktop_appearance` e `desktop_menu`. Cada
handler revalida, em Rust, o label da WebView e a origem exata já validada
antes de responder; window não permitida ou URL fora da origem devolve
"Origem da requisição inválida.". `desktop_appearance` aplica a cor de frame
real de `packages/ui` (`themes.light.frame`/`themes.dark.frame`, geradas em
`build.rs`) e `desktop_menu` abre menu nativo com o ambiente atual, Editar,
Visualizar/fullscreen e Sair do Persona.

**A ACL é concedida em runtime, por origem exata.** As janelas do piloto
carregam de `http://127.0.0.1:<porta>` e de origens HTTPS remotas, que o Tauri
trata como conteúdo remoto — uma capability `local: true` nunca se aplica a
elas. Por isso, na conexão validada, o piloto chama `Manager::add_capability`
(com a feature `dynamic-acl`) criando a capability `desktop-ui-<label>` com
`local: false`, `remote: [<origem validada>]`, `webviews: [<label>]` e apenas as
permissões `allow-desktop-state`, `allow-desktop-appearance` e
`allow-desktop-menu`. O padrão de URL é validado antes de ser entregue ao Tauri
porque um padrão inválido entra em pânico durante a resolução. O documento
local de setup não recebe essa capability: `desktop_state` é recusado por ACL
nessa janela, como o smoke confirma.

A validação nativa continua sendo a fronteira real: a capability concede por
origem, e o handler exige label e origem da conexão ativa. O alvo é apagado
quando a janela é fechada ou quando o startup falha.

Limitações em relação ao Electron: o Wry do Tauri 3 alpha não expõe
`windowControlsOverlay`/`setTitleBarOverlay`, então a cor dos símbolos da
titlebar e o inset das janela nativa não são ajustáveis; o menu popup resolve
antes de o usuário fechá-lo, e não há callback de fechamento como no Electron;
"Sair do Persona" encerra o app e o runtime Node local é encerrado pelo Job
Object como parada forçada, deixando os dados descartáveis em
`target/test-data`.

## Conexão remota

`remote-probe.mjs` é um worker Node privado. Ele recebe
`{ mode: "existing", url, name }` pelo stdin herdado e reutiliza
`probeInstance`/`serverOrigin` de `packages/hosting/src/instance.ts`. A política
é a do produto: HTTPS para servidores externos e HTTP somente para loopback;
userinfo, query e fragmento são rejeitados. O probe não segue redirects, tem
timeout de 10 s e pode ser cancelado. URL e nome não entram em argv, ambiente,
stdout ou logs; somente a origem validada retorna pelo pipe privado.

Rust revalida a origem recebida e cria a WebView `remote` com capability
apenas para `state`/`appearance`/`menu` e escopada à origem confirmada; os
comandos de setup e as operações de hospedagem local não ficam
disponíveis nessa janela. Nenhum cookie de sessão local é instalado: a UI
remota preserva o fluxo de pareamento do produto. A WebView é incógnita e não
persiste a sessão do navegador.

`--remote-smoke-test` inicia um serviço Persona descartável, preenche o
formulário real, abre a UI remota, confirma a negação do IPC de setup e de
`desktop_prepare`, a exposição de `desktop_state`, a negação de `desktop_state`
no documento de setup e da
navegação externa, e encerra o servidor. O smoke não abre o navegador pessoal;
a abertura externa de popup HTTPS é coberta pela política unitária, não por
uma execução que acione o sistema. HTTPS com certificado público válido
ainda não foi exercitado contra um servidor real.

## Risco aceito e limitações

**Contrato HTTP loopback equivalente ao Electron, não proteção contra outro
processo local hostil.** Existe corrida entre liberar a reserva de porta e o
bind, entre confirmar readiness e navegar, e entre morte do filho e fechar a
WebView. O número de porta pode ser ocupado por outro processo nesse intervalo;
cookies HTTP são por host, não por porta. Pairing privado e monitoramento não
eliminam essa corrida. Não há promessa de anti-substituição/risco zero.

A CSP de `tauri.conf.json` protege conteúdo incorporado; não alegamos que ela
seja aplicada à resposta HTTP do serviço. A UI HTTP usa as políticas do produto
e o bloqueio de navegação/permissões da WebView. Cookie é HTTP loopback, não TLS.

Identificador próprio `app.persona.tauri-pilot`, WebView incógnita e dados
descartáveis. Não há leitura/migração/limpeza de perfis Electron/Persona reais.
Incógnito não garante zero escrita do motor WebView. Isso **não é sandbox do SO**:
não conecte credenciais, crie Bots nem execute ações de computador em testes de
produção. O teste não dispara modelos ou ações de computador. Morte abrupta pode
deixar dados sob `target/test-data`; remova apenas esse diretório após confirmar
que seus processos terminaram. O encerramento forçado não comprova cleanup.

## Evidência e limites dos checks

- Testes Node reais: pairing privado/autorização por cookie, origem externa
  403, stdout vazio, startup/stop repetidos, EOF durante startup sem ready, morte
  do serviço sem restart, shutdown e remoção dos dados temporários.
- Teste do probe remoto: servidor loopback controlado respondeu à rota
  `/hosting/info`; redirect, schema, status e cancelamento foram rejeitados sem
  seguir redirect, e entrada inválida não chegou à rede. stdout permaneceu vazio.
- Proxy controlado em loopback, NODE_USE_ENV_PROXY=1, HTTP_PROXY/HTTPS_PROXY e
  variantes minúsculas, NO_PROXY vazio e credenciais fictícias: sessão local
  verificada com exatamente um pedido direto e **zero pedidos ao proxy**.
- Teste unitário de escalada simula SIGTERM ignorado: exige SIGKILL e mantém a
  promessa pendente até confirmar exit; não é aceitação nativa Unix.
- Teste Rust com supervisor Node real silencioso antes/depois de ready e frame
  parcial: solicita stop, atinge deadline, termina o Job e confirma zero processos
  ativos antes de remover somente o marcador da fixture. O neto escritor real
  ignora SIGTERM. Teste não alega cleanup do runtime após término forçado.
  A fixture usa grace de stop reduzida a 100 ms; a política real usa 40 s.
- Bootstrap bloqueante simulado em criação e leitura de cookies: o dono atinge
  o orçamento reduzido de 100 ms enquanto o worker permanece bloqueado, termina
  o Job e só então libera a simulação, que confirma cancelamento sem navegação.
  É uma simulação do bloqueio nativo, não reprodução de deadlock do WebView2.
- Regressão Unix adicionada sob cfg(unix): líder sai deixando escritor com pipe
  herdado aberto; Drop deve terminar o grupo e produzir EOF. Não foi executada
  neste host Windows; não alegamos aceitação Unix com base no teste Windows.
- Serviço real forçado: stop idempotente retorna graceful false/cleanup false,
  diretório permanece, transporte anuncia mode forced/cleanup false e sai 1.
- A fixture determinística reproduz os dois handlers do produto e exige marcador
  de cleanup assíncrono completo após exit 0, incluindo disconnect precoce. Não
  instrumenta o cleanup interno do runtime real.
- Smoke Windows executado: leitura nativa do cookie com atributos confirmados,
  navegação loopback Finished e encerramento completo, exit 0. O WebView2 emitiu
  aviso `Failed to unregister class Chrome_WidgetWin_0` / erro 1412 no teardown.
  Isso não é inspeção visual nem prova de renderização React ou interação.
- Smoke remoto Windows executado: formulário real, probe HTTP privado, UI
  remota carregada, `desktop_state` exposto na origem validada, IPC de setup e
  `desktop_prepare` rejeitados, `desktop_state` recusado no documento de setup,
  navegação externa bloqueada e
  servidor descartável encerrado, exit 0. Não abriu navegador pessoal.
- Smoke de setup Windows executado: `desktop_state` e `appearance` respondem na
  janela local, o documento de setup não recebe `window.personaDesktop` e tem
  `desktop_state` recusado por ACL, exit 0.
- `native-death.test.ps1`: exit 0. Após readback nativo do cookie, identifica
  supervisor e filho por parentesco, caminho e proprietário; termina somente
  esse serviço de teste e exige saída terminal Tauri 1 e supervisor encerrado.
  O teste não mede o intervalo até ocultar a janela nem elimina a corrida de porta.
- `cargo check --locked`, `cargo fmt --check`, `cargo test --locked` (7 testes Windows),
  `cargo build --locked` e smokes: exit 0. Testes Node (11), Biome focado,
  `pnpm typecheck` e `pnpm lint`: exit 0; o lint global manteve sete avisos
  preexistentes fora do piloto.
  Testes de morte forçada retêm diretórios descartáveis; não se alega cleanup
  nem se apagam esses dados de evidência.
- Não testados: Linux/macOS, build release/instalador, takeover/ações nativas,
  modelos, restauração após crash e processo local hostil. Owner valida integração
  e uso visível. Nenhuma mudança foi feita no launcher ou API compartilhada.

## APIs fixadas

Pins oficiais: tauri `3.0.0-alpha.6`, tauri-runtime-wry `3.0.0-alpha.6`,
tauri-runtime `3.0.0-alpha.5`, tauri-build `3.0.0-alpha.4`. Cargo.lock fixa o grafo;
serde_json `1.0.154` já existente no grafo agora é dependência direta do protocolo.
Windows `0.62.2` e libc `0.2.191`, já presentes no grafo, são dependências diretas
para ownership/encerramento nativo dos processos, não comandos expostos à WebView.
A feature `dynamic-acl` do tauri (sem crate novo) permite conceder a capability da
interface desktop por origem validada em runtime.

- [Conjunto oficial](https://github.com/tauri-apps/tauri/blob/tauri-v3.0.0-alpha.6/Cargo.toml)
- [Migração oficial v3](https://v3.tauri.app/start/migrate/from-tauri-2/)
- [WebviewWindowBuilder: navegação, popups e permissões](https://docs.rs/tauri/3.0.0-alpha.6/tauri/webview/struct.WebviewWindowBuilder.html)
- [Cookies nativos e ressalva de deadlock Windows](https://docs.rs/tauri/3.0.0-alpha.6/tauri/webview/struct.Webview.html#method.cookies_for_url)
- [Node Agent e proxyEnv](https://nodejs.org/api/http.html#new-agentoptions)
- [Windows Job Objects](https://learn.microsoft.com/en-us/windows/win32/procthread/job-objects)
- [POSIX waitid e WNOWAIT](https://pubs.opengroup.org/onlinepubs/9799919799/functions/waitid.html)
