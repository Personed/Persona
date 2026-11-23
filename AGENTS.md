# Persona

Persona provides persistent AI companions with memory, tools, and work computers.
Users choose their models and deployment: local, self-hosted, or cloud.
Pi Durable 1.4.0 stores Bot tasks. Native Harness processes own model loops. Persona owns product capabilities.

## Architecture and ownership

- The client device, Persona service computer, and Bot work computer are separate roles. They can be on different computers.
- Commands, files, and desktop actions use the Bot's bound work computer. Do not assume that it is the client or service computer.
- Persona owns Bot identity, full chat history, memories, and computer permissions. Native Harness accounts use the work computer's OS user and configuration. Preserve retained service credentials during migration.
- Memory records belong to one Bot unless the user shares them. Keep profile snapshots stable for each Durable task. Route every Harness through the shared memory tools. Background review uses a separate native session and cannot change user-authored records.
- Durable owns input admission and persistent tasks. Native Harness processes own generation and model context. Adapters coordinate cancellation and recovery through public protocols. Observe uncertain results before more mutations.
- Reuse the work computer's CLI before installing a version in its Persona runtime directory. Inspect, login, and execute with the same program and OS user. Switch programs and Harness only while the Bot is idle.
- Native CLI transports own model HTTP. Use Photon and model image limits at the attachment boundary. Preserve original evidence and include coordinate mapping when resizing observations.
- Keep system instructions stable across tool rounds. Read current time through routine_list. Bound tool images in model requests while retaining full evidence and user attachments. See [Harness context](docs/harness-context.md).
- Send chat, additional input, routines, API calls, and CLI calls through Runtime.submit and the same Durable conversation. Keep request IDs stable through retries.
- Keep model context separate from displayed history. Read [docs/harness-context.md](docs/harness-context.md) before changing history, migration, or compaction.
- Keep computer rules in shared services. Persist tool results. Do not replay mutations whose outcomes are uncertain.
- Native computer calls use the shared ComputerRuntime and a task-scoped intent/result journal. Other product tools use Durable ToolTask records. Keep raw Cua MCP results and error screenshots. Use query_result with a result handle and JMESPath query for bounded result reads. Partial results include their shape and a valid query example.
- Verify product-tool discovery before native task dispatch. Pi declares product tools directly through native MCP. Native tool_search discovers other MCP tools. Read [Harness context](docs/harness-context.md) before changing tool bridges or readiness checks.

| Location | Responsibility |
| --- | --- |
| `apps/api` | HTTP and RPC boundaries, request handling, and service composition. |
| `apps/web` | Browser UI, client state, and user interactions. |
| `apps/desktop` | Tauri desktop lifecycle, native IPC boundary, setup, and packaging. |
| `apps/desktop/src-tauri` | Tauri 3 shell, Node supervisor, native dialogs, persistent profiles, and Windows installer. |
| `apps/controller` | Authenticated control service for remote work sandboxes. |
| `apps/cli` | Command-line entry points, hosting, and eval commands. |
| `packages/agent` | Bots, durable tasks, chats, memory, files, and work environments. |
| `packages/agent/src/runtime` | Durable admission, native task coordination, storage migration, and full chat projection. |
| `packages/agent/src/harness` | Native drivers, CLI discovery and login, work-process services, bounded history reads, and usage records. |
| `packages/agent/src/mcp` | Managed connection metadata, application catalog, Bot grants, and encrypted credential references. |
| `packages/agent/src/computer` | Computer bindings, shared tools, control ownership, viewers, and optional Jev assistance. |
| `packages/hosting` | Hosting setup, credentials, device sessions, installation, and service lifecycle. |
| `packages/ui` | Shared controls, theme tokens, and avatars. |
| `cmd/persona` | Go binary entry point. Subcommands `serve`, `tui`, `chat`, `worker`, `controller`, `migrate`, and `version`. |
| `internal/` | Go packages, one domain each. `store`, `engine`, `tools`, `computer`, and `hosting` hold the shared interfaces. |
| `contracts/api` | Frozen API wire contract and recorded fixtures for the Go service. |

