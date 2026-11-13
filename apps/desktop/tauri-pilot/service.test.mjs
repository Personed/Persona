import assert from "node:assert/strict";
import {
  access,
  mkdtemp,
  readFile,
  rm,
  symlink,
  unlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import {
  createPersistentTestProfile,
  pilotProfileMarker,
  startPersistentService,
  startTestService,
} from "./service.mjs";

test("real temporary service: private pairing, auth, shutdown and cleanup", {
  timeout: 120_000,
}, async () => {
  const service = await startTestService();
  try {
    const health = await fetch(`${service.origin}/health`);
    assert.equal(health.status, 200);
    assert.ok(Number(new URL(service.origin).port) >= 49152);
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

test("imported Docker work template remains local and configured", {
  timeout: 120_000,
}, async () => {
  const directory = await mkdtemp(join(tmpdir(), "persona-pilot-work-"));
  const workComputer = {
    kind: "docker",
    id: "synthetic-template",
    image: "fixture:1",
    volume: "synthetic-volume",
  };
  await writeFile(
    join(directory, "persona.config.json"),
    JSON.stringify({ computer: { kind: "process" }, workComputer }),
  );
  await writeFile(join(directory, "pilot-profile"), pilotProfileMarker);
  const service = await startPersistentService(directory);
  try {
    const config = JSON.parse(
      await readFile(join(service.directory, "config.json"), "utf8"),
    );
    assert.deepEqual(config.workComputer, workComputer);
    assert.deepEqual(config.computer, { kind: "process" });
  } finally {
    assert.deepEqual(await service.stop(), { graceful: true, cleanup: false });
    await service.exited;
    await rm(directory, { recursive: true, force: true });
  }
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

test("isolated persistent profile retains an actual paired device across restart", {
  timeout: 240_000,
}, async () => {
  const profile = await createPersistentTestProfile();
  let service;
  try {
    const starting = profile.start();
    const blocked = Promise.all([
      assert.rejects(profile.start(), /still owned/),
      assert.rejects(profile.cleanup(), /still owned/),
    ]);
    service = await starting;
    await blocked;
    await assert.rejects(profile.start(), /still owned/);
    await assert.rejects(profile.cleanup(), /still owned/);
    const session = await service.pair();
    const read = async () => {
      const response = await fetch(`${service.origin}/hosting/session`, {
        headers: { authorization: `Bearer ${session.token}` },
      });
      assert.equal(response.status, 200);
      return (await response.json()).device;
    };
    const device = await read();
    assert.equal(device.scope, "admin");
    assert.ok(device.id);
    const first = service;
    assert.deepEqual(await first.stop(), { graceful: true, cleanup: false });
    assert.deepEqual(await first.exited, [0, null]);
    await access(first.directory);
    service = await profile.start();
    assert.equal(service.directory, first.directory);
    assert.deepEqual(await read(), device);
    await first.stop();
    await assert.rejects(profile.cleanup(), /still owned/);
    assert.deepEqual(await service.stop(), { graceful: true, cleanup: false });
    assert.deepEqual(await service.exited, [0, null]);
    const link = join(service.directory, "unsafe-link");
    await symlink(
      service.directory,
      link,
      process.platform === "win32" ? "junction" : "dir",
    );
    try {
      await assert.rejects(profile.start(), /symbolic link/);
      await assert.rejects(profile.cleanup(), /symbolic link/);
    } finally {
      await unlink(link);
    }
  } finally {
    if (service) {
      assert.deepEqual(await service.stop(), {
        graceful: true,
        cleanup: false,
      });
      assert.deepEqual(await service.exited, [0, null]);
    }
    const cleaning = profile.cleanup();
    await Promise.all([
      cleaning,
      assert.rejects(profile.start(), /closed or still owned/),
      assert.rejects(profile.cleanup(), /still owned/),
    ]);
  }
  await assert.rejects(access(service.directory), { code: "ENOENT" });
});
