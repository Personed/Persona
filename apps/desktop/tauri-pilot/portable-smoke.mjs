import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { access } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { portable } from "./layout.mjs";
import { startTestService } from "./service.mjs";

assert.ok(
  portable,
  "Run with the bundled node.exe from a relocated portable TEST package.",
);
const service = await startTestService();
try {
  assert.equal((await fetch(`${service.origin}/health`)).status, 200);
  assert.equal((await fetch(`${service.origin}/index.html`)).status, 200);
  assert.equal((await fetch(`${service.origin}/hosting/session`)).status, 401);
  const session = await service.pair();
  const response = await fetch(`${service.origin}/hosting/session`, {
    headers: { authorization: `Bearer ${session.token}` },
  });
  assert.equal(response.status, 200);
  assert.equal((await response.json()).device.scope, "admin");
} finally {
  assert.deepEqual(await service.stop(), { graceful: true, cleanup: true });
}
assert.deepEqual(await service.exited, [0, null]);
await assert.rejects(access(service.directory), { code: "ENOENT" });
console.log(
  "portable real runtime: health/web=200 anonymous=401 private pairing=admin shutdown=0 disposable data removed",
);
const remote = spawnSync(
  process.execPath,
  [
    "--test",
    fileURLToPath(new URL("./remote-probe.test.mjs", import.meta.url)),
  ],
  { stdio: "inherit", timeout: 40_000 },
);
assert.ifError(remote.error);
assert.equal(
  remote.status,
  0,
  "Packaged remote probe policy and transport smoke failed",
);