## Go migration

- The root Go module `github.com/Personed/Persona` replaces the TypeScript backend piece by piece. Both coexist until the TypeScript backend is removed. See [contracts/api/README.md](contracts/api/README.md).
- `cmd/persona` stays a thin dispatcher into `internal/cli`. Put behavior in the owning `internal/` package.
- The interfaces in `internal/store`, `internal/engine`, `internal/tools`, `internal/computer`, and `internal/hosting` are the boundaries between migration pieces. Change one only with its callers and mocks in the same change.
- `internal/store` records mirror pi-durable so existing databases import without translation. Pi is the only engine and runs as `pi --mode rpc` over JSONL on stdio. Pi has no Go SDK. An idle Bot has no engine process.
- The Go service serves the oRPC wire in `contracts/api` unchanged: paths, input validation, output shapes, error codes, and HTTP statuses. Regenerate `procedures.json` and the fixtures in the same change as any TypeScript API change.
- `internal/mcpserver` serves the `internal/tools` registry to Pi as one loopback MCP endpoint per Bot task. Add product tools by registering them in the registry; mutating tools set `Mutates` so calls pass the shared approver. A call's `CallID` is its MCP session and JSON-RPC request ID, and a redelivered request gets the recorded outcome. `internal/store/storetest` is the in-memory store for tests until the SQLite store lands. The gateway bounds each result's text for the model, holds the latest complete results, and serves `query_result` over them itself. `internal/history` owns `conversation_read`; a Bot's ID is its conversation ID.
- `internal/memory` owns background review. The runtime counts answered turns with `TurnCompleted` and runs a due review with `Review` as the Bot's only task: a separate native session, memory tools only, at most 12 calls and 120 seconds. A review task whose prompt was sent is never sent again.
- `internal/delivery` owns what a product tool shows in chat: `Deliver` appends one tool-result message per call (notice or files), hidden during conditional routine runs. `attach_file` snapshots a workspace file from the Bot's work computer into the Files root in the TypeScript artifact layout. It refuses a path through a symbolic link, a file with more than one hard link, and a path whose `computer.FileInfo` reports no link count; a Target fills `Symlink` and `Links` without following links. Updates and answers stay native assistant messages; do not add a message-sending tool.
- `internal/agent` owns Bots, the chat projection, the chat runtime, and Bot files, as `packages/agent` does. `Runtime.Submit` admits chat input as a submission and a task in one commit; a per-Bot worker runs the tasks in order, each in an `internal/engine` session that closes when the task ends, and `Stop` aborts the session and settles the turn without waiting for it. It reads and writes the pi-durable and Persona documents (`persona.bots`, `pi.agent`, `pi.live`, `persona.engine`) by kind and scope through `Harness.Commit`, and keeps files in the TypeScript `<database>.files` layout under `persona-go.sqlite.files` until the importer copies the TypeScript history. `persona serve` without a hosting config still runs behind the hosted origin guard, as the TypeScript service always does. `internal/server.RegisterAgent` serves its procedures, and chat input only when given a Runtime (`persona serve -pi-extension`); `internal/server/contract_test.go` requires every fixture of a registered procedure to match, except the listed skips.
- The `internal/agent` runtime opens one `internal/mcpserver` gateway per native task and passes its URL as `PERSONA_PI_MCP_URL`. `internal/engine/pi` refuses a session without it, because `pi-tools.mjs` installs its approval hook only after reading that manifest. Pi approval requests that cannot be answered yet are cancelled.
- `internal/routines` owns the `persona.routines` document and the `routine_*` tools. Route routine changes through its Service. It sets `enabled` and `nextAt` and leaves `taskId` unset; the routine runner starts tasks for enabled routines and sets `Service.RunNow` for `run_now`, which starts one task per request ID.
- `internal/approval` is the shared approval flow. Pass its `Approver` to `internal/mcpserver`; routine changes, computer actions, and engine requests call `Request`. Pending requests stay in the `persona.computer-requests` document and each settled request becomes one `persona.engine-message` entry. Only requests marked `Automatic` skip the question in a Bot's `auto` approval mode; product tool calls always ask.
- Use the Go standard library. Add a module dependency only when a piece needs it, and keep `go.mod` on the Go version CI installs.

