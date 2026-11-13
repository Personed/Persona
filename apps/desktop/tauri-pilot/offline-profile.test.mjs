import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import {
  access,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { createRequire } from "node:module";
import { join } from "node:path";
import test from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { fileURLToPath } from "node:url";
import { persistentCredentials } from "../../../packages/hosting/src/credentials.ts";
import { backupInstallation } from "../../../packages/hosting/src/maintenance.ts";
import { HostingStore } from "../../../packages/hosting/src/store.ts";
import { importPilotBackup } from "./import-backup.mjs";
import {
  createPersistentTestProfile,
  startPersistentService,
} from "./service.mjs";

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

test("offline synthetic Electron-equivalent backup: imported key, device, Bot and history survive pilot restart", {
  skip: process.platform !== "win32",
  timeout: 300_000,
}, async () => {
  const source = await createPersistentTestProfile();
  const base = fileURLToPath(new URL("./target/test-data/", import.meta.url));
  await mkdir(base, { recursive: true });
  const fixtureRoot = await mkdtemp(join(base, "import-backup-"));
  let sourceService;
  let importedService;
  let sourceDirectory;
  let importedDirectory;
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

    const password = randomUUID();
    const archive = join(fixtureRoot, "compatible.persona");
    const compatibleConfig = {
      dataDir: join(sourceDirectory, "data"),
      computer: { kind: "process" },
    };
    const configFile = join(sourceDirectory, "persona.config.json");
    await writeFile(configFile, JSON.stringify(compatibleConfig));
    await backupInstallation(sourceDirectory, archive, password);

    const unsupportedArchive = join(fixtureRoot, "unsupported.persona");
    await writeFile(
      configFile,
      JSON.stringify({
        ...compatibleConfig,
        workComputer: {
          kind: "docker",
          id: "fixture",
          image: "fixture:1",
          volume: "fixture",
        },
      }),
    );
    await backupInstallation(sourceDirectory, unsupportedArchive, password);
    await writeFile(configFile, JSON.stringify(compatibleConfig));
    const before = await snapshot(sourceDirectory);

    const wrong = join(fixtureRoot, "wrong");
    await assert.rejects(
      importPilotBackup(archive, `${password}-wrong`, wrong),
    );
    await assert.rejects(access(wrong), { code: "ENOENT" });
    await assert.rejects(access(`${wrong}.lock`), { code: "ENOENT" });

    const corruptArchive = join(fixtureRoot, "corrupt.persona");
    await writeFile(corruptArchive, "not an encrypted Persona backup");
    const corrupt = join(fixtureRoot, "corrupt");
    await assert.rejects(importPilotBackup(corruptArchive, password, corrupt));
    await assert.rejects(access(corrupt), { code: "ENOENT" });
    await assert.rejects(access(`${corrupt}.lock`), { code: "ENOENT" });

    const unsupported = join(fixtureRoot, "unsupported");
    await assert.rejects(
      importPilotBackup(unsupportedArchive, password, unsupported),
      /隔离电脑|computer/i,
    );
    await assert.rejects(access(unsupported), { code: "ENOENT" });
    await assert.rejects(access(`${unsupported}.lock`), { code: "ENOENT" });

    importedDirectory = join(fixtureRoot, "imported");
    await importPilotBackup(archive, password, importedDirectory);
    const importedSnapshot = await snapshot(importedDirectory);
    await assert.rejects(
      importPilotBackup(archive, password, importedDirectory),
      /já existe|exists/i,
    );
    assert.deepEqual(await snapshot(importedDirectory), importedSnapshot);

    importedService = await startPersistentService(importedDirectory);
    assert.equal(
      (await fetch(`${importedService.origin}/hosting/session`)).status,
      401,
    );
    const restoredSession = await fetch(
      `${importedService.origin}/hosting/session`,
      {
        headers: { authorization: `Bearer ${token}` },
      },
    );
    assert.equal(restoredSession.status, 200);
    assert.deepEqual((await restoredSession.json()).device, device);
    const restoredApi = connect(importedService, token);
    assert.ok(
      (await restoredApi.bots.list()).some(
        (item) => item.id === bot.id && item.name === bot.name,
      ),
    );
    assert.deepEqual(
      (await restoredApi.get({ botId: bot.id })).messages,
      history.messages,
    );
    await stop(importedService);
    importedService = undefined;

    const importedStore = await HostingStore.open(
      join(importedDirectory, "data"),
    );
    try {
      const restored =
        await persistentCredentials(importedStore).read("offline-synthetic");
      assert.ok(
        JSON.stringify(restored) === JSON.stringify(credential),
        "Imported credential did not decrypt",
      );
    } finally {
      await importedStore.close();
    }

    importedService = await startPersistentService(importedDirectory);
    assert.deepEqual(
      (await connect(importedService, token).get({ botId: bot.id })).messages,
      history.messages,
    );
    await stop(importedService);
    importedService = undefined;
    assert.deepEqual(
      await snapshot(sourceDirectory),
      before,
      "Source changed while backup import was running",
    );
  } finally {
    if (importedService) await stop(importedService);
    if (sourceService) await stop(sourceService);
    await rm(fixtureRoot, { recursive: true, force: true });
    await source.cleanup();
  }
});
