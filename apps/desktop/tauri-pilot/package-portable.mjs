import { spawnSync } from "node:child_process";
import {
  access,
  cp,
  mkdir,
  mkdtemp,
  readFile,
  writeFile,
} from "node:fs/promises";
import { stripTypeScriptTypes } from "node:module";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

if (process.platform !== "win32")
  throw new Error("Portable TEST packaging supports Windows only.");
if (Number(process.versions.node.split(".")[0]) < 24)
  throw new Error(
    "Node >=24 is required; run this script with a compatible installed node.exe.",
  );
const base = fileURLToPath(new URL("./", import.meta.url));
const runtime = fileURLToPath(new URL("../runtime/", import.meta.url));
for (const entry of [
  "server.mjs",
  "bridge.mjs",
  "package.json",
  "web/index.html",
  "node_modules/@persona/agent/src/harness/worker.mjs",
  "node_modules/@persona/hosting/src/instance.ts",
  "node_modules/zod/package.json",
]) {
  await access(join(runtime, entry)).catch(() => {
    throw new Error(
      `Missing production runtime prerequisite: apps/desktop/runtime/${entry}. Supply an existing production desktop runtime and web build; this script never builds or changes runtime/dist.`,
    );
  });
}
// Offline Cargo only: never invoke the Electron build or download a toolchain.
const build = spawnSync("cargo", ["build", "--offline", "--locked"], {
  cwd: base,
  stdio: "inherit",
});
if (build.error || build.status !== 0)
  throw new Error(
    "Pilot offline Cargo build failed. Prepare the installed Rust toolchain and cached pilot dependencies first.",
  );
await mkdir(join(base, "target"), { recursive: true });
const destination = await mkdtemp(join(base, "target", "portable-test-"));
await cp(
  join(base, "target/debug/persona-tauri-pilot.exe"),
  join(destination, "persona-tauri-pilot.exe"),
);
await cp(process.execPath, join(destination, "node.exe"));
// Copy the existing production artifact, dereferencing its workspace links for relocation.
await cp(runtime, join(destination, "runtime"), {
  recursive: true,
  dereference: true,
});
// Node cannot strip TypeScript inside node_modules; compile the artifact's shared policy unchanged.
await writeFile(
  join(destination, "runtime/hosting-instance.mjs"),
  stripTypeScriptTypes(
    await readFile(
      join(runtime, "node_modules/@persona/hosting/src/instance.ts"),
      "utf8",
    ),
  ),
  { flag: "wx" },
);
for (const entry of [
  "layout.mjs",
  "supervisor.mjs",
  "service.mjs",
  "service-child.mjs",
  "service-transport.mjs",
  "stop-child.mjs",
  "local-session.mjs",
  "remote-probe.mjs",
  "remote-probe.test.mjs",
  "remote-server.fixture.mjs",
  "portable-smoke.mjs",
]) {
  await cp(join(base, entry), join(destination, entry));
}
await writeFile(
  join(destination, "portable-test.json"),
  JSON.stringify({
    testOnly: true,
    node: process.versions.node,
    platform: process.platform,
    arch: process.arch,
  }),
  { flag: "wx" },
);
const smoke = spawnSync(
  join(destination, "node.exe"),
  [join(destination, "portable-smoke.mjs")],
  { cwd: destination, stdio: "inherit", timeout: 120_000 },
);
if (smoke.error || smoke.status !== 0)
  throw new Error(
    `Bundled runtime compatibility smoke failed; retain TEST evidence at ${destination}.`,
  );
console.log(
  `Portable TEST package (requires installed WebView2): ${destination}`,
);