## Computer Use contracts

- Reuse Cua for desktop execution and the existing browser service for DOM/CDP operations. Keep platform details at these boundaries.
- Browser viewing uses the Agent browser viewport stream and browser input commands. Cua owns desktop viewing and input. Share display control only for a confirmed headed browser. Keep the identity header fixed across Bot, Computer, Browser, and Work tabs. Use the native Fullscreen API for the screen and toolbar. See [Browser Use](docs/browser-use.md).
- Expose native desktop actions, batches, and scripts with their original parameters. The Agent selects the tool and requests observations. Check a state transition before dependent actions.
- Keep desktop operation guidance in shared computer instructions. Computer boundary sections report environment and permissions. Native schemas and get_guide own syntax. Keep window focus separate from action delivery mode. Match snapshot controls to the intended role and label.
- Bind actions and observation to the same confirmed target. Cua owns native sessions. Forward the Agent's parameters unchanged. Discover a new target when a dialog or window changes.
- Distinguish action delivery, action verification, observation, and user-task completion. A successful call or screenshot alone does not prove task completion.
- If an action ran but observation failed, preserve the execution record and recover observation. Do not replay an action whose outcome is uncertain.
- Preserve the saved native tool catalog, including output schemas and annotations. Deliver structured metadata in supported text content and retain screenshots with their current coordinates. Put Persona status outside strict native structured output.
- Preserve cancellation for direct actions, batches, and scripts. Request observation explicitly when an outcome is unknown.
- Preserve approval, cancellation, and user takeover on every execution path, including Jev assistance.
- Route ordinary computer mutations through the shared approval callback. Browser observation tools use an explicit allowlist; arbitrary JavaScript requires approval.
- Keep dispatched browser work alive until it settles. Cancel queued work. Preserve the page during takeover. Persist uncertain execution across worker restarts, and require explicit browser restart before more mutations.
- Share control between a headed browser and its desktop. Isolate control only after the worker confirms a headless browser. Wait for human input to finish before returning control.
- Route Agent handoff requests through the shared `computer_request_takeover` tool and stored chat cards. Complete only the live request whose viewer owns control. Closing a viewer does not complete a request.
- In Go, `internal/takeover` serves `computer_request_takeover` without `Mutates`. The computer runtime supplies its `Handoff`, which owns the stored chat card, the viewer lease, and completion.
- Reserve a viewer's lease before takeover waits. Keep its frames and acknowledgements live during the grant. Allow input only after the grant completes. Reuse concurrent requests from that viewer and release its reservation when it closes.
- Browser navigation and known page actions return bounded observations. Batch known continuous browser actions with `agent_browser_run_steps`. Use `agent_browser_observe` after takeover or unclear screenshots. Preserve delivered actions when observation fails. Do not infer missing-content causes from blank space or document readiness.
- Browser mode belongs to the shared work-browser service. Change it only through a confirmed restart. Reuse the Bot profile. Hold browser and desktop control during the change. Report observed access evidence; do not hardcode website fallbacks or open personal browsers.
- For viewer input, layout, or reconnect changes, follow [docs/browser-use.md](docs/browser-use.md). Test clicks through the scaled image and read back the real page. Scripted focus and mocked RPC output do not verify takeover.
- Preserve native action results when observation or evidence storage fails. Keep runtime browser arguments in Persona, and apply text limits at the browser boundary. See [docs/browser-use.md](docs/browser-use.md).
- Cua sessions own desktop cursors and motion. Use the native cursor renderer and timing. Select a native theme from the current Bot avatar color. Retain the native soft glow and reduced-motion policy.
- Track physical pixels, logical points, and screenshot coordinates explicitly at input and viewing boundaries.

