import { spawnSync } from "node:child_process";
import { access, cp, mkdir, readFile, writeFile } from "node:fs/promises";
import { createRequire, stripTypeScriptTypes } from "node:module";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const root = fileURLToPath(new URL("../../../", import.meta.url));
const require = createRequire(join(root, "apps/desktop/package.json"));
const { build } = require("esbuild");
const { build: buildWeb } = await import(
  pathToFileURL(
    createRequire(join(root, "apps/web/package.json")).resolve("vite"),
  ).href
);

export async function buildRuntime(runtime) {
  const located = spawnSync("where.exe", ["pnpm.cmd"], {
    encoding: "utf8",
    windowsHide: true,
  });
  if (located.error || located.status !== 0)
    throw new Error("Installed pnpm.cmd was not found");
  const pnpm = join(
    dirname(located.stdout.trim().split(/\r?\n/)[0]),
    "node_modules/pnpm/bin/pnpm.cjs",
  );
  await access(pnpm);
  const deployed = spawnSync(
    process.execPath,
    [
      pnpm,
      "--config.node-linker=hoisted",
      "--filter",
      "@persona/api",
      "deploy",
      "--offline",
      "--legacy",
      "--prod",
      runtime,
    ],
    {
      cwd: root,
      stdio: "inherit",
      windowsHide: true,
    },
  );
  if (deployed.error || deployed.status !== 0)
    throw new Error("Offline pilot dependency deployment failed");
  await buildWeb({
    root: join(root, "apps/web"),
    configFile: join(root, "apps/web/vite.config.ts"),
    cacheDir: join(runtime, "vite-cache"),
    build: { outDir: join(runtime, "web"), emptyOutDir: false },
  });
  for (const [entry, output] of [
    ["apps/api/src/main.ts", "server.mjs"],
    ["apps/controller/src/main.ts", "controller.mjs"],
    ["packages/agent/src/computer/bridge.ts", "bridge.mjs"],
  ])
    await build({
      entryPoints: [join(root, entry)],
      outfile: join(runtime, output),
      bundle: true,
      platform: "node",
      format: "esm",
      target: "node24",
      external: [
        "@earendil-works/*",
        "@hono/*",
        "@orpc/*",
        "hono",
        "hono/*",
        "undici",
        "zod",
      ],
      banner: {
        js: 'import { createRequire as createDesktopRequire } from "node:module"; const require = createDesktopRequire(import.meta.url);',
      },
    });
  await writeFile(
    join(runtime, "hosting-instance.mjs"),
    stripTypeScriptTypes(
      await readFile(join(root, "packages/hosting/src/instance.ts"), "utf8"),
    ),
    { flag: "wx" },
  );
  for (const name of ["NOTICE", "licenses"])
    await cp(join(root, "deploy", name), join(runtime, name), {
      recursive: true,
    });
  const release = JSON.parse(
    await readFile(join(root, "deploy/release.json"), "utf8"),
  );
  await mkdir(join(runtime, "work/deploy"), { recursive: true });
  await mkdir(join(runtime, "work/packages/agent/src/harness"), {
    recursive: true,
  });
  for (const name of [
    "worker.mjs",
    "account-environment.mjs",
    "pi-auth.mjs",
    "pi-tools.mjs",
    "http-service.mjs",
    "native-history.mjs",
    "native-mcp.mjs",
    "mcp-client.mjs",
    "gateway.mjs",
    "tool-results.mjs",
    "browser-service.mjs",
    "browser-tools.mjs",
    "browser-command.mjs",
    "browser-view.mjs",
    "desktop-view.mjs",
    "computer-service.mjs",
    "desktop-tools.mjs",
    "desktop-driver.mjs",
    "cursor-theme.mjs",
    "cursor.lottie",
  ])
    await cp(
      join(root, "packages/agent/src/harness", name),
      join(runtime, "work/packages/agent/src/harness", name),
    );
  for (const name of [
    "workspace.Dockerfile",
    "workspace.mjs",
    "proxy.Dockerfile",
    "squid.conf",
  ])
    await cp(join(root, "deploy", name), join(runtime, "work/deploy", name));
  await cp(
    join(root, "deploy/licenses"),
    join(runtime, "work/deploy/licenses"),
    {
      recursive: true,
    },
  );
  await writeFile(
    join(runtime, "release.json"),
    JSON.stringify({
      version: release.version,
      serviceImage: release.serviceImage,
      workImage: release.workImage,
      proxyImage: release.proxyImage,
      proxyBuild: {
        context: "work",
        dockerfile: "deploy/proxy.Dockerfile",
      },
      controllerEntry: "controller.mjs",
      serverEntry: "server.mjs",
      bridgeEntry: "bridge.mjs",
      workerEntry: "node_modules/@persona/agent/src/harness/worker.mjs",
      staticDir: "web",
      workBuild: {
        context: "work",
        dockerfile: "deploy/workspace.Dockerfile",
      },
    }),
  );
  const driver = join(
    runtime,
    "node_modules/@persona/agent/src/harness/cua-driver",
  );
  await mkdir(driver, { recursive: true });
  // Only use already installed native binaries; never trigger desktopBinary's download fallback.
  const version = JSON.parse(
    await readFile(
      join(root, "packages/agent/node_modules/@trycua/cua-driver/package.json"),
      "utf8",
    ),
  ).version;
  for (const name of ["cua-driver.exe", "cua-cursor-theme.exe"]) {
    const source = join(
      homedir(),
      ".persona/runtimes/cua-driver",
      version,
      name,
    );
    await access(source);
    await cp(source, join(driver, name));
  }
}
