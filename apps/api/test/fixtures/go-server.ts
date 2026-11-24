// Runs the Go service (`persona serve`) for the web E2E suite with the same
// argv and IPC replies as web-server.ts. An OpenAI-compatible double, declared
// to Pi as the "faux" provider in models.json, answers the model calls the way
// web-server.ts's faux provider does. Commands that need
// the TypeScript runtime (tool calls, seeded records, approvals, the work
// browser) answer { type: "unsupported" }.
import {
  type ChildProcess,
  execFileSync,
  type SpawnOptions,
  spawn,
} from "node:child_process";
import { createHash } from "node:crypto";
import { once } from "node:events";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import {
  createServer,
  request as httpRequest,
  type ServerResponse,
} from "node:http";
import { createServer as createNetServer } from "node:net";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";
import { DatabaseSync } from "node:sqlite";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { takeoverFixture } from "./takeover-page.ts";

const database = process.argv[2];
if (!database) throw new Error("Database required");
const root = fileURLToPath(new URL("../../../..", import.meta.url));
const directory = dirname(database);
const noMessage = "persona-fixture: failure without a message";
const models = [
  { id: "faux-1", name: "测试模型一" },
  { id: "faux-2", name: "测试模型二", reasoning: true },
];
type ToolCall = { id: string; name: string; arguments: unknown };
type Reply = {
  text: string;
  thinking?: string;
  toolCalls?: ToolCall[];
  stopReason?: string;
  errorMessage?: string;
};
let scripted: Reply[] = [];
// "takeover" replaces the scripted queue with the browser-takeover turn's
// replies, as web-server.ts's takeover mode does: open the fixture page, ask
// for takeover, observe, then answer with the observed controls.
const takeover = process.argv[4] === "takeover";
let takeoverRequests = 0;
let gate: PromiseWithResolvers<void> | undefined;
let toolGate: PromiseWithResolvers<void> | undefined;
// True while a fixture tool call that already waited at /tool still owes its
// tool result to a model request, which then must not take the next gate.
let toolReported = false;
let modelStarted = Promise.withResolvers<void>();

function lastUserText(body: {
  messages?: { role: string; content: unknown }[];
}) {
  const user = body.messages?.findLast((message) => message.role === "user");
  if (typeof user?.content === "string") return user.content;
  if (!Array.isArray(user?.content)) return "";
  return user.content
    .filter((part) => part?.type === "text")
    .map((part) => part.text)
    .join("\n");
}

// Scripted content the double can stream: a string, or text and tool call parts.
function replyOf(item: {
  content?: unknown;
  stopReason?: string;
  errorMessage?: string;
}): Reply | undefined {
  const { content, stopReason, errorMessage } = item;
  if (typeof content === "string")
    return { text: content, stopReason, errorMessage };
  if (!Array.isArray(content)) return undefined;
  const reply: Reply = { text: "", toolCalls: [], stopReason, errorMessage };
  for (const part of content) {
    if (part?.type === "text" && typeof part.text === "string")
      reply.text += part.text;
    else if (part?.type === "thinking" && typeof part.thinking === "string")
      reply.thinking = (reply.thinking ?? "") + part.thinking;
    else if (part?.type === "toolCall" && typeof part.name === "string")
      reply.toolCalls?.push({
        id: part.id ?? crypto.randomUUID(),
        name: part.name,
        arguments: part.arguments ?? {},
      });
    else return undefined;
  }
  return reply;
}

// Writes chat records into serve's store as one commit, as the TypeScript
// fixture appends them through the harness.
function seed(botId: number, records: unknown[]) {
  const db = new DatabaseSync(join(directory, "go-data", "persona-go.sqlite"));
  try {
    db.exec("PRAGMA busy_timeout=5000; BEGIN IMMEDIATE");
    const { seq, last_id } = db
      .prepare("UPDATE store_state SET seq=seq+1 RETURNING seq, last_id")
      .get() as { seq: number; last_id: number };
    let id = last_id;
    const claim = db.prepare("INSERT INTO record_ids VALUES (?, 'entry')");
    const entry = db.prepare("INSERT INTO entries VALUES (?, ?, ?, NULL, ?)");
    for (const data of records) {
      id++;
      claim.run(id);
      entry.run(
        id,
        botId,
        seq,
        JSON.stringify({
          id,
          conversationId: botId,
          kind: "persona.engine-message",
          data,
        }),
      );
    }
    db.prepare("UPDATE store_state SET last_id=?").run(id);
    db.exec("COMMIT");
  } catch (error) {
    if (db.isTransaction) db.exec("ROLLBACK");
    throw error;
  } finally {
    db.close();
  }
}

