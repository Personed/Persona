# API contract

The frozen wire contract of the TypeScript API. The Go service (`persona serve`) must answer every request here exactly as the TypeScript service does, so the web UI and desktop shell keep working unchanged during the migration.

| File | Source | Update |
| --- | --- | --- |
| `procedures.json` | `createRouter` in `apps/api/src/router.ts` | `pnpm contract:export` |
| `fixtures/*.json` | `apps/api/test/contract.test.ts` against the real router | `PERSONA_RECORD_CONTRACT=1 pnpm vitest run apps/api/test/contract.test.ts` |
| `http.json` | `apps/api/src/app.ts`, `apps/api/src/hosting/routes.ts` | By hand |

CI fails when `procedures.json` or a fixture no longer matches the TypeScript API. `go test ./internal/contract` checks that the fixtures agree with `procedures.json`; Go packages load both with `internal/contract`.

## Wire

- Every procedure is `POST /rpc/<path>` with dots replaced by slashes, for example `bots.create` is `POST /rpc/bots/create`.
- The request body is `{"json": <input>}`; procedures without input send `{}`. The success body is `{"json": <output>}`. oRPC adds `"meta"` only for values JSON cannot carry (`Date`, `BigInt`, `Map`, `Set`, `undefined` in arrays); the current API sends none.
- Errors use the HTTP status of the code and the body `{"json": {"defined": false, "code", "status", "message", "data"?}}`. Input validation failures are `400 BAD_REQUEST` with `data.issues`.
- `stream` procedures answer with `text/event-stream`: `message` events carry `{"json": <value>}`, and the stream ends with `done` or `error`.
- `fixtures/native-conversation.json` uses the real native runtime and a Pi protocol double. For `changes`, `response.body` is the first snapshot frame as SSE text; the recorder cancels the stream after that frame. It does not record updates or the terminal event.

## Procedure fields

- `input` is the JSON Schema 2020-12 of the accepted input (`io: "input"`, so defaults are optional), or `null`. For parity with Zod, count string lengths in UTF-16 code units.
- `x-persona-trimmed` means apply ECMAScript `String.prototype.trim` to the supplied string, validate the result against the nested schema, and pass the trimmed value to the handler. For example, Bot names containing only spaces are invalid, but external spaces do not count toward the 80-unit limit.
- `x-persona-url-protocols` restricts a URL to the listed protocols (including the colon) after parsing with WHATWG URL semantics. Other refinements are expressed as JSON Schema constraints. Consumers must enforce these extensions; plain JSON Schema validators ignore them.
- `errors` lists every oRPC code the handler can return, found statically from the handler and the helpers it calls. It can include codes a given input never reaches. `INTERNAL_SERVER_ERROR` covers unexpected failures.
- Outputs have no declared schema in the router. The fixtures record real outputs; volatile values are replaced with `<uuid>`, `<time>` and `<path>`.
