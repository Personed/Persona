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
import { Files } from "@persona/agent/files";
import { expect, onTestFinished, test } from "vitest";
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
  response: { status: number; body: unknown };
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

async function setup() {
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
      env: ({ conversationId }: { conversationId: number }) =>
        files.workspace(conversationId),
    },
    agent,
    context,
  );
  onTestFinished(() => runtime.harness.close(context));
  const app = createApp(runtime.bots, models, runtime.files, runtime.work);
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
  return { runtime, client, faux, exchanges };
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
