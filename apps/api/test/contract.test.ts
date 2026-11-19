import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { BACKGROUND_CONTEXT } from "@earendil-works/chord/context";
import { createModels } from "@earendil-works/pi-ai/models";
import {
  fauxAssistantMessage,
  fauxProvider,
} from "@earendil-works/pi-ai/providers/faux";
import { createRegistry } from "@earendil-works/pi-durable";
import { createORPCClient } from "@orpc/client";
import { RPCLink } from "@orpc/client/fetch";
import type { RouterClient } from "@orpc/server";
import { openAgent } from "@persona/agent";
import type { Computers } from "@persona/agent/computer/catalog";
import { Files } from "@persona/agent/files";
import { expect, onTestFinished, test } from "vitest";
import { ProtocolProcess } from "../../../packages/agent/test/fixtures/engine-process.ts";
import { procedures } from "../contract/procedures.ts";
import { createApp } from "../src/app.ts";
import type { AppRouter } from "../src/router.ts";

// Records the oRPC wire of the TypeScript API into contracts/api/fixtures.
// Set PERSONA_RECORD_CONTRACT=1 to rewrite the fixtures after an intended change.
const fixtures = fileURLToPath(
  new URL("../../../contracts/api/fixtures/", import.meta.url),
);
const record = process.env.PERSONA_RECORD_CONTRACT === "1";
const context = BACKGROUND_CONTEXT;

type Exchange = {
  procedure: string;
  request: { method: string; path: string; body: unknown };
  response: { status: number; body: unknown; contentType?: string };
};

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const isoTime = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/;
const timeKey = /(At|Time|time|timestamp)$/;

function normalize(value: unknown, key = ""): unknown {
  if (Array.isArray(value)) return value.map((item) => normalize(item, key));
  if (value && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value).map(([name, item]) => [
        name,
        normalize(item, name),
      ]),
    );
  if (typeof value === "string") {
    if (uuid.test(value)) return "<uuid>";
    if (isoTime.test(value)) return "<time>";
    if (/^\/tmp\/|persona-contract-/.test(value)) return "<path>";
  }
  if (typeof value === "number" && timeKey.test(key) && value > 1e12)
    return "<time>";
  return value;
}

