import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import {
  access,
  copyFile,
  mkdir,
  mkdtemp,
  rm,
  writeFile,
} from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import test from "node:test";
import { fileURLToPath } from "node:url";

function supervisor(
  script = fileURLToPath(new URL("./supervisor.mjs", import.meta.url)),
) {
  const child = spawn(process.execPath, [script], {
    stdio: ["pipe", "pipe", "pipe"],
  });
  let stdout = "";
  child.stdout.on("data", (chunk) => {
    stdout += chunk;
  });
  const lines = createInterface({ input: child.stderr });
  const frames = [];
  lines.on("line", (line) => frames.push(JSON.parse(line)));
  const iterator = lines[Symbol.asyncIterator]();
  const next = async () => {
    const result = await iterator.next();
    assert.equal(result.done, false);
    return JSON.parse(result.value);
  };
  const exited = once(child, "exit");
  const send = (type, value = {}) =>
    child.stdin.write(`${JSON.stringify({ type, ...value })}\n`);
  return { child, next, exited, send, frames, stdout: () => stdout };
}

test("private protocol routes Docker selection, reports progress, and verifies local pairing without Docker", {
  timeout: 30_000,
}, async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "persona-docker-supervisor-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const server = createServer((request, response) => {
    if (request.url !== "/hosting/session") {
      response.writeHead(404).end();
      return;
    }
    response.writeHead(200, { "content-type": "application/json" });
    response.end(JSON.stringify({ device: { scope: "admin" } }));
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  assert.ok(address && typeof address === "object");
  const origin = `http://127.0.0.1:${address.port}`;
  const token = "b".repeat(64);
  for (const name of [
    "supervisor.mjs",
    "layout.mjs",
    "service.mjs",
    "service-child.mjs",
    "service-transport.mjs",
    "stop-child.mjs",
    "local-session.mjs",
  ])
    await copyFile(new URL(name, import.meta.url), join(directory, name));
  await writeFile(
    join(directory, "docker-service.mjs"),
    `export async function startPilotDockerService({progress}) {
      progress({phase:"checking",message:"Synthetic Docker check"});
      return {
        origin:${JSON.stringify(origin)},
        pair:async()=>({token:${JSON.stringify(token)},expires:Date.now()+60000}),
        exited:new Promise(()=>{}),
        stop:async()=>({graceful:true,cleanup:false})
      };
    }\n`,
  );

  const control = supervisor(join(directory, "supervisor.mjs"));
  try {
    control.send("start", { runtime: "docker", persistent: true });
    assert.deepEqual(await control.next(), {
      type: "progress",
      phase: "checking",
      message: "Synthetic Docker check",
    });
    const ready = await control.next();
    assert.equal(ready.type, "ready");
    assert.equal(ready.origin, origin);
    assert.equal(ready.token, token);
    assert.equal("servicePid" in ready, false);
    control.send("stop");
    assert.deepEqual(await control.next(), {
      type: "stopped",
      mode: "graceful",
      cleanup: false,
    });
    assert.deepEqual(await control.exited, [0, null]);
  } finally {
    if (control.child.exitCode === null) {
      control.child.stdin.end();
      await control.exited;
    }
    server.close();
  }
});

test("private pipes: owned service readiness, pairing, cookie auth, idempotent stop", {
  timeout: 120_000,
}, async () => {
  const control = supervisor();
  try {
    control.send("start");
    control.send("start");
    const ready = await control.next();
    assert.equal(ready.type, "ready");
    assert.match(ready.origin, /^http:\/\/127\.0\.0\.1:\d+$/);
    assert.ok(ready.expires > Date.now());
    const session = await fetch(`${ready.origin}/hosting/session`, {
      headers: { cookie: `persona_session=${ready.token}` },
    });
    assert.equal(session.status, 200);
    assert.equal((await session.json()).device.scope, "admin");
    assert.equal(
      (
        await fetch(`${ready.origin}/hosting/session`, {
          headers: {
            origin: "https://example.com",
            cookie: `persona_session=${ready.token}`,
          },
        })
      ).status,
      403,
    );
    control.send("stop");
    control.send("stop");
    assert.deepEqual(await control.next(), {
      type: "stopped",
      mode: "graceful",
      cleanup: true,
    });
    assert.deepEqual(await control.exited, [0, null]);
    assert.equal(control.stdout(), "");
    assert.equal(
      control.frames.filter((frame) => frame.type === "ready").length,
      1,
    );
    assert.throws(() => process.kill(ready.servicePid, 0), { code: "ESRCH" });
  } finally {
    if (control.child.exitCode === null) {
      control.child.stdin.end();
      await control.exited;
    }
  }
});