function chunk(response: ServerResponse, model: string, delta: object) {
  response.write(
    `data: ${JSON.stringify({
      id: "chatcmpl-fixture",
      object: "chat.completion.chunk",
      created: Math.floor(Date.now() / 1000),
      model,
      choices: [{ index: 0, delta, finish_reason: null }],
    })}\n\n`,
  );
}

// The takeover-mode reply for each model request, in request order: the
// chat turn (open, takeover, observe, answer with the observed controls),
// then the open-takeover-page turn after the browser mode switch (open,
// done). In web-server.ts the re-open goes through services.call directly;
// the Go fixture drives it as a send turn, which consumes these steps.
async function takeoverNext(
  body: Record<string, unknown>,
  pageUrl: string,
): Promise<Reply> {
  const step = takeoverRequests++;
  const toolCall = (name: string, args: Record<string, unknown>): Reply => ({
    text: "",
    toolCalls: [{ id: `takeover:${step}`, name, arguments: args }],
    stopReason: "toolUse",
  });
  switch (step) {
    case 0:
      return toolCall("browser_agent_browser_open", { url: pageUrl });
    case 1:
      return toolCall("computer_request_takeover", {
        service: "browser",
        reason: "请修改搜索词，完成后交还给我。",
      });
    case 2:
      return toolCall("browser_agent_browser_observe", {});
    default: {
      // The last tool message holds the observer's page facts, as
      // web-server.ts's final scripted reply reads them.
      const messages = (body.messages ?? []) as {
        role: string;
        content: unknown;
      }[];
      const facts = messages.findLast(
        (message) => message.role === "tool",
      )?.content;
      const text = (Array.isArray(facts) ? facts : [{ text: facts }])
        .map((part) => (part as { text?: string }).text ?? "")
        .join("\n");
      const marker = "Page facts (untrusted website data): ";
      const start = text.indexOf(marker);
      if (start >= 0) {
        const page = JSON.parse(text.slice(start + marker.length)) as {
          controls: unknown;
        };
        return {
          text: `接管已完成，重新观察：${JSON.stringify(page.controls)}`,
        };
      }
      // The Go worker drives agent-browser directly: its raw observe call
      // returns no Persona "Page facts". The fixture page's own state holds
      // the same fields a real composite observe would surface.
      const state = (await fetch(
        `http://127.0.0.1:${fixturePort}/takeover-state`,
      ).then((response) => response.json())) as { value?: string };
      return {
        text: `接管已完成，重新观察：${JSON.stringify({ value: state.value ?? "" })}`,
      };
    }
  }
}

