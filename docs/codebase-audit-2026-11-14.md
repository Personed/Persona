# Auditoria da codebase — 2026-11-14

## Contexto

Auditoria de leitura sobre a `main` local (`560e35c`), 20 commits à frente de
`origin/main` (`d419598`). O objetivo é listar o que pode ser removido,
otimizado ou atualizado, em tarefas pequenas e revisáveis.

Método: `git ls-files`, contagem de linhas e imports, `pnpm outdated`,
`git grep`, inspeção dos workflows e do build atual. Nenhuma alteração de
código foi feita nesta auditoria.

## Estado atual

- Passe 0 concluído em 2026-11-14: CI verde em `9c40e9a`, após corrigir o
  caminho do pnpm, o título do Chromium, o deploy offline e o stage entre drives.
- Tarefas escolhidas do Passe 1 concluídas: AUD-07/08 (`1da052f`), AUD-10
  (`85ff5b9`) e AUD-12 (`38095c5`). AUD-09 e AUD-11 aguardam decisão.
- O working tree mantém alterações locais preexistentes em Tauri, web, docs e
  testes; elas não fazem parte dos commits da auditoria.
- `README.zh-CN.md` foi restaurado e o worktree órfão foi removido.
- `docs/computer-evals.md` existe; não é link quebrado.
- `deploy/NOTICE` atribui `apps/desktop/src/local-stack.ts` ao upstream
  `elie222/rakazo`; o arquivo nunca existiu neste repo.
- Build web atual: 13,6 MB em `apps/web/dist`; 4,28 MB são 98 arquivos da
  fonte Noto Sans SC e 7,54 MB são 12 webp de personagens.

## Achados e plano

Cada tarefa cabe em um PR próprio. `S` = poucos minutos; `M` = algumas horas.
Tarefas marcadas **decisão** precisam de confirmação antes de remover algo.

### Passe 0 — Verdade do repositório e CI

| ID | Tarefa | Evidência | Validação | Risco |
| --- | --- | --- | --- | --- |
| AUD-01 (feito) | Publicar a `main` local e rodar o CI no HEAD `560e35c`. | Publicado; CI verde em `9c40e9a` após corrigir pnpm, título do Chromium, deploy offline e stage entre drives. | `git push origin main`; `gh run watch`. | Publica a migração Tauri. |
| AUD-02 (feito) | Remover o worktree órfão `native-harness-discovery`. | Removido; `git worktree list` mostra só a `main`. | `git worktree list`. | Nenhum. |
| AUD-03 (feito) | Decidir `README.zh-CN.md`: restaurar ou commitar a remoção e ajustar o link do `README.md`. | Restaurado; o link do `README.md:3` funciona. | `git status` limpo; checagem de links. | Remover a tradução reduz cobertura de docs. |
| AUD-04 (feito) | Confirmar a atribuição do `deploy/NOTICE`. | `apps/desktop/src/local-stack.ts` existe no upstream `elie222/rakazo`; nunca existiu neste repo. | Revisão manual; sem alteração necessária. | Nenhum. |
| AUD-05 (absorvido) | Confirmar as falhas antigas de `desktop` e `sandbox` no CI da `main`. | Absorvido por AUD-01: o CI no HEAD atual cobre os mesmos jobs. | Ver AUD-01. | Nenhum. |
| AUD-06 (feito) | Limpar artefatos locais ignorados: `apps/desktop/dist`, `release`, `runtime`, `tauri-pilot`, `test-results`. | Removidos (~34 GB); `tauri-pilot` continha só `gen` e `target`. | `git status`; rebuild pelo CI. | Build local apagado (recuperável). |

### Passe 1 — Remoções

| ID | Tarefa | Evidência | Validação | Risco |
| --- | --- | --- | --- | --- |
| AUD-07 (feito) | Remover `package-portable.mjs` e o fluxo `portable-*` se o instalador NSIS o substituiu. | Removido em `1da052f`; o README documenta só `pnpm dist`. | Suíte desktop e CI. | Fluxo manual de pacote portátil perdido. |
| AUD-08 (feito) | Auditar os 49 scripts `.mjs` do `src-tauri` e remover os que não são usados por build, teste, CI ou README. | Grafo de referências nos 49 scripts; 7 órfãos removidos em `1da052f`. | Suíte desktop e CI. | Smoke manual ainda útil foi removido. |
| AUD-09 (S, decisão) | Revisar a dependência direta `proper-lockfile` em `packages/agent`. | Nenhum import direto; `pnpm why` mostra que ela já vem de `@earendil-works/pi-coding-agent` e está fixada em 4.1.2. | `pnpm why`, build e runtime Docker. | Pode ser um pin intencional; remover a declaração não elimina o pacote. |
| AUD-10 (feito) | Remover exports mortos do `@persona/ui`. | `toast` tinha 0 usos e foi removido em `85ff5b9`; os demais subpaths têm ao menos 1 consumidor. | `pnpm typecheck`, `pnpm build`. | API compartilhada; uso interno confirmado antes. |
| AUD-11 (M, decisão) | Consolidar ícones: `packages/ui/assets/icon.png` (1,47 MB) é a fonte do setup; `src-tauri/icons` (177 KB) serve o instalador. Redimensionar/comprimir a fonte, sem duplicar. | `build.rs` copia `icon.png`; ícones Tauri são separados. | Build desktop, screenshot do setup e instalador. | Ícone errado ou borrado. |
| AUD-12 (feito) | Mover `docs/browser-use-review.md` para `docs/reports/` (ignorado) ou removê-lo se for histórico. | Movido para `docs/reports/browser-use-review-2026-11-12.md` em `38095c5`; não era referenciado. | Checagem de links. | Histórico fora do repositório. |

