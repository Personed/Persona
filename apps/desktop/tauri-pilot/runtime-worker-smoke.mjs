import assert from "node:assert/strict";
import { cp, mkdtemp, readFile, rm } from "node:fs/promises";
import { join, resolve } from "node:path";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import { WorkProcess } from "../../../packages/agent/src/harness/process.ts";

// No external credentials, account homes or production profiles enter this smoke.
for (const key of Object.keys(process.env))
  if (
    !["PATH", "SYSTEMROOT", "WINDIR", "SYSTEMDRIVE"].includes(key.toUpperCase())
  )
    delete process.env[key];
const artifact = resolve(process.argv[2]);
const directory = await mkdtemp(join(artifact, "runtime/worker-smoke-"));
const worker = new WorkProcess(
  [
    join(artifact, "node.exe"),
    join(
      artifact,
      "runtime/node_modules/@persona/agent/src/harness/worker.mjs",
    ),
  ],
  { cwd: join(directory, "bot"), home: join(directory, "accounts") },
  [],
  async () => ({ content: [] }),
);
worker.error.on("data", (chunk) => process.stderr.write(chunk));
try {
  await worker.ready;
  console.log("packaged worker handshake verified");
  const tools = await worker.computer({
    operation: "catalog",
    service: "desktop",
  });
  assert.ok(tools.some((tool) => tool.name === "get_desktop_state"));
  console.log("desktop catalog verified");
  const browser = await worker.computer({
    operation: "catalog",
    service: "browser",
  });
  assert.ok(browser.length > 0);
  console.log("browser catalog verified");
  const fixture = join(directory, "mcp-stdio.mjs");
  await cp(
    new URL(
      "../../../packages/agent/test/fixtures/mcp-stdio.mjs",
      import.meta.url,
    ),
    fixture,
  );
  const connected = await worker.mcp({
    action: "connect",
    id: "fixture",
    config: {
      transport: "stdio",
      command: join(artifact, "node.exe"),
      args: [fixture],
      env: { MCP_FIXTURE_TEXT: "portable verified" },
    },
  });
  assert.ok(connected.tools.some((tool) => tool.name === "echo"));
  const result = await worker.mcp({
    action: "request",
    id: "fixture",
    method: "tools/call",
    params: { name: "echo" },
  });
  assert.equal(result.result.content[0].text, "portable verified");
  const lines = createInterface({ input: worker.output });
  const completed = new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error("synthetic task timed out")),
      20000,
    );
    lines.on("line", (line) => {
      const frame = JSON.parse(line);
      if (
        frame.type === "extension_ui_request" &&
        frame.message === "pilot synthetic task completed"
      ) {
        clearTimeout(timer);
        resolve();
      }
    });
  });
  const state = new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error("native Pi get_state timed out")),
      20000,
    );
    lines.on("line", (line) => {
      const frame = JSON.parse(line);
      if (frame.id === "smoke-state") {
        clearTimeout(timer);
        resolve(frame);
      }
    });
  });
  await worker.launch({
    command: join(artifact, "node.exe"),
    args: [
      join(
        artifact,
        "runtime/node_modules/@earendil-works/pi-coding-agent/dist/cli.js",
      ),
      "--mode",
      "rpc",
      "--session-dir",
      join(directory, "sessions"),
      "--extension",
      fileURLToPath(new URL("./harness-smoke.fixture.mjs", import.meta.url)),
    ],
    env: { PI_CODING_AGENT_DIR: join(directory, "pi") },
  });
  worker.input.write(
    `${JSON.stringify({ id: "smoke-state", type: "get_state" })}\n`,
  );
  assert.equal((await state).success, true);
  worker.input.write(
    `${JSON.stringify({ type: "prompt", message: "/pilot-smoke" })}\n`,
  );
  await completed;
  assert.equal(
    await readFile(join(directory, "bot/synthetic-task.txt"), "utf8"),
    "isolated native harness completed",
  );
  lines.close();
  console.log(
    "packaged worker handshake, native Pi RPC, fixture MCP echo, desktop/browser catalogs verified",
  );
} finally {
  await worker.close();
  await rm(directory, { recursive: true });
}
