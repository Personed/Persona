import assert from "node:assert/strict";
import { access } from "node:fs/promises";
import test from "node:test";
import { startTestService } from "./service.mjs";

test("real temporary service: private pairing, auth, shutdown and cleanup", {
  timeout: 120_000,
}, async () => {
  const service = await startTestService();
  try {
    const health = await fetch(`${service.origin}/health`);
    assert.equal(health.status, 200);
    assert.deepEqual(await health.json(), { ok: true });
    assert.equal(
      (await fetch(`${service.origin}/hosting/session`)).status,
      401,
    );
    assert.equal(
      (
        await fetch(`${service.origin}/hosting/desktop`, {
          method: "POST",
          headers: { origin: service.origin },
        })
      ).status,
      404,
    );
    const session = await service.pair();
    assert.ok(session.token.length >= 32);
    assert.ok(session.expires > Date.now());
    const authenticated = await fetch(`${service.origin}/hosting/session`, {
      headers: { authorization: `Bearer ${session.token}` },
    });
    assert.equal(authenticated.status, 200);
    assert.equal((await authenticated.json()).device.scope, "admin");
    assert.equal(
      (
        await fetch(`${service.origin}/hosting/session`, {
          headers: {
            origin: "https://example.com",
            authorization: `Bearer ${session.token}`,
          },
        })
      ).status,
      403,
    );
  } finally {
    assert.deepEqual(await service.stop(), { graceful: true, cleanup: true });
  }
  const [code, signal] = await service.exited;
  assert.equal(code, 0);
  assert.equal(signal, null);
  await assert.rejects(access(service.directory), { code: "ENOENT" });
  console.log(
    "health=200 anonymous=401 desktop-http=404 paired=200 cross-origin=403 shutdown=0 temporary-data=removed",
  );
});

test("forced service exit reports uncertainty and retains disposable data", {
  timeout: 120_000,
}, async () => {
  const service = await startTestService();
  try {
    process.kill(service.pid);
    await service.exited;
    assert.deepEqual(await service.stop(), { graceful: false, cleanup: false });
    assert.deepEqual(await service.stop(), { graceful: false, cleanup: false });
    await access(service.directory);
  } finally {
    await service.stop();
  }
});
