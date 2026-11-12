# Rebrand from Domos to Persona

Status: adopted.

The product, its packages, and every runtime identifier are renamed from Domos to Persona. There is no installed base to preserve, so the rename ships without a compatibility layer: no legacy env fallbacks, no dual headers, no first-run data migration, no alias packages. Old identifiers disappear from the tree.

## Target values

| Layer | From | To |
| --- | --- | --- |
| Display name | Domos | Persona |
| npm scope | `@domos/*` | `@persona/*` |
| Root package | `domos` | `persona` |
| Config file | `domos.config.json` | `persona.config.json` |
| Environment | `DOMOS_*` | `PERSONA_*` |
| Work state directory | `~/.domos` | `~/.persona` |
| Backup extension | `*.domos` | `*.persona` |
| Desktop app id | `app.domos.desktop` | `app.persona.desktop` |
| Controller headers | `x-domos-bot-id`, `x-domos-daemon-token` | `x-persona-*` |
| MCP tool prefix | `domos__` | `persona__` |
| Images, volumes, units | `domos-*`, `domos-computer-egress` | `persona-*` |

Brand copy covers both web locales and every desktop surface. `docs/assets/domos-hero.jpg` is replaced, not only renamed.

## Order

Each step is one commit that leaves the tree green. Typecheck and the affected tests run before the next step.

1. **Package scope.** `@domos/*` to `@persona/*`, root name, `pnpm-workspace.yaml`, lockfile, CI filters, every importer.
2. **Config file.** `git mv` `domos.config.json` and `domos.config.example.json`, then `deploy/Dockerfile`, `apps/desktop/build.mjs`, install, stack, maintenance restore, fixtures.
3. **Environment.** `DOMOS_*` in compose, workflows, `install.sh`, `deploy/README.md`, test configs.
4. **Wire and host identity.** Controller headers, MCP tool prefix, `appId`, cursor theme id, systemd unit, AppArmor profile, egress script path, image and volume names in `release.json` and compose.
5. **Work state directory.** `~/.domos` in engines, runtimes, cursor themes, skills, browser state, and the `.domos-instructions.md` / `.domos-mcp.json` / `.domos-tools.json` work files.
6. **Backup extension.** `.persona` in desktop save dialogs and hosting backup paths.
7. **Brand copy and docs.** Web copy in both locales, desktop tray, menus, setup, dialogs, `README`, `README.zh-CN`, `docs/`, `deploy/README.md`, `AGENTS.md`, icons, hero image.
8. **Release.** Published tag, GitHub repository name, CI artifact names, a clean-machine install from the release rather than the checkout.

## Completion check

`git grep -i domos` returns nothing outside this file, the historical reports under `docs/reports/`, and the gate that runs it. That grep runs as one CI step in `checks.yml`; a rename is complete when it passes, not when the diff looks right.

The remaining verification is the existing set: `pnpm typecheck`, `pnpm lint`, `pnpm test`, a desktop end-to-end run on Windows, the container tests, and the `.github/workflows/checks.yml` matrix including `computer-input.yml`. A green typecheck does not prove a host install works; the release install test does.

## Notes

Sequence matters in two places. Rename the scope before touching anything imported, so the compiler locates the work. Rename the controller headers together with the code on both sides; controller and app ship together, and a half-renamed pair fails pairing silently.

The MCP prefix appears in saved native tool catalogs and in prompts that name `domos__` tools. Regenerating a catalog is enough; there is no catalog to keep because nothing is deployed.

`AGENTS.md` describes current modules and contracts. Update it in the same commit as step 7, not before.
