# Persona Tauri pilot — integração temporária

Piloto de desenvolvimento isolado. Não substitui Electron nem migra perfis
automaticamente; a paridade ainda não foi comprovada. Abre o setup existente em
uma WebView local separada. A execução nativa padrão usa dados temporários; a
opção Docker usa `LocalStack` com um perfil persistente exclusivo do piloto. O
submit remoto abre a UI em uma WebView HTTP separada, depois de validar o servidor.
A UI real recebe `window.personaDesktop` (estado do ambiente, tema e menu). O
piloto não instala Docker nem runtimes.

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

`--maintenance-file-dialog-smoke-test` exercita os diálogos nativos de salvar e
abrir sem iniciar supervisor ou worker. Rode somente uma cópia descartável que
contenha `portable-test.json`; defina `PERSONA_TAURI_SMOKE_BACKUP_PATH` para um
caminho sintético inexistente e `PERSONA_TAURI_SMOKE_RESTORE_PATH` para um arquivo
sintético existente, ambos na raiz dessa cópia. O smoke exige que os caminhos
retornados coincidam e não lê nem grava o conteúdo selecionado.

Na raiz do repositório:

```sh
node --test --test-concurrency=1 apps/desktop/tauri-pilot/local-session.test.mjs apps/desktop/tauri-pilot/stop-child.test.mjs apps/desktop/tauri-pilot/supervisor.test.mjs apps/desktop/tauri-pilot/service.test.mjs apps/desktop/tauri-pilot/service-transport.test.mjs apps/desktop/tauri-pilot/remote-probe.test.mjs apps/desktop/tauri-pilot/offline-profile.test.mjs
pnpm exec biome check apps/desktop/tauri-pilot/local-session.mjs apps/desktop/tauri-pilot/local-session.fixture.mjs apps/desktop/tauri-pilot/local-session.test.mjs apps/desktop/tauri-pilot/stop-child.mjs apps/desktop/tauri-pilot/stop-child.test.mjs apps/desktop/tauri-pilot/service.mjs apps/desktop/tauri-pilot/supervisor.mjs apps/desktop/tauri-pilot/silent-supervisor.fixture.mjs apps/desktop/tauri-pilot/remote-probe.mjs apps/desktop/tauri-pilot/remote-probe.test.mjs apps/desktop/tauri-pilot/remote-server.fixture.mjs
node --test apps/desktop/tauri-pilot/docker-profile.test.mjs apps/desktop/tauri-pilot/docker-service.test.mjs
node --test apps/desktop/tauri-pilot/maintenance.test.mjs apps/desktop/tauri-pilot/maintenance-worker.test.mjs
pnpm vitest run packages/hosting/test/stack.test.ts packages/hosting/test/work-images.test.ts
pnpm vitest run packages/hosting/test/work-backup.test.ts
```

O executável de desenvolvimento depende das fontes Node deste checkout. O build
normal continua sem bundle; o instalador usa configuração separada.

## Instalador Windows isolado (NSIS)

Na raiz: `node apps/desktop/tauri-pilot/build-installer.mjs`.
Usa o CLI fixado em `3.0.0-alpha.6`, Cargo `--locked --offline` e o Node >=24
que executa o script. Recria somente `target/installer-resources`, inclui runtime,
Node e importador, e gera `target/release/bundle/nsis/*-setup.exe`.
Usa ZLIB para evitar a demora extrema de LZMA nesse runtime grande.
O primeiro empacotamento pode baixar ferramentas NSIS com hash validado pelo CLI.
WebView2 deve estar instalado. O instalador não pede elevação (`currentUser`).

Recursos instalados ficam junto ao executável; dados ficam exclusivamente em
`%LOCALAPPDATA%/app.persona.tauri-pilot`. O modo portátil continua usando
`target/test-data` junto ao executável, e desenvolvimento usa o checkout.
`--persistent-local` ativa o perfil nativo persistente. A execução Docker usa
sempre `pilot-docker-v1` (ou o perfil Docker selecionado) sob o mesmo diretório
exclusivo do piloto; ela não lê `pilot-local-*`. A importação nativa continua
recusando sobrescrever um perfil existente.
Não há descoberta ou importação automática de instalações reais.
O desinstalador oferece exclusão opcional dos dados do piloto; não selecione essa
opção se deseja manter o perfil.

Após gerar o NSIS, o script executa o binário release com os recursos staged e
`LOCALAPPDATA` sintético para verificar o ciclo de serviço/UI, sem instalar ou
alterar registro. Fechar uma janela normal apenas a oculta. O menu da bandeja
reabre a janela ou encerra explicitamente o app pelo mesmo caminho de shutdown
do serviço. Os modos smoke/startup/aceitação mantêm o fechamento e shutdown
anteriores, sem bandeja.

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

