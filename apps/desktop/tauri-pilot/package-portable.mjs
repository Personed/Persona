import { spawnSync } from "node:child_process";
import { access, cp, mkdir, mkdtemp, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { buildPilotBundle } from "./build-pilot-bundle.mjs";
import { buildRuntime } from "./build-runtime.mjs";

if (process.platform !== "win32")
  throw new Error("Portable TEST packaging supports Windows only.");
if (Number(process.versions.node.split(".")[0]) < 24)
  throw new Error(
    "Node >=24 is required; run this script with a compatible installed node.exe.",
  );
const base = fileURLToPath(new URL("./", import.meta.url));
await mkdir(join(base, "target"), { recursive: true });
const destination = await mkdtemp(join(base, "target", "portable-test-"));
const runtime = join(destination, "runtime");
await buildRuntime(runtime);
for (const entry of [
  "server.mjs",
  "bridge.mjs",
  "package.json",
  "web/index.html",
  "node_modules/@persona/agent/src/harness/worker.mjs",
  "node_modules/@persona/hosting/src/instance.ts",
  "node_modules/@persona/hosting/src/stack.ts",
  "node_modules/@persona/hosting/src/install.ts",
  "node_modules/@persona/hosting/src/maintenance.ts",
  "node_modules/zod/package.json",
  "release.json",
  "work/deploy/workspace.Dockerfile",
  "work/deploy/proxy.Dockerfile",
  "work/packages/agent/src/harness/worker.mjs",
]) {
  await access(join(runtime, entry)).catch(() => {
    throw new Error(`Fresh pilot runtime is missing ${entry}.`);
  });
}
// Cargo verifies current sources and regenerates only the pilot's own outputs.
const nativeBuild = spawnSync(
  "cargo",
  ["build", "--offline", "--locked", "--target-dir", join(base, "target")],
  { cwd: base, stdio: "inherit", windowsHide: true },
);
if (nativeBuild.error || nativeBuild.status !== 0)
  throw new Error(
    "Current pilot shell offline build failed; refusing to package an existing executable.",
  );
await cp(
  join(base, "target/debug/persona-tauri-pilot.exe"),
  join(destination, "persona-tauri-pilot.exe"),
);
await cp(process.execPath, join(destination, "node.exe"));
for (const entry of [
  "layout.mjs",
  "supervisor.mjs",
  "docker-profile.mjs",
  "docker-service.mjs",
  "service.mjs",
  "service-child.mjs",
  "service-transport.mjs",
  "stop-child.mjs",
  "local-session.mjs",
  "remote-probe.mjs",
  "remote-probe.test.mjs",
  "remote-server.fixture.mjs",
  "portable-smoke.mjs",
  "portable-chat-smoke.mjs",
  "chat-acp.fixture.mjs",
]) {
  await cp(join(base, entry), join(destination, entry));
}
for (const entry of ["import-backup.mjs", "portable-import-smoke.mjs"])
  await buildPilotBundle(join(base, entry), join(destination, entry));
await buildPilotBundle(
  join(base, "prepare-docker.mjs"),
  join(destination, "prepare-docker.mjs"),
);
await cp(
  join(base, "maintenance-worker.mjs"),
  join(destination, "maintenance-worker.mjs"),
);
await cp(
  join(base, "maintenance-protocol.mjs"),
  join(destination, "maintenance-protocol.mjs"),
);
await buildPilotBundle(
  join(base, "maintenance-operation.mjs"),
  join(destination, "maintenance-operation.mjs"),
);
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
const workerSmoke = spawnSync(
  process.execPath,
  [join(base, "runtime-worker-smoke.mjs"), destination],
  { cwd: base, stdio: "inherit", timeout: 120_000 },
);
if (workerSmoke.error || workerSmoke.status !== 0)
  throw new Error(
    `Packaged worker/harness/computer smoke failed; retain TEST evidence at ${destination}.`,
  );
console.log(
  `Portable TEST package (requires installed WebView2): ${destination}`,
);