async function complete(
  body: Record<string, unknown>,
  response: ServerResponse,
) {
  modelStarted.resolve();
  const model = String(body.model ?? "");
  const aborted = new AbortController();
  response.on("close", () => aborted.abort());
  // Pi runs tools itself, so a gated tool holds the request that reports its
  // result — except the fixture tool, which already waits at /tool. A gate
  // armed for the next tool call must not be stolen by the request reporting
  // a tool that was already held.
  const afterTool =
    (body.messages as { role: string }[] | undefined)?.at(-1)?.role === "tool";
  const next = takeover
    ? await takeoverNext(
        body,
        `http://127.0.0.1:${fixturePort}/takeover-fixture`,
      )
    : scripted.shift();
  let waiting: PromiseWithResolvers<void> | undefined;
  if (afterTool && toolReported) {
    toolReported = false;
    waiting = next ? undefined : gate;
  } else if (afterTool && toolGate) {
    waiting = toolGate;
    toolGate = undefined;
  } else {
    waiting = next ? undefined : gate;
  }
  if (waiting && waiting === gate) gate = undefined;
  if (waiting) {
    aborted.signal.addEventListener("abort", () => waiting.resolve(), {
      once: true,
    });
    await waiting.promise;
  }
  const reply = next ?? {
    text: `已收到：${lastUserText(body as never)}。这是一条用于验证流式显示和会话恢复的完整回复。（${model}）`,
  };
  if (reply.stopReason === "error") {
    // Pi retries a 500; a faux error without a message is not retried.
    response.writeHead(reply.errorMessage ? 500 : 400, {
      "Content-Type": "application/json",
    });
    response.end(
      JSON.stringify({ error: { message: reply.errorMessage ?? noMessage } }),
    );
    return;
  }
  response.writeHead(200, { "Content-Type": "text/event-stream" });
  chunk(response, model, { role: "assistant", content: "" });
  // One four-character token every 25 ms, as web-server.ts's faux provider streams.
  for (const [field, text] of [
    ["reasoning_content", reply.thinking ?? ""],
    ["content", reply.text],
  ] as const) {
    const characters = [...text];
    for (let index = 0; index < characters.length; index += 4) {
      await new Promise((resolve) => setTimeout(resolve, 25));
      if (aborted.signal.aborted) return;
      chunk(response, model, {
        [field]: characters.slice(index, index + 4).join(""),
      });
    }
  }
  // Specs script bare tool names; map them to the name the engine declared —
  // persona tools arrive as mcp__persona__<name> and Pi's native tools as-is.
  const declared = new Map<string, string>();
  for (const tool of (body.tools ?? []) as {
    function?: { name?: string };
  }[]) {
    const name = tool.function?.name;
    if (!name) continue;
    declared.set(name, name);
    const bare = name.replace(/^mcp__persona__/, "");
    if (!declared.has(bare)) declared.set(bare, name);
  }
  const calls = reply.toolCalls ?? [];
  for (const [index, call] of calls.entries())
    chunk(response, model, {
      tool_calls: [
        {
          index,
          id: call.id,
          type: "function",
          function: {
            name: declared.get(call.name) ?? call.name,
            arguments: JSON.stringify(call.arguments ?? {}),
          },
        },
      ],
    });
  response.write(
    `data: ${JSON.stringify({
      id: "chatcmpl-fixture",
      object: "chat.completion.chunk",
      created: Math.floor(Date.now() / 1000),
      model,
      choices: [
        {
          index: 0,
          delta: {},
          finish_reason: calls.length ? "tool_calls" : "stop",
        },
      ],
      usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
    })}\n\ndata: [DONE]\n\n`,
  );
  response.end();
}

// The fixture tool's Pi extension asks here before it returns, so a gated
// tool stays running the way web-server.ts's test tool does.
async function runTool(response: ServerResponse) {
  // Each "gate-tool" arms one hold for the next tool call. Mark the call's
  // result outstanding so the request that reports it skips the tool gate.
  const waiting = toolGate;
  toolGate = undefined;
  toolReported = true;
  if (waiting) {
    response.on("close", () => waiting.resolve());
    await waiting.promise;
  }
  response.end();
}

const ollama = createServer((request, response) => {
  let raw = "";
  request.on("data", (part) => {
    raw += part;
  });
  request.on("end", () => {
    const json = (value: unknown) => {
      response.setHeader("Content-Type", "application/json");
      response.end(JSON.stringify(value));
    };
    if (request.url === "/api/version") return json({ version: "0.0.0" });
    if (request.url === "/api/tags")
      return json({
        models: models.map(({ id }) => ({ name: id, model: id })),
      });
    if (request.url === "/api/show")
      return json({ model_info: { "fixture.context_length": 32768 } });
    if (request.url === "/v1/models")
      return json({
        object: "list",
        data: models.map(({ id }) => ({ id, object: "model" })),
      });
    if (request.url === "/v1/chat/completions" && request.method === "POST")
      return void complete(JSON.parse(raw || "{}"), response);
    if (request.url === "/tool" && request.method === "POST")
      return void runTool(response);
    response.statusCode = 404;
    response.end();
  });
});
await new Promise<void>((resolve) => ollama.listen(0, "127.0.0.1", resolve));
const ollamaAddress = ollama.address();
if (!ollamaAddress || typeof ollamaAddress === "string")
  throw new Error("Port required");