## Change discipline

- Read the affected code and its callers before editing. Make the smallest complete change that satisfies the request.
- Reuse existing services, controls, dependencies, and platform APIs. Add an abstraction or dependency only when the current task needs it.
- Keep each module responsible for one coherent area. Put a fix in the shared owner rather than adding parallel paths for each caller.
- Remove unused code, obsolete branches, and tests that no longer apply. Keep validation, cancellation, and error handling needed by active callers.
- Keep tests proportional to behavior. Extend the existing relevant test where possible. Replace stale cases rather than stacking duplicate cases.
- Use comments for non-obvious domain or OS constraints. Keep change history in Git.
- Reuse `packages/ui` controls and tokens. Use SearchField for search inputs and InlineNotice for lightweight events. Keep search logic and event state in their feature owners. Share navigation styles and Dialog footer actions. Build visual hierarchy with spacing and typography. Add labels and descriptions only when they provide needed information.
- Stop revokes the active turn and terminates its native Harness process tree. Settle the UI and submissions without joining Durable cleanup. Retain control barriers for issued computer actions until they settle. Ignore events from stopped turns.
- Work projects native Harness events into per-execution cards and the shared record dialog. Capture receipt times before queued processing. Batch changed messages through existing storage. Event delivery does not await display writes. Persist native session acceptance before dispatch continues. Preserve native IDs, structured tool results, progress, subagent ancestry, plans, reasoning availability, stop confirmation, and original evidence. Live events and native history reuse the existing adapters and EngineProjection. Recovery only fills records. Missing native times and reasoning stay unknown. Resume native sessions with unreceived messages. Rebuilt sessions receive chat text; tool history is omitted by default. Optional tool summaries have at most 20 entries of 120 characters each. See [Harness context](docs/harness-context.md).
- Managed MCP connections use one shared catalog and the existing encrypted credential store. Bot grants and tool ranges apply before approval and before each call. Native MCP stays read-only and keeps its own credentials. Mount managed connections through the existing worker, gateway and Harness adapters. Use the official MCP SDK for transport and OAuth. Test and call from the saved work computer. Do not advertise unsupported client capabilities.
- Native Harness catalogs own native skills. Show them read-only with their reported source. Managed skill ZIP packages use the existing Files store and immutable content hashes. Load supporting files into the bound work directory through skill_read. Keep same-name native skills authoritative. See [Bot work](docs/decisions/0001-bot-work.md).
- Routine changes from chat use shared approval cards. Routine runs use Runtime.submit and native Work records. Track active execution and human waits separately. Pause the execution deadline while awaiting an answer or approval. Expiry uses the shared Stop path.
- Conditional routines submit a candidate through routine_report. Commit its baseline and delivery only after the same task version succeeds. Keep quiet work in Work, retain human requests and failures in chat, and queue new chat input as a separate turn.
- Keep workspace navigation in the contact column. The centered Bot capsule opens details. Put model and context controls in the Bot tab. Reserve native window controls and keep interactive controls outside drag regions. See [Workspace UI](docs/decisions/0005-workspace-ui.md).
- Render native questions and permission requests in the chat transcript. Store answers and terminal states through the shared ComputerRuntime. Derive takeover state from the viewer control lease. Reuse the attachment gallery and file controls; deduplicate delivered files by ID within each turn.
- Retain Harness status, model catalogs and effort choices in shared client state. Save model choices in order. Wait for pending saves before dispatch. Read chat history in pages of at most 200 messages. Stream changed messages and runtime fields. Read full tool output and image content on demand through the same authenticated Bot boundary.
- Use React View Transitions for panel opening and closing. During pointer resize, apply the latest width once per frame and let transcript text reflow. Animate compact sidebar changes with shared CSS width transitions and motion tokens. Keep snapshot transitions out of dragging and snapping. Start an interrupted drag from the displayed width. Keep transcript position and drafts stable. Honor reduced motion. Closing a viewer still releases its control lease.
- Use short sentences and active voice. Report confirmed results separately from assumptions and unverified behavior.
- Check installed versions and current official documentation before changing external API or protocol behavior. Reference projects inform design; their rules do not govern this repository.