async function setup(computers?: Computers) {
  const directory = await mkdtemp(join(tmpdir(), "persona-contract-"));
  onTestFinished(() => rm(directory, { recursive: true, force: true }));
  const faux = fauxProvider();
  const models = createModels();
  models.setProvider(faux.provider);
  const database = join(directory, "agent.sqlite");
  const files = new Files(database);
  const registry = createRegistry();
  registry.install(files.extension());
  const agent = { model: { provider: "faux", modelId: "faux-1" } };
  const runtime = await openAgent(
    database,
    {
      models,
      registry,
      computers,
      env: ({ conversationId }: { conversationId: number }) =>
        files.workspace(conversationId),
    },
    computers ? {} : agent,
    context,
  );
  onTestFinished(() => runtime.harness.close(context));
  const app = createApp(
    runtime.bots,
    models,
    runtime.files,
    runtime.work,
    undefined,
    undefined,
    runtime.engines,
  );
  const exchanges: Exchange[] = [];
  const client: RouterClient<AppRouter> = createORPCClient(
    new RPCLink({
      url: "http://localhost/rpc",
      fetch: async (request, init) => {
        const raw = new Request(request, init);
        const path = new URL(raw.url).pathname;
        const body = await raw.clone().text();
        const response = await app.fetch(raw);
        const text = await response.clone().text();
        exchanges.push({
          procedure: path.replace(/^\/rpc\//, "").replaceAll("/", "."),
          request: {
            method: raw.method,
            path,
            body: normalize(body ? JSON.parse(body) : null),
          },
          response: {
            status: response.status,
            body: normalize(text ? JSON.parse(text) : null),
          },
        });
        return response;
      },
    }),
  );
  return { app, runtime, client, faux, exchanges };
}

async function compare(name: string, exchanges: Exchange[]) {
  const file = join(fixtures, `${name}.json`);
  const text = `${JSON.stringify(exchanges, null, 2)}\n`;
  if (record) {
    await mkdir(fixtures, { recursive: true });
    await writeFile(file, text);
  }
  expect(JSON.parse(text)).toEqual(JSON.parse(await readFile(file, "utf8")));
}

test("contract: bots and conversation", async () => {
  const { runtime, client, faux, exchanges } = await setup();
  const botId = runtime.conversation.id;
  await client.bots.list();
  await client.get({ botId });
  faux.setResponses([fauxAssistantMessage("Hello from Persona.")]);
  await client.send({ botId, text: "hello", requestId: crypto.randomUUID() });
  await runtime.conversation.waitForIdle(context);
  await client.get({ botId });
  await client.stop({ botId });
  await client.bots.create({
    name: "Contract",
    instructions: "Answer briefly.",
    model: { provider: "faux", modelId: "faux-1" },
    requestId: crypto.randomUUID(),
  });
  await compare("bots-conversation", exchanges);
});

test("contract: models and work", async () => {
  const { runtime, client, exchanges } = await setup();
  const botId = runtime.conversation.id;
  await client.models.list();
  await client.models.providers();
  await client.models.visibility();
  await client.engines.catalog();
  await client.work.get({ botId });
  await client.mcp.catalog();
  await compare("models-work", exchanges);
});

test("contract: errors", async () => {
  const { client, exchanges } = await setup();
  await expect(client.get({ botId: 999_999 })).rejects.toThrow();
  await expect(
    client.get({ botId: -1 } as unknown as { botId: number }),
  ).rejects.toThrow();
  await expect(client.engines.get({ botId: 999_999 })).rejects.toThrow();
  await expect(client.computer.list()).rejects.toThrow();
  await compare("errors", exchanges);
});

test("contract: Bot names are trimmed before validation", async () => {
  const { client, exchanges } = await setup();
  const name = "x".repeat(80);
  const input = {
    model: { provider: "faux", modelId: "faux-1" },
    requestId: crypto.randomUUID(),
  };
  await expect(
    client.bots.create({ ...input, name: " \t\n " }),
  ).rejects.toMatchObject({ code: "BAD_REQUEST" });
  const bot = await client.bots.create({
    ...input,
    requestId: crypto.randomUUID(),
    name: `  ${name}  `,
  });
  expect(bot.name).toBe(name);
  expect(
    procedures().find(({ path }) => path === "bots.create")?.input,
  ).toHaveProperty("properties.name", {
    type: "string",
    "x-persona-trimmed": { minLength: 1, maxLength: 80 },
  });
  await compare("bot-name-validation", exchanges);
});

function nativeComputer() {
  return {
    initialize: async () => {},
    preflight: async () => {},
    computerId: async () => "local",
    tools: { catalog: async () => [], finish: () => {} },
    process: async () => {
      const process = new ProtocolProcess();
      process.receive = (request) => {
        process.frame({
          type: "response",
          id: request.id,
          success: true,
          data:
            request.type === "get_state"
              ? { sessionFile: "/native/session" }
              : {},
        });
        if (request.type !== "prompt") return;
        const message = {
          role: "assistant",
          timestamp: 100,
          provider: "test-provider",
          model: "test-model",
          stopReason: "stop",
          content: [{ type: "text", text: "Hello from Pi." }],
          usage: { input: 12, output: 4, cacheRead: 0, cacheWrite: 0 },
        };
        process.frame({ type: "message_start", message });
        process.frame({ type: "message_end", message });
        process.frame({ type: "agent_settled" });
      };
      return process;
    },
  } as unknown as Computers;
}

test("contract: native conversation and event stream", async () => {
  const { app, runtime, client, exchanges } = await setup(nativeComputer());
  expect(runtime.engines).toBeDefined();
  const botId = runtime.conversation.id;
  await client.get({ botId });
  const submission = await client.send({
    botId,
    text: "hello",
    requestId: crypto.randomUUID(),
  });
  await (await runtime.harness.submission(submission.id, context))?.wait(
    context,
  );
  const view = await client.get({ botId });
  expect(view.messages).toContainEqual(
    expect.objectContaining({ role: "assistant" }),
  );

  const path = "/rpc/changes";
  const body = { json: { botId } };
  const response = await app.fetch(
    new Request(`http://localhost${path}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(5000),
    }),
  );
  expect(response.status).toBe(200);
  const contentType = response.headers.get("content-type");
  expect(contentType).toBe("text/event-stream");
  const reader = response.body?.getReader();
  if (!reader || !contentType) throw new Error("Event stream missing");
  const decoder = new TextDecoder();
  let pending = "";
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) throw new Error("Event stream closed before snapshot");
      pending += decoder.decode(value, { stream: true });
      const frames = pending.split("\n\n");
      const first = frames
        .slice(0, -1)
        .find((frame) => frame.includes("data:"));
      if (!first) continue;
      const frame = first.replace(
        /^data: (.*)$/m,
        (_, data: string) =>
          `data: ${JSON.stringify(normalize(JSON.parse(data)))}`,
      );
      expect(frame).toContain("event: message\n");
      expect(frame).toContain('"type":"snapshot"');
      exchanges.push({
        procedure: "changes",
        request: { method: "POST", path, body },
        response: {
          status: response.status,
          contentType,
          body: `${frame}\n\n`,
        },
      });
      break;
    }
  } finally {
    await reader.cancel();
  }
  await compare("native-conversation", exchanges);
});