const ollamaPort = ollamaAddress.port;

let binary = process.env.PERSONA_GO_BIN;
if (!binary) {
  binary = join(
    tmpdir(),
    "persona-web-go",
    process.platform === "win32" ? "persona.exe" : "persona",
  );
  execFileSync("go", ["build", "-o", binary, "./cmd/persona"], {
    cwd: root,
    stdio: "inherit",
  });
}
const command = binary;
const agentDir = join(directory, "pi-agent");
await mkdir(agentDir, { recursive: true });
await writeFile(
  join(agentDir, "settings.json"),
  JSON.stringify({
    defaultProvider: "faux",
    defaultModel: "faux-1",
  }),
);
await writeFile(
  join(agentDir, "models.json"),
  JSON.stringify({
    providers: {
      faux: {
        baseUrl: `http://127.0.0.1:${ollamaPort}/v1`,
        apiKey: "fixture",
        api: "openai-completions",
        models: models.map(({ id, name, reasoning }) => ({
          id,
          name,
          reasoning: reasoning ?? false,
          input: ["text", "image"],
          contextWindow: 32768,
          maxTokens: 8192,
          cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        })),
      },
    },
  }),
);
// Pi loads extensions from its agent directory. This one stands in for
// web-server.ts's test tool: it returns the supplied text after the fixture
// releases a gated tool.
const toolName = "fixture_echo";
await mkdir(join(agentDir, "extensions"), { recursive: true });
await writeFile(
  join(agentDir, "extensions", "fixture-tool.js"),
  `export default function (pi) {
  pi.registerTool({
    name: ${JSON.stringify(toolName)},
    label: ${JSON.stringify(toolName)},
    description: "Return the supplied text.",
    parameters: {
      type: "object",
      properties: { text: { type: "string" }, fail: { type: "boolean" } },
      required: ["text"],
    },
    async execute(_id, params, signal) {
      await fetch(${JSON.stringify(`http://127.0.0.1:${ollamaPort}/tool`)}, { method: "POST", signal });
      if (params.fail) throw new Error(params.text);
      return { content: [{ type: "text", text: params.text }], details: {} };
    },
  });
}
`,
);
const bin = join(root, "packages", "agent", "node_modules", ".bin");
const dataDir = join(directory, "go-data");
// The browser driver proxies agent-browser so calls are timed for
// /takeover-metrics, as web-server.ts wraps computer.tools.invoke.
const metricsFile = join(directory, "takeover-metrics.json");
// web-server.ts's "hosting" mode: a hosting config puts serve behind device
// pairing, as openHostedRuntime does. A hosting config needs a fixed port.
const hosting = process.argv[4] === "hosting";
const config = join(directory, "go-hosting.json");
if (hosting)
  await writeFile(
    config,
    JSON.stringify({
      host: "127.0.0.1",
      port: await freePort(),
      dataDir,
    }),
  );
const args = [
  "serve",
  "-host",
  "127.0.0.1",
  // Serve binds an ephemeral port; the requested port belongs to the proxy so
  // a restart reusing it never collides with a serve on the same number.
  "-port",
  "0",
  "-data",
  dataDir,
  "-static",
  process.env.PERSONA_TEST_STATIC_DIR ?? join(root, "apps", "web", "dist"),
  "-contract",
  join(root, "contracts", "api"),
  "-pi-extension",
  join(root, "packages", "agent", "src", "harness", "pi-tools.mjs"),
  // serve always runs Pi with --no-extensions, so settings.json extensions
  // never load; the faux failure stream and the fixture tool must be explicit
  // extensions.
  "-pi-extension",
  fileURLToPath(new URL("./pi-faux-failure.mjs", import.meta.url)),
  "-pi-extension",
  join(agentDir, "extensions", "fixture-tool.js"),
];
const options = {
  // Outside the repository so persona.config.json does not select hosted mode.
  cwd: directory,
  env: {
    ...process.env,
    PI_CODING_AGENT_DIR: agentDir,
    ...(hosting ? { PERSONA_CONFIG: config } : {}),
    PERSONA_FAUX_NO_MESSAGE: noMessage,
    // The Go service records each product computer call here, as the
    // TypeScript computer.tools.invoke tap did in the fixture driver.
    PERSONA_BROWSER_METRICS_FILE: metricsFile,
    PATH: `${bin}${process.platform === "win32" ? ";" : ":"}${process.env.PATH}`,
  },
  stdio: ["pipe", "pipe", "pipe"],
} satisfies SpawnOptions;
// Tests stop by killing this fixture, as they kill web-server.ts, and then
// remove its directory. Serve drains for seconds after stdin closes, so on POSIX
// a shell leads serve's process group and kills it as soon as this fixture's pipe closes.
const watchdog =
  'exec 3<&0; "$@" </dev/null 3<&- & pid=$!; { cat <&3 >/dev/null; kill -9 0; } & wait $pid';
const launch = () =>
  process.platform === "win32"
    ? spawn(command, args, options)
    : spawn("sh", ["-c", watchdog, "sh", command, ...args], {
        ...options,
        detached: true,
      });

// A killed fixture's serve can still hold the data directory, so a restart on
// the same directory waits for that process to release it.
async function startService(): Promise<{ child: ChildProcess; port: number }> {
  for (let attempt = 0; ; attempt++) {
    const child = launch();
    let errors = "";
    child.stderr?.on("data", (chunk: Buffer) => {
      errors += chunk;
      process.stderr.write(chunk);
    });
    const exited = once(child, "exit").then(() => null);
    const port = await Promise.race([listening(child), exited]);
    if (port !== null) return { child, port };
    await exited;
    if (!errors.includes("data directory is in use") || attempt >= 100)
      process.exit(1);
    await delay(100);
  }
}

async function listening(child: ChildProcess) {
  if (!child.stdout) throw new Error("Service output required");
  for await (const line of createInterface({ input: child.stdout })) {
    const address = /listening on http:\/\/[^:]+:(\d+)/.exec(line);
    if (address) {
      child.stdout.resume();
      return Number(address[1]);
    }
  }
  return null;
}

const { child: service, port } = await startService();
service.on("exit", () => process.exit(1));
const origin = `http://127.0.0.1:${port}`;
// The takeover specs need /takeover-* endpoints on the service origin, so the
// fixture listens on the requested port and proxies everything else to serve.
const takeoverPage = takeoverFixture();
const proxy = createServer((request, response) => {
  if (request.url === "/takeover-metrics") {
    response.setHeader("Content-Type", "application/json");
    readFile(metricsFile, "utf8").then(
      (metrics) => response.end(metrics),
      () => response.end("[]"),
    );
    return;
  }
  if (takeoverPage(request, response)) return;
  // serve's origin guard compares Host and Origin with its own address.
  const headers = { ...request.headers, host: `127.0.0.1:${port}`, origin };
  const upstream = httpRequest(
    {
      host: "127.0.0.1",
      port,
      method: request.method,
      path: request.url,
      headers,
    },
    (back) => {
      response.writeHead(back.statusCode ?? 502, back.headers);
      back.pipe(response);
    },
  );
  upstream.on("error", () => {
    if (!response.headersSent) response.writeHead(502);
    response.end();
  });
  request.pipe(upstream);
});
const fixturePort = Number(process.argv[3] ?? 0) || (await freePort());
await new Promise<void>((resolve) =>
  proxy.listen(fixturePort, "127.0.0.1", resolve),
);
// The fixture pairs as an admin device with the owner code serve wrote, and
// gives the test a fresh admin code, as web-server.ts gives sessions.pairing("admin").
let cookie: string | undefined;
if (hosting) {
  const code = await readFile(join(dataDir, "owner-code"), "utf8");
  const exchanged = await hosted("exchange", { code, label: "fixture" });
  cookie = exchanged.headers.getSetCookie()[0]?.split(";")[0];
}
await configure();
const pairing = hosting
  ? (
      (await (await hosted("pairing", { scope: "admin" })).json()) as {
        code: string;
      }
    ).code
  : undefined;
