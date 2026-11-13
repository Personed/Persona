import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { createInterface } from "node:readline";
import test from "node:test";
import { fileURLToPath } from "node:url";

function supervisor() {
  const child = spawn(
    process.execPath,
    [fileURLToPath(new URL("./supervisor.mjs", import.meta.url))],
    {
      stdio: ["pipe", "pipe", "pipe"],
    },
  );
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
  const send = (type) => child.stdin.write(`${JSON.stringify({ type })}\n`);
  return { child, next, exited, send, frames, stdout: () => stdout };
}

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