test("persistent pilot workflow survives separate supervisor/backend launches in a fresh installation fixture", {
  timeout: 240_000,
}, async () => {
  const base = fileURLToPath(new URL("./target/test-data/", import.meta.url));
  await mkdir(base, { recursive: true });
  const directory = await mkdtemp(join(base, "persistent-install-"));
  const controls = [];
  let cleanupSafe = false;
  try {
    for (const name of [
      "supervisor.mjs",
      "docker-profile.mjs",
      "docker-service.mjs",
      "service.mjs",
      "service-child.mjs",
      "service-transport.mjs",
      "stop-child.mjs",
      "local-session.mjs",
    ])
      await copyFile(new URL(name, import.meta.url), join(directory, name));
    // Isolate profile storage while both launches run the actual backend.
    await writeFile(
      join(directory, "layout.mjs"),
      `export * from ${JSON.stringify(new URL("./layout.mjs", import.meta.url).href)};\nexport const dataDirectory = ${JSON.stringify(join(directory, "target/test-data"))};\n`,
    );
    const script = join(directory, "supervisor.mjs");
    const launch = () => {
      const control = supervisor(script);
      controls.push(control);
      control.send("start", { persistent: true });
      return control;
    };
    const first = launch();
    const ready = await first.next();
    assert.equal(ready.type, "ready");
    const read = async (origin) => {
      const response = await fetch(`${origin}/hosting/session`, {
        headers: { authorization: `Bearer ${ready.token}` },
      });
      assert.equal(response.status, 200);
      return (await response.json()).device;
    };
    const saved = await read(ready.origin);
    assert.ok(saved.id);
    const contender = launch();
    assert.equal((await contender.next()).type, "failed");
    assert.equal((await contender.next()).type, "stopped");
    await contender.exited;
    const stop = async (control, pid) => {
      control.send("stop");
      assert.deepEqual(await control.next(), {
        type: "stopped",
        mode: "graceful",
        cleanup: false,
      });
      assert.deepEqual(await control.exited, [0, null]);
      assert.throws(() => process.kill(pid, 0), { code: "ESRCH" });
    };
    await stop(first, ready.servicePid);
    const profile = join(directory, "target/test-data/pilot-local-v1");
    await access(join(profile, "data"));
    const second = launch();
    const restarted = await second.next();
    assert.equal(restarted.type, "ready");
    assert.deepEqual(await read(restarted.origin), saved);
    await stop(second, restarted.servicePid);
    await access(join(profile, "data"));
    cleanupSafe = true;
  } finally {
    for (const control of controls) {
      if (control.child.exitCode === null) control.child.stdin.end();
      await control.exited;
    }
    // All fixture supervisors have exited, and successful stops attest backend exit.
    if (cleanupSafe) await rm(directory, { recursive: true });
  }
});

test("EOF during startup cancels without announcing ready", {
  timeout: 120_000,
}, async () => {
  const control = supervisor();
  control.send("start");
  control.child.stdin.end();
  assert.deepEqual(await control.exited, [0, null]);
  assert.equal(
    control.frames.some((frame) => frame.type === "ready"),
    false,
  );
  assert.equal(control.stdout(), "");
});

test("owned service death is terminal, with no restart", {
  timeout: 120_000,
}, async () => {
  const control = supervisor();
  try {
    control.send("start");
    const ready = await control.next();
    assert.equal(ready.type, "ready");
    // Only the PID received from our private supervisor is terminated.
    process.kill(ready.servicePid);
    assert.equal((await control.next()).type, "died");
    assert.deepEqual(await control.next(), {
      type: "stopped",
      mode: "forced",
      cleanup: false,
    });
    assert.deepEqual(await control.exited, [1, null]);
    assert.equal(
      control.frames.filter((frame) => frame.type === "ready").length,
      1,
    );
  } finally {
    if (control.child.exitCode === null) {
      control.child.stdin.end();
      await control.exited;
    }
  }
});
