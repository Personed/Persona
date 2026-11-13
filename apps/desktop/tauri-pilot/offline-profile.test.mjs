import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import { access, cp, readdir, readFile, rm } from "node:fs/promises";
import { createRequire } from "node:module";
import { join } from "node:path";
import test from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { persistentCredentials } from "../../../packages/hosting/src/credentials.ts";
import { HostingStore } from "../../../packages/hosting/src/store.ts";
import { createPersistentTestProfile } from "./service.mjs";

const require = createRequire(
  new URL("../../api/package.json", import.meta.url),
);
const { createORPCClient } = require("@orpc/client");
const { RPCLink } = require("@orpc/client/fetch");

async function snapshot(directory) {
  const result = {};
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    assert.ok(
      !entry.isSymbolicLink(),
      "Synthetic fixture must not contain links",
    );
    result[entry.name] = entry.isDirectory()
      ? await snapshot(path)
      : createHash("sha256")
          .update(await readFile(path))
          .digest("hex");
  }
  return result;
}

test("offline synthetic Electron-equivalent profile: copied key, device, Bot and history survive pilot service", {
  skip: process.platform !== "win32",
  timeout: 240_000,
}, async () => {
  // Both capabilities allocate fresh pilot-owned roots; no supplied profile paths.
  const source = await createPersistentTestProfile();
  const copy = await createPersistentTestProfile();
  let sourceService;
  let copyService;
  let sourceDirectory;
  let copyDirectory;
  const connect = (service, token) =>
    createORPCClient(
      new RPCLink({
        url: `${service.origin}/rpc`,
        headers: { authorization: `Bearer ${token}` },
        fetch: (request, init) =>
          fetch(request, { ...init, signal: AbortSignal.timeout(60_000) }),
      }),
    );
  const stop = async (service) => {
    assert.deepEqual(await service.stop(), { graceful: true, cleanup: false });
    assert.deepEqual(await service.exited, [0, null]);
  };
  try {
    sourceService = await source.start();
    sourceDirectory = sourceService.directory;
    const { token } = await sourceService.pair();
    const deviceResponse = await fetch(
      `${sourceService.origin}/hosting/session`,
      {
        headers: { authorization: `Bearer ${token}` },
      },
    );
    assert.equal(deviceResponse.status, 200);
    const device = (await deviceResponse.json()).device;
    const api = connect(sourceService, token);
    const computer = (await api.computer.list()).items.find(
      (item) => item.id === "process",
    );
    assert.ok(computer);
    if (!computer.authorized)
      await api.computer.authorize({ id: computer.id, authorized: true });
    const bot = await api.bots.create({
      name: "Offline synthetic compatibility",
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
        args: [
          fileURLToPath(new URL("./chat-acp.fixture.mjs", import.meta.url)),
        ],
      },
    });
    const marker = `portable-chat-${randomUUID()}`;
    await api.send({ botId: bot.id, text: marker, requestId: randomUUID() });
    const text = (message) =>
      message.content
        .filter((block) => block.type === "text")
        .map((block) => block.text)
        .join("");
    const expected = `Synthetic reply: ${marker}; Persona product tools discovered.`;
    let history;
    const deadline = performance.now() + 60_000;
    for (;;) {
      history = await api.get({ botId: bot.id });
      assert.ok(
        !history.failed && !history.error,
        "Synthetic offline turn failed",
      );
      if (
        !history.busy &&
        history.messages.some(
          (message) =>
            message.role === "assistant" && text(message) === expected,
        )
      )
        break;
      assert.ok(
        performance.now() < deadline,
        "Synthetic history did not settle",
      );
      await delay(100);
    }
    assert.ok(
      history.messages.some(
        (message) => message.role === "user" && text(message) === marker,
      ),
    );
    await stop(sourceService);
    sourceService = undefined;
    // Electron and the pilot use this same production hosting store and agent service.
    const credential = {
      type: "api_key",
      key: `synthetic-only-${randomUUID()}`,
    };
    const sourceStore = await HostingStore.open(join(sourceDirectory, "data"));
    try {
      await persistentCredentials(sourceStore).modify(
        "offline-synthetic",
        () => credential,
      );
    } finally {
      await sourceStore.close();
    }
    copyService = await copy.start();
    copyDirectory = copyService.directory;
    await stop(copyService);
    copyService = undefined;
    const before = await snapshot(sourceDirectory);
    // Copy the entire settled directory, not selected SQLite files or a reconstructed marker.
    await rm(copyDirectory, { recursive: true });
    await cp(sourceDirectory, copyDirectory, { recursive: true });
    assert.deepEqual(await snapshot(copyDirectory), before);
    copyService = await copy.start();
    assert.equal(
      (await fetch(`${copyService.origin}/hosting/session`)).status,
      401,
    );
    const restoredSession = await fetch(
      `${copyService.origin}/hosting/session`,
      {
        headers: { authorization: `Bearer ${token}` },
      },
    );
    assert.equal(restoredSession.status, 200);
    assert.deepEqual((await restoredSession.json()).device, device);
    const restoredApi = connect(copyService, token);
    assert.ok(
      (await restoredApi.bots.list()).some(
        (item) => item.id === bot.id && item.name === bot.name,
      ),
    );
    assert.deepEqual(
      (await restoredApi.get({ botId: bot.id })).messages,
      history.messages,
    );
    await stop(copyService);
    copyService = undefined;
    const copiedStore = await HostingStore.open(join(copyDirectory, "data"));
    try {
      // Do not let an assertion print credentials, even though these are synthetic.
      const restored =
        await persistentCredentials(copiedStore).read("offline-synthetic");
      assert.ok(
        JSON.stringify(restored) === JSON.stringify(credential),
        "Copied credential did not decrypt",
      );
    } finally {
      await copiedStore.close();
    }
    assert.deepEqual(
      await snapshot(sourceDirectory),
      before,
      "Source changed while copy was running",
    );
    await copy.cleanup();
    await assert.rejects(access(copyDirectory), { code: "ENOENT" });
    assert.deepEqual(
      await snapshot(sourceDirectory),
      before,
      "Dropping the copy changed source",
    );
  } finally {
    // Uncertain shutdown retains the affected fixture; cleanup cannot bypass ownership.
    if (copyService) await stop(copyService);
    if (sourceService) await stop(sourceService);
    await copy.cleanup();
    await source.cleanup();
  }
});