O smoke cria também um Bot sintético pelo serviço empacotado, envia uma mensagem
via API autenticada e verifica a resposta e o histórico após reinício. Um Harness
ACP de teste responde localmente; não são feitas chamadas a modelos reais.

`node --test apps/desktop/tauri-pilot/offline-profile.test.mjs` verifica uma
importação offline de backup sintético: autenticação, credencial criptografada,
Bot e histórico continuam acessíveis após reiniciar o serviço e a origem não
muda. O teste não abre o Electron nem migra um perfil real.

Para experimentar persistência nativa isolada, inicie o piloto com
`--persistent-local`; os dados ficam em `target/test-data/pilot-local-v1` no
ambiente de desenvolvimento/portátil. Docker persiste em
`target/test-data/pilot-docker-v1` (ou `%LOCALAPPDATA%/app.persona.tauri-pilot/pilot-docker-v1`
no instalador), com ponteiro e lock próprios. A seleção Docker é restaurada no
próximo início. Cancelamento e parada atuam somente no projeto Compose do perfil;
se a saída dos contêineres não puder ser confirmada, o lock permanece e bloqueia
reutilização. Não remova esse lock manualmente. O perfil não acessa nem migra
dados do Electron.

Para importar um backup cifrado em um perfil ainda vazio, execute o comando
explícito:

```powershell
.\persona-tauri-pilot.exe --import-backup C:\caminho\backup.persona
```

A senha é lida sem eco pelo console e não entra em argumentos, ambiente ou logs.
Após a importação bem-sucedida, execute o mesmo executável novamente com
`--persistent-local` para abrir o perfil importado.
A origem permanece intacta; a reversão é abrir o Electron original ou remover
somente a cópia do piloto depois que seus processos terminarem. O importador
aceita computador principal local e registros de computador locais. Backups com
computador principal/controller ou registros isolados são recusados antes de
publicar o perfil. Um template local `workComputer` Docker incluído no formato
de backup v2 é aceito: o volume da origem é montado somente para leitura e a
restauração cria um volume novo; a origem não é reutilizada nem alterada. O
Docker precisa estar disponível ao criar o backup e ao restaurar um volume
arquivado. Templates Docker remotos ainda são recusados. O importador não copia
homes de contas nativas, credenciais da CLI ou outros dados fora do backup; ele
não abre perfis automaticamente.

## Fluxo real e fronteiras

- Rust inicia um supervisor Node próprio. stdin e stderr são **pipes privados
  herdados**, com frames JSON delimitados por newline; stderr é exclusivamente
  transporte, não log. O supervisor só inicia após submit do setup. stdout é descartado. Não rode
  `supervisor.mjs` manualmente redirecionando o protocolo para um terminal/log.
- `supervisor.mjs` roteia execução nativa temporária por `service.mjs` e execução
  Docker persistente por `LocalStack` compartilhado. O modo nativo usa configuração
  exclusiva em `target/test-data/service-*`, dados/homes temporários, somente
  127.0.0.1 e porta loopback alta. Docker usa Compose e arquivos somente do perfil
  `pilot-docker-*`; não reutiliza o namespace nativo. Rust limpa todo o ambiente
  do supervisor e repassa somente PATH, SystemRoot, WINDIR e SystemDrive quando
  presentes. Não consulta valores de credenciais ou proxies herdados.
- `service-child.mjs` adapta o IPC privado do runtime nativo para o `parentPort` esperado e importa
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
`window.personaSetup`: state, submit, cancel, docker e progress. Submit local
aceita somente `{ mode: "local", runtime: "native" }` ou
`{ mode: "local", runtime: "docker" }`. Docker não é instalado pelo piloto; se
indisponível, a interface orienta o usuário a iniciá-lo ou instalá-lo manualmente.
A conexão remota usa o mesmo formulário e contrato de submit,
mas seu probe e sua WebView são separados da janela local de setup.

`AppManifest` enumera sete comandos: quatro de setup e três de desktop. A
capability local concede somente os quatro de setup ao label `setup`, sem
concessão remota. Cada handler também verifica em
Rust o label e a URL nativa exata de `setup.html`. Submit aceita somente os dois
objetos locais acima, sem campos extras. Navegação para qualquer
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
Falhas/cancelamento permitem outra tentativa. Perfis Docker persistentes usam o
mesmo diretório isolado e conservam lock quando a parada fica incerta.
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

Na inicialização normal, uma conexão remota selecionada ou um runtime Docker local
selecionado é novamente verificado e aberto. Uma seleção nativa local ou ausente
abre o setup. Se a conexão falhar, o setup mostra o erro e permite tentar
novamente. Os checks `--startup-check` usam
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
`platform`, `state`, `appearance` e `menu`. `prepare` não executa preparação
pela página HTTP. `maintain` apenas abre a janela nativa empacotada de
confirmação; a página HTTP não pode escolher ação/arquivo, fornecer senha nem
iniciar a operação. O usuário escolhe tudo e confirma nessa janela local.

