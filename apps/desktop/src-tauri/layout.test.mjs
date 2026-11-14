import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { cp, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { test } from "node:test";
import { pathToFileURL } from "node:url";

test("installed resources and pilot-owned data remain separate; portable stays adjacent", async () => {
  const root = await mkdtemp(join(tmpdir(), "persona-pilot-layout-"));
  try {
    await cp(
      new URL("./layout.mjs", import.meta.url),
      join(root, "layout.mjs"),
    );
    const inspect = () => {
      const result = spawnSync(
        process.execPath,
        [
          "--input-type=module",
          "-e",
          `import {dataDirectory, portable, runtimeEntry} from ${JSON.stringify(pathToFileURL(join(root, "layout.mjs")).href)}; console.log(JSON.stringify({dataDirectory,portable,runtime:runtimeEntry.href}));`,
        ],
        {
          env: {
            ...process.env,
            LOCALAPPDATA: join(root, "synthetic-appdata"),
          },
          encoding: "utf8",
        },
      );
      assert.equal(result.status, 0, result.stderr);
      return JSON.parse(result.stdout);
    };
    assert.equal(inspect().portable, false);
    await writeFile(join(root, "portable-test.json"), "{}");
    assert.equal(
      resolve(inspect().dataDirectory),
      join(root, "target", "test-data"),
    );
    await rm(join(root, "portable-test.json"));
    await writeFile(join(root, "installer.json"), "{}");
    const installed = inspect();
    assert.equal(
      installed.dataDirectory,
      join(root, "synthetic-appdata", "app.persona.desktop"),
    );
    assert.equal(installed.portable, true);
    assert.equal(
      installed.runtime,
      pathToFileURL(join(root, "runtime", "server.mjs")).href,
    );
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