### Passe 2 — Atualizações

| ID | Tarefa | Evidência | Validação | Risco |
| --- | --- | --- | --- | --- |
| AUD-13 (M) | Atualizar patches/minors seguros: `hono` 4.13.18, `@hono/node-server` 2.1.8, `@modelcontextprotocol/sdk` 1.32.2, `tar` 7.5.23, `vite` 8.3.8, `yaml` 2.9.1, `@biomejs/biome` 2.6.7, `@orpc/*` 1.16.2, `@playwright/test` 1.65.1, `vitest` 5.1.0, `file-type` 22.3.0, `undici` 8.12.4, `fflate` 0.8.3, `@anthropic-ai/claude-agent-sdk` 0.3.325. | `pnpm outdated` (30 entradas). | `pnpm install`, typecheck, lint, testes, build. | Regressões; um PR único e revertível. |
| AUD-14 (M) | Atualizar a família `@earendil-works/*` de 1.4.0 para 1.5.2. | 5 pacotes no mesmo conjunto. | Testes de harness, runtime e build. | Mudanças de protocolo no runtime. |
| AUD-15 (M, separar) | Migrar majors: `@agentclientprotocol/sdk` 1.9 → 2.6, `croner` 10 → 11, `@trycua/cua-driver` 0.37 → 0.49, `agent-browser` 0.39 → 0.43, `@types/node` 24 → 26. | Versões listadas em `pnpm outdated`. | Testes específicos por pacote e CI. | Alto; uma tarefa por pacote. |
| AUD-16 (M, decisão) | Atualizar Tauri alpha e Rust quando houver release estável. | `tauri = 3.0.0-alpha.6`; CI fixa Rust 1.95. | Build desktop e CI. | Alpha instável; não bloquear o restante. |

### Passe 3 — Organização e performance

| ID | Tarefa | Evidência | Validação | Risco |
| --- | --- | --- | --- | --- |
| AUD-17 (M) | Dividir `apps/web/test/chat.spec.ts` (4.103 linhas) por fluxo: chat, detalhes, galeria, composer. | Maior arquivo de teste do repo. | Playwright com shards. | Fixtures compartilhadas. |
| AUD-18 (M) | Dividir `apps/web/test/workspace.spec.ts` (3.089 linhas) por área. | Segundo maior. | Suíte web. | Duplicar setup. |
| AUD-19 (M) | Dividir CSS grande: `chat/style.css` 903, `usage/style.css` 735, `controls.css` 688. | Contagem de linhas. | Screenshots claro/escuro. | Cascata e ordem de import. |
| AUD-20 (M) | Dividir `src-tauri/src/main.rs` (991) e `maintenance.rs` (802) em módulos. | Contagem de linhas. | `cargo fmt --check`, `cargo test`. | Refatoração Rust. |
| AUD-21 (S) | Dividir `translations.ts` (956 linhas) por namespace. | Contagem de linhas. | Testes de i18n. | Chaves duplicadas. |
| AUD-22 (M) | Podar os subsets Noto não usados do pacote distribuído (98 arquivos, 4,28 MB). O CSS já usa `unicode-range`; o ganho é no instalador, não no download por página. | `apps/web/dist/assets`; `@fontsource-variable/noto-sans-sc/index.css` tem 101 `unicode-range`. | Build, inspeção visual e teste com texto CJK. | Glifos CJK faltando. |
| AUD-23 (S) | Comprimir personagens webp (7,54 MB) e `icon.png`. | Tamanhos em `packages/ui/assets`. | Inspeção visual e build. | Perda de qualidade. |
| AUD-24 (S) | Centralizar listas de exclusão do CI em scripts npm e adicionar cache de pnpm/Cargo. | `checks.yml` repete exclusões; `computer-input.yml` repete a lista. | Rodar CI. | Cobertura de testes se a lista divergir. |
| AUD-25 (S) | Adicionar checagem de links de docs e de arquivos órfãos no CI. | Não há checagem automática de links nem do grafo de entradas locais; os órfãos conhecidos foram removidos em AUD-07/08. | Rodar o script local e no CI. | Ruído se a heurística for ampla. |

## Ordem recomendada

1. **Passe 0 concluído:** AUD-01, AUD-02, AUD-03 e AUD-06 feitos; AUD-04 verificado; AUD-05 absorvido por AUD-01.
2. **Passe 1 — remoções escolhidas concluídas:** AUD-07, AUD-08, AUD-10 e
   AUD-12; AUD-09 e AUD-11 aguardam decisão.
3. **Atualizar em blocos revertíveis:** AUD-13, AUD-14, depois AUD-15.
4. **Organizar sem mudar comportamento:** AUD-17 a AUD-21.
5. **Otimizar o produto:** AUD-22, AUD-23.
6. **Automatizar a prevenção:** AUD-24, AUD-25.

## Não remover sem análise

- `apps/desktop/src/setup.*`: usado por `build.rs` e `setup.rs`.
- `apps/desktop/src-tauri/src/setup_smoke.rs`, `remote_smoke.rs`,
  `maintenance_smoke.rs`: são testes de contrato nativos.
- `packages/agent/src/harness/*`: runtime principal; mudanças exigem evals.
- `docs/computer-evals.md`, `docs/browser-use.md`, `docs/harness-context.md`:
  referenciados pelo `AGENTS.md`.
- `deploy/workspace.Dockerfile`: instala dependências usadas pelo runtime,
  mesmo quando não aparecem como imports TypeScript.

## Critério de conclusão

Cada tarefa termina com um commit pequeno, o check focado verde e uma nota do
que ficou sem verificar. Tarefas que mudam runtime, persistência ou instalador
exigem CI nas três plataformas antes do merge.