Backup e restauração exigem um perfil local persistente Windows e confirmação
na janela empacotada; a página HTTP só pode abrir essa janela. O serviço precisa
encerrar normalmente antes do worker. O backup fica fora do perfil. Restauração
usa um perfil novo `pilot-local-restore-<UUID v4>` ou
`pilot-docker-restore-<UUID v4>` e troca atomicamente somente o ponteiro do
runtime correspondente; o perfil anterior permanece intacto em falhas. Docker
valida configuração, imagens, template e computadores salvos antes de criar
volumes. Volumes do template e dos computadores compatíveis são remapeados para
nomes novos. A aceitação com daemon Docker real continua pendente. O worker recebe
a senha e o runtime somente pelo pipe privado, depois da confirmação nativa e da
escolha nativa do arquivo. Depois do envio, o app aguarda o resultado e a saída
reais do worker e não promete cancelamento ou rollback.

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
`allow-desktop-menu` e `allow-desktop-maintain`. O comando de manutenção apenas
abre a confirmação nativa e valida novamente que o ambiente selecionado é local;
os comandos que recebem senha e executam trabalho ficam disponíveis somente na
capability local da janela `maintenance`, com origem e label exatos. O padrão de URL é validado antes de ser entregue ao Tauri
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
descartáveis por padrão. Não há leitura automática, migração ou limpeza de
perfis Electron/Persona reais. O único acesso ao conteúdo de uma origem é o
importador explícito de um backup escolhido pelo usuário, que publica somente
uma cópia isolada e preserva a origem. Incógnito não garante zero escrita do
motor WebView. Isso **não é sandbox do SO**. O piloto ainda é de teste: não
conecte credenciais de produção nem execute ações de computador antes de a
migração real ser validada com Docker real. Morte abrupta pode deixar dados sob
`target/test-data`; remova apenas
esse diretório após confirmar que seus processos terminaram. O encerramento
forçado não comprova cleanup.

## Evidência e limites dos checks

- Testes Node reais: pairing privado/autorização por cookie, origem externa
  403, stdout vazio, startup/stop repetidos, EOF durante startup sem ready, morte
  do serviço sem restart, shutdown e remoção dos dados temporários.
- Backup sintético cifrado: exportado pela rotina real, importado pelo CLI do
  executável empacotado, aberto pelo serviço empacotado e reiniciado; o
  dispositivo original autenticou, a credencial cifrada foi descriptografada e
  o histórico permaneceu. Senha errada, computador isolado e destino existente
  não publicaram um perfil parcial. Nenhum modelo ou perfil real foi usado.
- Template local `workComputer` Docker: um teste sintético do comando Docker
  verificou montagem somente leitura da origem, criação de volume novo na
  restauração, preservação dos arquivos e não reutilização da origem. O daemon
  Docker real e volumes do usuário não foram acessados.
- Runtime Tauri Docker: perfil, lock, cancelamento, encaminhamento de progresso,
  pairing loopback, roteamento do supervisor e shutdown verificado têm testes
  sintéticos. O daemon não estava disponível neste host; não houve pull, build,
  criação de contêineres ou inspeção de volumes reais. A aceitação com daemon
  autorizado permanece pendente.
- Backup/restauração Docker: além dos testes sintéticos de arquivo de volume, uma
  execução isolada da camada real de manutenção criou um backup cifrado, recusou
  senha incorreta sem trocar a seleção e restaurou o perfil em um destino novo,
  remapeando ID e volume do template. Um `docker.exe` sintético estrito emulou
  somente `volume ls`, `container ls`, `volume create` e `run` com `tar cf/xf`,
  transferindo um tar de fixture e falhando para qualquer outro comando. Nenhum
  daemon, volume ou contêiner real foi acessado; a execução verifica o fluxo de
  arquivo/cifra/publicação, não o Docker real nem o fluxo visual nativo ponta a
  ponta. A janela empacotada continua como única entrada de produção para
  manutenção.
- Smoke de seletor nativo Windows concluído em cópia portátil descartável: os
  diálogos “Salvar como” e “Abrir” retornaram exatamente os caminhos de fixture
  esperados. O backup de saída não foi criado, o arquivo de entrada permaneceu
  intacto e nenhum worker/operação foi despachado. UIA usou um `AGENT_DESKTOP_HOME`
  temporário com DACL restrita; nenhuma ACL do app foi alterada. O WebView2 ainda
  registrou o aviso de teardown 1412.
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
