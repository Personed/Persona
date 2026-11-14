import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { createRequire } from "node:module";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { portable } from "./layout.mjs";
import { createPersistentTestProfile } from "./service.mjs";

assert.ok(
  portable,
  "Run using the relocated portable TEST package's node.exe.",
);
const require = createRequire(
  new URL("./runtime/package.json", import.meta.url),
);
const { createORPCClient } = require("@orpc/client");
const { RPCLink } = require("@orpc/client/fetch");
const profile = await createPersistentTestProfile();
const marker = `portable-chat-${randomUUID()}`;
const expected = `Synthetic reply: ${marker}; Persona product tools discovered.`;
const started = performance.now();
let service;
async function connect() {
  service = await profile.start();
  const { token } = await service.pair();
  return createORPCClient(
    new RPCLink({
      url: `${service.origin}/rpc`,
      headers: { authorization: `Bearer ${token}` },
      fetch: (request, init) =>
        fetch(request, {
          ...init,
          signal: AbortSignal.timeout(60_000),
        }),
    }),
  );
}
const text = (message) =>
  message.content
    .filter((block) => block.type === "text")
    .map((block) => block.text)
    .join("");
function persisted(view) {
  assert.ok(!view.busy && !view.failed && !view.error);
  const user = view.messages.filter(
    (message) => message.role === "user" && text(message) === marker,
  );
  const replies = view.messages.filter(
    (message) => message.role === "assistant" && text(message) === expected,
  );
  assert.equal(user.length, 1);
  assert.equal(replies.length, 1);
  assert.ok(!replies[0].pending && !replies[0].error && !replies[0].stopped);
  return replies[0];
}
try {
  let api = await connect();
  assert.equal((await fetch(`${service.origin}/rpc/bots/list`)).status, 401);
  const computers = await api.computer.list();
  const computer = computers.items.find((item) => item.id === "process");
  assert.ok(computer, "Isolated service computer must be available");
  if (!computer.authorized)
    await api.computer.authorize({ id: computer.id, authorized: true });
  const bot = await api.bots.create({
    name: "Portable synthetic chat",
    computerId: computer.id,
    requestId: randomUUID(),
  });
  const { revision } = await api.engines.get({ botId: bot.id });
  await api.engines.configure({
    botId: bot.id,
    revision,
    requestId: randomUUID(),
    config: {
      provider: "acp",
      model: null,
      command: process.execPath,
      args: [fileURLToPath(new URL("./chat-acp.fixture.mjs", import.meta.url))],
    },
  });
  const input = { botId: bot.id, text: marker, requestId: randomUUID() };
  const receipt = await api.send(input);
  assert.ok(receipt.id);
  const deadline = performance.now() + 60_000;
  let view;
  for (;;) {
    view = await api.get({ botId: bot.id });
    assert.ok(!view.failed && !view.error, "Synthetic turn failed");
    if (
      !view.busy &&
      view.messages.some(
        (message) => message.role === "assistant" && text(message) === expected,
      )
    )
      break;
    assert.ok(
      performance.now() < deadline,
      "Persisted synthetic response timed out",
    );
    await delay(100);
  }
  const reply = persisted(view);
  assert.deepEqual(
    await api.send(input),
    receipt,
    "Stable request ID must deduplicate admission",
  );
  assert.equal(persisted(await api.get({ botId: bot.id })).id, reply.id);
  assert.deepEqual(await service.stop(), { graceful: true, cleanup: false });
  assert.deepEqual(await service.exited, [0, null]);
  service = undefined;
  api = await connect();
  const restored = persisted(await api.get({ botId: bot.id }));
  assert.equal(restored.id, reply.id);
  assert.equal(restored.cursor, reply.cursor);
  assert.ok((await api.bots.list()).some((item) => item.id === bot.id));
  console.log(
    `portable Bot chat: authenticated HTTP -> Runtime.submit -> custom ACP + real MCP discovery -> persisted reply; request deduplication + graceful restart history verified (${Math.round(performance.now() - started)}ms, 1 synthetic turn, 0 model requests)`,
  );
} finally {
  if (service)
    assert.deepEqual(await service.stop(), { graceful: true, cleanup: false });
  await profile.cleanup();
}