process.send?.({ type: "ready", port: fixturePort, toolName, pairing });

async function hosted(path: string, json: unknown) {
  const response = await fetch(`${origin}/hosting/${path}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Origin: origin,
      ...(cookie ? { Cookie: cookie } : {}),
    },
    body: JSON.stringify(json),
  });
  if (!response.ok)
    throw new Error(`${path}: ${response.status} ${await response.text()}`);
  return response;
}

async function freePort() {
  const server = createNetServer();
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  await new Promise((resolve) => server.close(resolve));
  if (!address || typeof address === "string") throw new Error("Port required");
  return address.port;
}

// web-server.ts opens every Bot with the faux model and prepares its engine;
// here each Bot is configured through engines.configure instead.
async function call(path: string, json: unknown) {
  const response = await fetch(`${origin}/rpc/${path}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Origin: origin,
      ...(cookie ? { Cookie: cookie } : {}),
    },
    body: JSON.stringify({ json }),
  });
  if (!response.ok)
    throw new Error(`${path}: ${response.status} ${await response.text()}`);
  return ((await response.json()) as { json: unknown }).json;
}

async function firstBot() {
  const [bot] = (await call("bots/list", {})) as { id: number }[];
  if (!bot) throw new Error("Bot required");
  return bot.id;
}

