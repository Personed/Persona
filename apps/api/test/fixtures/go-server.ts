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
import { mkdir, writeFile } from "node:fs/promises";
import { createServer, type ServerResponse } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";
import { DatabaseSync } from "node:sqlite";
import { fileURLToPath } from "node:url";

const database = process.argv[2];
if (!database) throw new Error("Database required");
const root = fileURLToPath(new URL("../../../..", import.meta.url));
const directory = dirname(database);
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
let gate: PromiseWithResolvers<void> | undefined;
let toolGate: PromiseWithResolvers<void> | undefined;
const modelStarted = Promise.withResolvers<void>();

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

async function complete(
  body: Record<string, unknown>,
  response: ServerResponse,
) {
  modelStarted.resolve();
  const model = String(body.model ?? "");
  const aborted = new AbortController();
  response.on("close", () => aborted.abort());
  // Pi runs tools itself, so a gated tool holds the request that reports its result.
  const afterTool =
    (body.messages as { role: string }[] | undefined)?.at(-1)?.role === "tool";
  const waiting = afterTool && toolGate ? toolGate : gate;
  if (waiting === toolGate) toolGate = undefined;
  else gate = undefined;
  if (waiting) {
    aborted.signal.addEventListener("abort", () => waiting.resolve(), {
      once: true,
    });
    await waiting.promise;
  }
  const reply = scripted.shift() ?? {
    text: `已收到：${lastUserText(body as never)}。这是一条用于验证流式显示和会话恢复的完整回复。（${model}）`,
  };
  if (reply.stopReason === "error") {
    response.writeHead(500, { "Content-Type": "application/json" });
    response.end(JSON.stringify({ error: { message: reply.errorMessage } }));
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
  const calls = reply.toolCalls ?? [];
  for (const [index, call] of calls.entries())
    chunk(response, model, {
      tool_calls: [
        {
          index,
          id: call.id,
          type: "function",
          function: {
            name: call.name,
            arguments: JSON.stringify(call.arguments),
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
    response.statusCode = 404;
    response.end();
  });
});
await new Promise<void>((resolve) => ollama.listen(0, "127.0.0.1", resolve));
const ollamaAddress = ollama.address();
if (!ollamaAddress || typeof ollamaAddress === "string")
  throw new Error("Port required");

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
const agentDir = join(directory, "pi-agent");
await mkdir(agentDir, { recursive: true });
await writeFile(
  join(agentDir, "settings.json"),
  JSON.stringify({ defaultProvider: "faux", defaultModel: "faux-1" }),
);
await writeFile(
  join(agentDir, "models.json"),
  JSON.stringify({
    providers: {
      faux: {
        baseUrl: `http://127.0.0.1:${ollamaAddress.port}/v1`,
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
const bin = join(root, "packages", "agent", "node_modules", ".bin");
const args = [
  "serve",
  "-host",
  "127.0.0.1",
  "-port",
  process.argv[3] ?? "0",
  "-data",
  join(directory, "go-data"),
  "-static",
  process.env.PERSONA_TEST_STATIC_DIR ?? join(root, "apps", "web", "dist"),
  "-contract",
  join(root, "contracts", "api"),
  "-pi-extension",
  join(root, "packages", "agent", "src", "harness", "pi-tools.mjs"),
];
const options = {
  // Outside the repository so persona.config.json does not select hosted mode.
  cwd: directory,
  env: {
    ...process.env,
    PI_CODING_AGENT_DIR: agentDir,
    PATH: `${bin}${process.platform === "win32" ? ";" : ":"}${process.env.PATH}`,
  },
  stdio: ["pipe", "pipe", "inherit"],
} satisfies SpawnOptions;
// Tests stop by killing this fixture, as they kill web-server.ts, and then
// remove its directory. Serve drains for seconds after stdin closes, so on POSIX
// a shell leads serve's process group and kills it as soon as this fixture's pipe closes.
const watchdog =
  'exec 3<&0; "$@" </dev/null 3<&- & pid=$!; { cat <&3 >/dev/null; kill -9 0; } & wait $pid';
const launch = () =>
  process.platform === "win32"
    ? spawn(binary, args, options)
    : spawn("sh", ["-c", watchdog, "sh", binary, ...args], {
        ...options,
        detached: true,
      });
// A restart on the same port can start before the killed serve released it.
const deadline = Date.now() + 15_000;
let service = launch();
let port = await listening(service);
while (port === undefined && Date.now() < deadline) {
  await new Promise((resolve) => setTimeout(resolve, 100));
  service = launch();
  port = await listening(service);
}
if (port === undefined) process.exit(1);
service.on("exit", () => process.exit(1));
await configure();
// Pi's own ls stands in for web-server.ts's gated test tool.
process.send?.({ type: "ready", port, toolName: "ls" });

async function listening(child: ChildProcess) {
  if (!child.stdout) throw new Error("Service output required");
  for await (const line of createInterface({ input: child.stdout })) {
    const address = /listening on http:\/\/[^:]+:(\d+)/.exec(line);
    if (address) {
      child.stdout.resume();
      return Number(address[1]);
    }
  }
  return undefined;
}

// web-server.ts opens every Bot with the faux model and prepares its engine;
// here each Bot is configured through engines.configure instead.
async function call(path: string, json: unknown) {
  const origin = `http://127.0.0.1:${port}`;
  const response = await fetch(`${origin}/rpc/${path}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Origin: origin },
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
    if (profile.config.model?.startsWith("faux/")) continue;
    await call("engines/configure", {
      botId: id,
      config: { provider: "pi", model: "faux/faux-1" },
      revision: profile.revision,
      requestId: crypto.randomUUID(),
    });
  }
}

let release: (() => void) | undefined;
process.on("message", async (message) => {
  if (message === "gate") {
    gate = Promise.withResolvers<void>();
    release = gate.resolve;
    process.send?.({ type: "gated" });
  } else if (message === "wait-model") {
    await modelStarted.promise;
    process.send?.({ type: "generating" });
  } else if (message === "release") {
    release?.();
    process.send?.({ type: "released" });
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
