import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { access, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { backupInstallation } from "./backup-maintenance.mjs";
import { HostingStore, persistentCredentials } from "./hosting-store.mjs";
import { portable } from "./layout.mjs";
import { startPilotService } from "./service.mjs";

assert.ok(
  portable,
  "Run with the bundled node.exe from a relocated portable TEST package.",
);
const base = dirname(fileURLToPath(import.meta.url));
const testData = join(base, "target", "test-data");
await mkdir(testData, { recursive: true });
const source = await mkdtemp(join(testData, "portable-import-source-"));
const data = join(source, "data");
await mkdir(data);
const password = randomUUID();
const credential = {
  type: "api_key",
  key: `synthetic-only-${randomUUID()}`,
};
let service;
let importedDirectory;
let archive;
try {
  await writeFile(
    join(source, "persona.config.json"),
    JSON.stringify({ dataDir: data, computer: { kind: "process" } }),
  );
  const store = await HostingStore.open(data);
  try {
    await persistentCredentials(store).modify(
      "portable-import",
      () => credential,
    );
  } finally {
    await store.close();
  }
  archive = join(testData, `portable-import-archive-${randomUUID()}.persona`);
  await backupInstallation(source, archive, password);
  const imported = spawnSync(
    join(base, "persona-tauri-pilot.exe"),
    ["--import-backup", archive],
    {
      cwd: base,
      input: password,
      encoding: "utf8",
      timeout: 120_000,
      windowsHide: true,
    },
  );
  assert.ifError(imported.error);
  assert.equal(
    imported.status,
    0,
    `Packaged backup import failed: ${imported.stderr}`,
  );
  service = await startPilotService();
  importedDirectory = service.directory;
  const session = await service.pair();
  const response = await fetch(`${service.origin}/hosting/session`, {
    headers: { authorization: `Bearer ${session.token}` },
  });
  assert.equal(response.status, 200);
  assert.equal((await response.json()).device.scope, "admin");
  assert.deepEqual(await service.stop(), { graceful: true, cleanup: false });
  assert.deepEqual(await service.exited, [0, null]);
  service = undefined;
  const restored = await HostingStore.open(join(importedDirectory, "data"));
  try {
    assert.deepEqual(
      await persistentCredentials(restored).read("portable-import"),
      credential,
    );
  } finally {
    await restored.close();
  }
  console.log(
    "portable backup import: encrypted archive -> isolated pilot profile -> authenticated service -> credential decrypt verified",
  );
} finally {
  if (service) await service.stop();
}
await rm(join(testData, "pilot-local-v1"), {
  recursive: true,
  force: true,
});
await rm(source, { recursive: true, force: true });
if (archive) await rm(archive, { force: true });
await assert.rejects(access(join(testData, "pilot-local-v1")), {
  code: "ENOENT",
});