## Verification

- Use the scripts in `package.json` as the command source. Run focused tests for the changed behavior first, then the checks required by affected consumers and CI.
- Run `pnpm typecheck` and `pnpm lint` for code changes. Fix new errors and report retained warnings.
- For Go changes, run `gofmt -l .`, `go vet ./...`, `go test ./...`, and `golangci-lint run` with the root `.golangci.yml`. The Checks `go` job runs build, vet, and tests on Linux, macOS, and Windows, and lint on Linux. `pnpm contract:check` runs in the `check` job.
- For Harness or Computer Use changes, read [docs/computer-evals.md](docs/computer-evals.md) before changing behavior. Save the relevant failure or baseline first. Run the regression after the change.
- Add new confirmed failure cases to the small, sanitized eval set. Report completion evidence, coverage, elapsed time, and model round trips when available. Without eval evidence, do not claim a Harness performance or success-rate improvement.
- Offline trajectory scoring evaluates recorded behavior. Scorer tests test the scorer. Neither reruns an Agent. Rule pass rates are separate from task success rates.
- Verify visible UI changes in the running UI. Use real input and result readback for native input claims; simulated protocol tests are not native desktop acceptance.
- Wait for a state or event in tests. Use timeouts as hang guards. Keep a performance threshold tied to the work it measures and report runner limits.
- For cross-platform claims, inspect Windows, Linux, and macOS results for the same commit in `.github/workflows/checks.yml` and `.github/workflows/computer-input.yml`.
- Checks runs lint and typecheck once before platform jobs. It calls Computer input for input tests. Keep each test file in one job per OS. Run Harness process tests and real browser service tests in separate sequential steps. Use the runner default shell for native process tests. Web E2E splits files across two CI shards per OS. Each runner uses one worker because real browser and desktop tests share its GUI session. Main pushes and pull requests start checks. New runs cancel older runs for the same workflow and ref.
- Report skipped and untested capabilities explicitly. Green CI does not prove every OS permission, desktop session, or third-party application works.
- Documentation-only edits need link, path, and diff checks. Do not add tests that assert document wording.
- Push and PR workflows skip changes limited to root `AGENTS.md`, root `README*.md`, and Markdown under `docs/`. Other changes run the full workflows. Before making these workflows required checks, review path filtering so a skipped workflow cannot leave a required check pending.

## Environment secrets

- Do not read, search, display, or log secret values from environment files or process environments.
- Application code may load and use secrets at runtime.
- Check secret files only for existence and Git ignore status.
- Preserve existing secret files. Do not overwrite them.
- Keep `.env.example` free of secret values.

## Data and local processes

- Preserve user data and existing public contracts. Keep authentication, authorization, native IPC validation, and WebView origin permissions at their boundaries.
- Restart only identified Persona processes. Check PID, executable path, and ownership before stopping them. Do not kill an entire tree that may contain user applications.
- Use a small test window for desktop checks when possible. Use the user's requested application only within the authorized task.

## Git workflow

- Create feature branches from the latest `main`. Develop and commit in the existing repository directory, one feature at a time.
- Commit or stash local changes before switching branches. Create a Git worktree only when the user explicitly requests one.
- Keep `main` clean between integrations. Review and test each feature before merging it into `main`.
- Push when authorized by the user. Verify the merged commit and all required CI results.

## Keep this guide current

- Update this file in the same change when module ownership, public contracts, development workflows, or verification boundaries change. Replace or remove stale guidance.
- Store current decisions and rules here. Keep task progress, incident timelines, commit IDs, CI run links, and temporary plans in task reports or Git.
- Link to existing specialist documentation when a task needs detail. Do not duplicate command catalogs or file inventories that the code already provides.
- Keep `AGENTS.md` as the shared project source. Check each AI client's repository-instruction entry point before claiming it loads this file. Any required compatibility entry should point here instead of copying the rules.
