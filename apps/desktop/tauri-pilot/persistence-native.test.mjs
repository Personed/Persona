import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import {
  access,
  copyFile,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

async function writeFixtureLayout(directory) {
  const layout = join(directory, "layout.mjs");
  await writeFile(
    layout,
    `export * from ${JSON.stringify(new URL("./layout.mjs", import.meta.url).href)};\nexport const dataDirectory = ${JSON.stringify(join(directory, "target/test-data"))};\n`,
  );
  return import(pathToFileURL(layout).href);
}

test("native persistence fixture layout keeps profile storage inside its own installation", async () => {
  const base = fileURLToPath(new URL("./target/test-data/", import.meta.url));
  await mkdir(base, { recursive: true });
  const directory = await mkdtemp(join(base, "native-layout-"));
  try {
    const layout = await writeFixtureLayout(directory);
    assert.equal(layout.dataDirectory, join(directory, "target/test-data"));
    assert.equal(
      layout.runtimeEntry.href,
      new URL("../../api/src/main.ts", import.meta.url).href,
    );
  } finally {
    await rm(directory, { recursive: true });
  }
});

test("native persistent pilot: synthetic application record survives complete executable restart", {
  skip: process.platform !== "win32",
  timeout: 300_000,
}, async () => {
  const base = fileURLToPath(new URL("./target/test-data/", import.meta.url));
  await mkdir(base, { recursive: true });
  const directory = await mkdtemp(join(base, "native-persistence-"));
  let cleanupSafe = true;
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
    await copyFile(
      new URL("./target/debug/persona-tauri-pilot.exe", import.meta.url),
      join(directory, "persona-tauri-pilot.exe"),
    );
    await copyFile(process.execPath, join(directory, "node.exe"));
    await writeFile(join(directory, "portable-test.json"), "{}\n", {
      flag: "wx",
    });
    await writeFile(
      join(directory, "native-persistence-fixture"),
      "isolated native persistence acceptance\n",
      { flag: "wx" },
    );
    await writeFixtureLayout(directory);
    for (const phase of ["write", "read"]) {
      const child = spawn(
        join(directory, "persona-tauri-pilot.exe"),
        ["--persistent-local", `--persistence-${phase}-test`],
        { cwd: directory, stdio: ["ignore", "pipe", "pipe"] },
      );
      cleanupSafe = false;
      let output = "";
      let errors = "";
      child.stdout.on("data", (chunk) => {
        output += chunk;
      });
      child.stderr.on("data", (chunk) => {
        errors += chunk;
      });
      const exited = once(child, "exit");
      const guard = setTimeout(() => {
        void child.kill();
      }, 140_000);
      try {
        const status = await exited;
        if (status[0] !== 0) {
          for (const reason of [
            "acceptance requires a fresh isolated installation fixture",
            "synthetic application record unavailable through native webview",
            "application record changed across executable launches",
            "missing setup",
            "eval timed out",
            "native setup smoke timed out",
            "missing pilot window",
          ])
            if (errors.includes(reason))
              console.error(`native acceptance diagnostic: ${reason}`);
        }
        assert.deepEqual(
          status,
          [0, null],
          "native acceptance failed; fixture retained for inspection",
        );
      } finally {
        clearTimeout(guard);
      }
      // Exit 0 requires the supervisor's graceful service-stop acknowledgement.
      const profile = join(directory, "target/test-data/pilot-local-v1");
      await access(join(profile, "data"));
      await assert.rejects(
        access(join(directory, "target/test-data/pilot-local-v1.lock")),
        { code: "ENOENT" },
      );
      // WebView2 can write platform diagnostics. Never forward raw native output.
      assert.ok(
        JSON.stringify(output.trim().split(/\r?\n/)) ===
          JSON.stringify([
            "pilot: native cookie verified; opening persistent isolated loopback UI",
            `pilot: native persistence acceptance ${phase} verified`,
          ]),
        "unexpected native output (not printed to avoid credential leaks)",
      );
      const record = JSON.parse(
        await readFile(join(directory, "native-synthetic-record.json"), "utf8"),
      );
      assert.deepEqual(Object.keys(record).sort(), ["id", "label", "scope"]);
      cleanupSafe = true;
    }
  } finally {
    // On a timeout, forced exit, or uncertain shutdown, retain the entire fixture.
    if (cleanupSafe) await rm(directory, { recursive: true });
  }
});
