// Runs the Go service (`persona serve`) for the web E2E suite with the same
// argv and IPC replies as web-server.ts. A local Ollama double answers the
// model calls the way web-server.ts's faux provider does. Commands that need
// the TypeScript runtime (tool calls, seeded records, approvals, the work
// browser) answer { type: "unsupported" }.
import {
  type ChildProcessWithoutNullStreams,
  execFileSync,
  spawn,
} from "node:child_process";
import { once } from "node:events";
import { mkdir, writeFile } from "node:fs/promises";
import { createServer, type ServerResponse } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { createInterface } from "node:readline";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";

const database = process.argv[2];
if (!database) throw new Error("Database required");
const root = fileURLToPath(new URL("../../../..", import.meta.url));
const directory = dirname(database);
const models = [
  { id: "faux-1", name: "测试模型一" },
  { id: "faux-2", name: "测试模型二" },
];
type Reply = { text: string; stopReason?: string; errorMessage?: string };
let scripted: Reply[] = [];
let gate: PromiseWithResolvers<void> | undefined;
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
  const waiting = gate;
  gate = undefined;
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
  // 40 one-character tokens per second, as web-server.ts streams.
  for (const character of reply.text) {
    if (aborted.signal.aborted) return;
    chunk(response, model, { content: character });
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  response.write(
    `data: ${JSON.stringify({
      id: "chatcmpl-fixture",
      object: "chat.completion.chunk",
      created: Math.floor(Date.now() / 1000),
      model,
      choices: [{ index: 0, delta: {}, finish_reason: "stop" }],
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
  JSON.stringify({ defaultProvider: "ollama", defaultModel: "faux-1" }),
);
const bin = join(root, "packages", "agent", "node_modules", ".bin");
// A killed fixture leaves its serve process to stop after stdin closes, so a
// restart on the same data directory waits for that process to release it.
async function startService(): Promise<{
  child: ChildProcessWithoutNullStreams;
  port: number;
}> {
  for (let attempt = 0; ; attempt++) {
    const child = spawn(
      command,
      [
        "serve",
        "-host",
        "127.0.0.1",
        "-port",
        process.argv[3] ?? "0",
        "-data",
        join(directory, "go-data"),
        "-static",
        process.env.PERSONA_TEST_STATIC_DIR ??
          join(root, "apps", "web", "dist"),
        "-contract",
        join(root, "contracts", "api"),
        "-pi-extension",
        join(root, "packages", "agent", "src", "harness", "pi-tools.mjs"),
      ],
      {
        // Outside the repository so persona.config.json does not select hosted mode.
        cwd: directory,
        env: {
          ...process.env,
          OLLAMA_HOST: `http://127.0.0.1:${ollamaPort}`,
          PI_CODING_AGENT_DIR: agentDir,
          PATH: `${bin}${process.platform === "win32" ? ";" : ":"}${process.env.PATH}`,
        },
        stdio: ["pipe", "pipe", "pipe"],
      },
    );
    let errors = "";
    child.stderr.on("data", (chunk: Buffer) => {
      errors += chunk;
      process.stderr.write(chunk);
    });
    const exited = once(child, "exit").then(() => null);
    const listening = (async () => {
      for await (const line of createInterface({ input: child.stdout })) {
        const address = /listening on http:\/\/[^:]+:(\d+)/.exec(line);
        if (address) return Number(address[1]);
      }
      return null;
    })();
    const port = await Promise.race([listening, exited]);
    if (port !== null) return { child, port };
    await exited;
    if (!errors.includes("data directory is in use") || attempt >= 100)
      process.exit(1);
    await delay(100);
  }
}

const { child: service, port } = await startService();
service.on("exit", () => process.exit(1));
await configure(`http://127.0.0.1:${port}`);
process.send?.({ type: "ready", port, toolName: "" });
service.stdout.resume();

// web-server.ts opens every Bot with the faux model and prepares its engine;
// here each Bot is configured through engines.configure instead.
async function configure(origin: string) {
  const call = async (path: string, json: unknown) => {
    const response = await fetch(`${origin}/rpc/${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: origin },
      body: JSON.stringify({ json }),
    });
    if (!response.ok)
      throw new Error(`${path}: ${response.status} ${await response.text()}`);
    return ((await response.json()) as { json: unknown }).json;
  };
  const bots = (await call("bots/list", {})) as { id: number }[];
  for (const { id } of bots) {
    const profile = (await call("engines/get", { botId: id })) as {
      revision: number;
    };
    await call("engines/configure", {
      botId: id,
      config: { provider: "pi", model: "ollama/faux-1" },
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
  } else if (
    typeof message === "object" &&
    message !== null &&
    "content" in message &&
    typeof message.content === "string" &&
    !("stopReason" in message && message.stopReason === "toolUse")
  ) {
    scripted = [{ text: message.content }];
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
