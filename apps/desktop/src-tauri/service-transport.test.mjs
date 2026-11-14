import assert from "node:assert/strict";
import { fork } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

for (const mode of ["shutdown", "early-disconnect"]) {
  test(`cleanup completes before exit: ${mode}`, {
    timeout: 10_000,
  }, async () => {
    const base = fileURLToPath(new URL("./target/test-data/", import.meta.url));
    await mkdir(base, { recursive: true });
    const directory = await mkdtemp(join(base, "transport-"));
    const marker = join(directory, "cleaned");
    const child = fork(
      fileURLToPath(new URL("./transport-fixture.mjs", import.meta.url)),
      [marker, mode],
      {
        execArgv: [],
        stdio: ["ignore", "ignore", "pipe", "ipc"],
      },
    );
    const exited = once(child, "exit");
    try {
      const [message] = await once(child, "message");
      if (mode === "shutdown") {
        assert.equal(message, "ready");
        const paired = once(child, "message");
        child.send("pair-desktop");
        assert.deepEqual(await paired, ["pairing-private", undefined]);
        child.send("shutdown");
        child.send("shutdown");
      } else {
        assert.equal(message, "initializing");
        child.disconnect();
      }
      assert.deepEqual(await exited, [0, null]);
      assert.equal(await readFile(marker, "utf8"), "cleanup-complete");
    } finally {
      if (child.exitCode === null && child.signalCode === null) {
        child.kill();
        await exited;
      }
      await rm(directory, { recursive: true, force: true });
    }
  });
}
