import { spawnSync } from "node:child_process";
import {
  access,
  cp,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { buildPilotBundle } from "./build-pilot-bundle.mjs";
import { buildRuntime } from "./build-runtime.mjs";

if (
  process.platform !== "win32" ||
  Number(process.versions.node.split(".")[0]) < 24
)
  throw new Error("The pilot installer requires Windows and Node >=24.");
const base = fileURLToPath(new URL("./", import.meta.url));
// NSIS cannot open deep dependency paths beneath a long checkout path.
const temporary = join(tmpdir(), "opencode");
await mkdir(temporary, { recursive: true });
const stage = await mkdtemp(join(temporary, "pi-"));
await buildRuntime(join(stage, "runtime"));
for (const entry of [
  "runtime/release.json",
  "runtime/node_modules/@persona/hosting/src/stack.ts",
  "runtime/node_modules/@persona/hosting/src/install.ts",
  "runtime/work/deploy/workspace.Dockerfile",
  "runtime/work/deploy/proxy.Dockerfile",
  "runtime/work/packages/agent/src/harness/worker.mjs",
])
  await access(join(stage, entry));
await cp(process.execPath, join(stage, "node.exe"));
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
])
  await cp(join(base, entry), join(stage, entry));
await buildPilotBundle(
  join(base, "import-backup.mjs"),
  join(stage, "import-backup.mjs"),
);
await buildPilotBundle(
  join(base, "prepare-docker.mjs"),
  join(stage, "prepare-docker.mjs"),
);
await cp(
  join(base, "maintenance-worker.mjs"),
  join(stage, "maintenance-worker.mjs"),
);
await cp(
  join(base, "maintenance-protocol.mjs"),
  join(stage, "maintenance-protocol.mjs"),
);
await buildPilotBundle(
  join(base, "maintenance-operation.mjs"),
  join(stage, "maintenance-operation.mjs"),
);
await writeFile(
  join(stage, "installer.json"),
  JSON.stringify({ pilot: true, node: process.versions.node }),
);
const cli = fileURLToPath(
  new URL("../../../node_modules/@tauri-apps/cli/tauri.js", import.meta.url),
);
const config = JSON.parse(
  await readFile(join(base, "tauri.installer.conf.json"), "utf8"),
);
config.bundle.resources = { [`${stage}/`]: "./" };
await mkdir(join(base, "target"), { recursive: true });
const configPath = join(base, "target", "installer-config.json");
await writeFile(configPath, JSON.stringify(config));
const result = spawnSync(
  process.execPath,
  [
    cli,
    "build",
    "--bundles",
    "nsis",
    "--config",
    configPath,
    "--",
    "--locked",
    "--offline",
  ],
  { cwd: base, stdio: "inherit" },
);
if (result.error || result.status !== 0)
  throw result.error ?? new Error("Pilot installer build failed.");

const executable = join(stage, "persona.exe");
const appData = await mkdtemp(join(base, "target", "installer-smoke-data-"));
await cp(join(base, "target", "release", "persona.exe"), executable);
const env = Object.fromEntries(
  ["PATH", "SystemRoot", "WINDIR", "SystemDrive"]
    .filter((key) => process.env[key])
    .map((key) => [key, process.env[key]]),
);
env.LOCALAPPDATA = appData;
const smoke = spawnSync(executable, ["--smoke-test"], {
  cwd: stage,
  env,
  stdio: "inherit",
  timeout: 120_000,
  windowsHide: true,
});
if (smoke.error || smoke.status !== 0)
  throw (
    smoke.error ??
    new Error(`Installed-resource smoke failed; retain evidence at ${appData}.`)
  );
await rm(executable, { force: true });
await rm(appData, { recursive: true, force: true });
await rm(stage, { recursive: true, force: true });
console.log(
  "Installed-resource lifecycle smoke passed with isolated LocalAppData.",
);
