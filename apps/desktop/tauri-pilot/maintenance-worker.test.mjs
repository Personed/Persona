import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import {
  access,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { buildPilotBundle } from "./build-pilot-bundle.mjs";
import { pilotProfileMarker } from "./service.mjs";

function workerEnvironment() {
  return Object.fromEntries(
    ["PATH", "SystemRoot", "WINDIR", "SystemDrive", "COMSPEC", "PATHEXT"]
      .filter((key) => process.env[key])
      .map((key) => [key, process.env[key]]),
  );
}

async function runWorker(directory, request) {
  const child = spawn(
    process.execPath,
    [join(directory, "maintenance-worker.mjs")],
    {
      cwd: directory,
      env: workerEnvironment(),
      stdio: ["pipe", "ignore", "pipe"],
      windowsHide: true,
    },
  );
  const lines = createInterface({ input: child.stderr, crlfDelay: Infinity });
  const exited = once(child, "exit");
  let rejectTimeout;
  const timedOut = new Promise((_, reject) => {
    rejectTimeout = reject;
  });
  const timer = setTimeout(() => {
    child.kill();
    rejectTimeout(new Error("Maintenance worker timed out"));
  }, 120_000);
  const frames = [];
  let ready;
  let result;
  const readyFrame = new Promise((resolve, reject) => {
    ready = resolve;
    child.once("error", reject);
  });
  const resultFrame = new Promise((resolve, reject) => {
    result = resolve;
    child.once("error", reject);
  });
  lines.on("line", (line) => {
    try {
      const frame = JSON.parse(line);
      frames.push(frame);
      if (frame.type === "ready") ready(frame);
      if (frame.type === "result") result(frame);
    } catch (error) {
      result(error);
    }
  });
  try {
    assert.deepEqual(await Promise.race([readyFrame, timedOut]), {
      type: "ready",
    });
    child.stdin.end(`${JSON.stringify({ type: "start", ...request })}\n`);
    const terminal = await Promise.race([resultFrame, timedOut]);
    const [code] = await Promise.race([exited, timedOut]);
    return { code, frames, terminal };
  } finally {
    clearTimeout(timer);
    lines.close();
    child.stdin.destroy();
    if (child.exitCode === null) child.kill();
  }
}

test("packaged worker backs up and restores only into a fresh isolated profile", async () => {
  const packageDirectory = await mkdtemp(
    join(tmpdir(), "pilot-maintenance-worker-"),
  );
  const profileBase = join(packageDirectory, "target", "test-data");
  const source = join(profileBase, "pilot-local-v1");
  const archive = join(packageDirectory, "synthetic.persona");
  const password = "synthetic-only-password";
  try {
    await mkdir(join(source, "data"), { recursive: true });
    await writeFile(join(packageDirectory, "portable-test.json"), "{}", {
      flag: "wx",
    });
    await writeFile(join(source, "pilot-profile"), pilotProfileMarker);
    await writeFile(
      join(source, "config.json"),
      JSON.stringify({ dataDir: "data", computer: { kind: "process" } }),
    );
    await writeFile(join(source, "data", "synthetic.txt"), "synthetic only");
    await buildPilotBundle(
      fileURLToPath(new URL("./maintenance-operation.mjs", import.meta.url)),
      join(packageDirectory, "maintenance-operation.mjs"),
    );
    assert.match(
      await readFile(
        join(packageDirectory, "maintenance-operation.mjs"),
        "utf8",
      ),
      /persona-tauri-pilot-docker-v1/,
    );
    await writeFile(
      join(packageDirectory, "maintenance-worker.mjs"),
      await readFile(new URL("./maintenance-worker.mjs", import.meta.url)),
    );
    await writeFile(
      join(packageDirectory, "maintenance-protocol.mjs"),
      await readFile(new URL("./maintenance-protocol.mjs", import.meta.url)),
    );

    const backup = await runWorker(packageDirectory, {
      action: "backup",
      archive,
      password,
    });
    assert.equal(backup.code, 0);
    assert.deepEqual(backup.terminal, { type: "result", ok: true });
    assert.equal(backup.frames[0].type, "ready");
    await access(archive);

    const restore = await runWorker(packageDirectory, {
      action: "restore",
      archive,
      password,
    });
    assert.equal(restore.code, 0);
    assert.deepEqual(restore.terminal, { type: "result", ok: true });
    const selected = JSON.parse(
      await readFile(join(profileBase, "pilot-local-current.json"), "utf8"),
    );
    assert.match(selected.profile, /^pilot-local-restore-/);
    const restored = join(profileBase, selected.profile);
    assert.equal(
      await readFile(join(restored, "data", "synthetic.txt"), "utf8"),
      "synthetic only",
    );
    assert.equal(
      await readFile(join(source, "data", "synthetic.txt"), "utf8"),
      "synthetic only",
    );
    assert.equal(
      JSON.stringify([...backup.frames, ...restore.frames]).includes(password),
      false,
    );
  } finally {
    await rm(packageDirectory, { recursive: true, force: true });
  }
});