async function configure() {
  const bots = (await call("bots/list", {})) as { id: number }[];
  for (const { id } of bots) {
    const profile = (await call("engines/get", { botId: id })) as {
      revision: number;
      config: { model: string | null };
    };
    // A restart keeps the model the test chose.
    if (!profile.config.model?.startsWith("faux/"))
      await call("engines/configure", {
        botId: id,
        config: { provider: "pi", model: "faux/faux-1" },
        revision: profile.revision,
        requestId: crypto.randomUUID(),
      });
    // engines.prepare's inspection, so engines.get lists the catalog's names.
    await call("engines/inspect", {
      botId: id,
      config: { provider: "pi", model: profile.config.model ?? "faux/faux-1" },
    });
  }
}

let release: (() => void) | undefined;
process.on("message", async (message) => {
  if (message === "gate") {
    // wait-model then waits for the request this gate holds.
    modelStarted = Promise.withResolvers<void>();
    gate = Promise.withResolvers<void>();
    release = gate.resolve;
    process.send?.({ type: "gated" });
  } else if (message === "wait-model") {
    await modelStarted.promise;
    process.send?.({ type: "generating" });
  } else if (message === "release") {
    release?.();
    process.send?.({ type: "released" });
  } else if (message === "open-takeover-page") {
    // The TypeScript fixture calls the browser service directly; here the page
    // opens through a scripted agent turn, as a real model request would. In
    // takeover mode the request stepper already answers these turns.
    if (!takeover)
      scripted = [
        {
          text: "",
          toolCalls: [
            {
              id: "open-takeover",
              name: "browser_agent_browser_open",
              arguments: {
                url: `http://127.0.0.1:${fixturePort}/takeover-fixture`,
              },
            },
          ],
          stopReason: "toolUse",
        },
        { text: "已打开" },
      ];
    // The TypeScript fixture opens the page through services.call, which
    // never asks; on Go the scripted turn calls through the shared approver,
    // so the binding needs the same "auto" mode the takeover card spec sets.
    const bot = await firstBot();
    const binding = (await call("computer/get", { botId: bot })) as {
      computerId?: string;
      revision?: number;
    };
    await call("computer/approval", {
      botId: bot,
      computerId: binding.computerId,
      revision: binding.revision,
      mode: "auto",
    });
    // The TypeScript fixture opens the page through services.call, which is
    // not a tool invocation (and is not counted in the takeover metrics).
    // The driver CLI reaches the same daemon session and navigates the
    // streamed page directly, so viewer input lands on this page too.
    const dir = join(
      dataDir,
      "persona-go.sqlite.files",
      String(bot),
      "workspace",
    );
    const session = createHash("sha256").update(dir).digest("hex").slice(0, 24);
    const state = join(dir, ".persona");
    const headed = await readFile(join(state, "browser-mode.json"), "utf8")
      .then((mode) =>
        (JSON.parse(mode) as { mode?: string }).mode === "headed" ? "1" : "0",
      )
      .catch(() => "0");
    const open = async () => {
      const code = await new Promise<number>((resolve) => {
        const child = spawn(
          "agent-browser",
          [
            "open",
            `http://127.0.0.1:${fixturePort}/takeover-fixture`,
            "--json",
          ],
          {
            env: {
              ...process.env,
              PATH: `${bin}${process.platform === "win32" ? ";" : ":"}${process.env.PATH}`,
              AGENT_BROWSER_SESSION: session,
              AGENT_BROWSER_SOCKET_DIR: join(
                tmpdir(),
                `persona-browser-${session}`,
              ),
              AGENT_BROWSER_CONFIG: join(state, "browser.json"),
              AGENT_BROWSER_HEADED: headed,
            },
            stdio: ["ignore", "ignore", "pipe"],
          },
        );
        let error = "";
        child.stderr?.on("data", (c) => (error += c));
        child.on("exit", (exit) => {
          if (exit !== 0)
            console.error(`[takeover] browser open: ${error.slice(0, 200)}`);
          resolve(exit ?? 1);
        });
        child.on("error", (spawnError) => {
          console.error(`[takeover] browser open: ${spawnError.message}`);
          resolve(1);
        });
      });
      if (code !== 0) return false;
      for (let attempt = 0; attempt < 300; attempt++) {
        await delay(100);
        const current = (await fetch(
          `http://127.0.0.1:${fixturePort}/takeover-state`,
        )
          .then((response) => response.json())
          .catch(() => ({}))) as { width?: number };
        if (typeof current.width === "number") return true;
      }
      return false;
    };
    let opened = await open();
    // A cold headless launch can abort the first navigation, so retry, as
    // web-server.ts's open-takeover-page does.
    for (let attempt = 1; !opened && attempt < 3; attempt++)
      opened = await open();
    if (!opened) throw new Error("takeover page did not report");
    process.send?.({ type: "takeover-page-opened" });
  } else if (message === "gate-tool") {
    toolGate = Promise.withResolvers<void>();
    release = toolGate.resolve;
    process.send?.({ type: "gated" });
  } else if (
    typeof message === "object" &&
    message !== null &&
    "history" in message &&
    typeof message.history === "number"
  ) {
    seed(
      await firstBot(),
      Array.from({ length: message.history }, (_, index) => {
        const turnId = `history:${index}`;
        return (["user", "assistant"] as const).map((role) => ({
          id: `${turnId}:${role}`,
          turnId,
          role,
          stopped: false,
          content: [
            {
              type: "text",
              key: `${turnId}:${role}:text`,
              text:
                role === "user"
                  ? `历史任务 ${index + 1}`
                  : `## 报告 ${index + 1}\n\n**资料已核对。**\n\n- 第一项结果\n- 第二项结果`,
            },
          ],
        }));
      }).flat(),
    );
    process.send?.({ type: "seeded" });
  } else if (
    typeof message === "object" &&
    message !== null &&
    "records" in message &&
    Array.isArray(message.records)
  ) {
    seed(await firstBot(), message.records);
    process.send?.({ type: "seeded" });
  } else if (
    typeof message === "object" &&
    message !== null &&
    "responses" in message &&
    Array.isArray(message.responses) &&
    message.responses.every((item) => replyOf(item))
  ) {
    scripted = message.responses.map((item) => replyOf(item) as Reply);
    process.send?.({ type: "scripted" });
  } else if (
    typeof message === "object" &&
    message !== null &&
    "content" in message &&
    replyOf(message) !== undefined
  ) {
    scripted = [replyOf(message) as Reply];
    process.send?.({ type: "scripted" });
  } else {
    process.stderr.write(
      `go-server: unsupported command ${JSON.stringify(message)}\n`,
    );
    process.send?.({ type: "unsupported" });
  }
});
process.on("disconnect", () => {
  service.removeAllListeners("exit");
  // Closing stdin stops serve on every platform.
  service.stdin?.end();
  ollama.closeAllConnections();
  ollama.close();
  const timer = setTimeout(() => service.kill("SIGKILL"), 10_000);
  service.once("exit", () => {
    clearTimeout(timer);
    process.exit();
  });
});
