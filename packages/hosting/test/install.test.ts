import {
  mkdir,
  mkdtemp,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { restoreComputers } from "@persona/agent/computer/storage";
import { acquireDataLease } from "@persona/agent/lease";
import { expect, onTestFinished, test } from "vitest";
import {
  prepareInstallation,
  prepareLocalInstallation,
  type Release,
  runCommand,
} from "../src/install.ts";
import { backupInstallation, restoreInstallation } from "../src/maintenance.ts";
import { HostingStore } from "../src/store.ts";

async function fixture() {
  const directory = await mkdtemp(join(tmpdir(), "persona-install-test-"));
  onTestFinished(() => rm(directory, { recursive: true, force: true }));
  return directory;
}
const release: Release = {
  version: "test",
  workImage: "test-image:1",
  controllerEntry: "controller.ts",
  serverEntry: "server.ts",
  bridgeEntry: "bridge.ts",
  staticDir: "web",
};

test("installation commands retain exit diagnostics behind a user-facing error", async () => {
  await expect(
    runCommand(process.execPath, [
      "-e",
      "process.stderr.write('fixture command failed'); process.exitCode = 23",
    ]),
  ).rejects.toMatchObject({
    message:
      "A etapa de instalação não foi concluída. Verifique a rede e o ambiente e tente novamente.",
    cause: {
      command: process.execPath,
      exitCode: 23,
      stderr: "fixture command failed",
    },
  });
});

test("local installation creates a loopback service and preserves existing configuration", async () => {
  const directory = await fixture();
  const first = await prepareLocalInstallation(directory, release);
  expect(first.config).toMatchObject({
    host: "127.0.0.1",
    computer: { kind: "process" },
    dataDir: join(directory, "data"),
  });
  expect(first.config.port).toBeGreaterThan(0);
  const config = { ...first.config, port: 34567 };
  await writeFile(first.configFile, JSON.stringify(config));
  expect((await prepareLocalInstallation(directory, release)).config).toEqual(
    config,
  );
});

test("installation checks Linux mode, preserves configuration and retries image setup", async () => {
  const directory = await fixture();
  const installation = join(directory, "host");
  const phases: string[] = [];
  let missingImage = true;
  const executed: string[][] = [];
  const run = async (_command: string, args: string[]) => {
    executed.push(args);
    if (args[0] === "info") return JSON.stringify("linux");
    if (args[0] === "image" && missingImage) throw new Error("Missing image");
    if (args[0] === "pull") missingImage = false;
    return "";
  };
  const first = await prepareInstallation(
    installation,
    release,
    (value) => phases.push(value.phase),
    undefined,
    run,
  );
  const token = await readFile(join(installation, "controller-token"), "utf8");
  const config = {
    ...(first.config as object),
    model: { provider: "test", modelId: "a" },
  };
  await writeFile(first.configFile, JSON.stringify(config));
  const second = await prepareInstallation(
    installation,
    release,
    () => {},
    undefined,
    run,
  );
  expect(second.config).toEqual(config);
  expect(await readFile(join(installation, "controller-token"), "utf8")).toBe(
    token,
  );
  expect(executed.some((args) => args[0] === "pull")).toBe(true);
  expect(phases).toEqual(["checking", "image", "configuration"]);
  await expect(
    prepareInstallation(
      join(directory, "windows"),
      release,
      () => {},
      undefined,
      async () => JSON.stringify("windows"),
    ),
  ).rejects.toThrow("Linux");
});

test("encrypted backups require stopped writers and restore credentials and files to a new location", async () => {
  const directory = await fixture();
  const root = join(directory, "host");
  const data = join(root, "data");
  await mkdir(data, { recursive: true });
  await writeFile(
    join(root, "persona.config.json"),
    JSON.stringify({ dataDir: data, computer: { kind: "process" } }),
  );
  const secret = crypto.randomUUID();
  await writeFile(join(data, "artifact.txt"), secret);
  const archive = join(directory, "backup.persona");
  const password = crypto.randomUUID();
  const release = await acquireDataLease(data);
  await expect(backupInstallation(root, archive, password)).rejects.toThrow(
    "运行实例",
  );
  await release();
  await backupInstallation(root, archive, password);
  expect((await readFile(archive)).includes(Buffer.from(secret))).toBe(false);
  await expect(
    restoreInstallation(archive, join(directory, "wrong"), "wrong-password"),
  ).rejects.toThrow();
  const target = join(directory, "restored");
  await restoreInstallation(archive, target, password);
  expect(await readFile(join(target, "data/artifact.txt"), "utf8")).toBe(
    secret,
  );
  expect(
    JSON.parse(await readFile(join(target, "persona.config.json"), "utf8"))
      .dataDir,
  ).toBe(join(target, "data"));
  await expect(restoreInstallation(archive, target, password)).rejects.toThrow(
    "新的",
  );
});

async function externalFixture() {
  const directory = await fixture();
  const root = join(directory, "host");
  const data = join(directory, "external");
  await mkdir(root);
  await mkdir(data);
  const config = join(root, "persona.config.json");
  await writeFile(
    config,
    JSON.stringify({ dataDir: data, computer: { kind: "process" } }),
  );
  return {
    directory,
    root,
    data,
    config,
    archive: join(directory, "backup.persona"),
    password: crypto.randomUUID(),
  };
}

test.each([false, true])(
  "external data backup normalizes only the snapshot and retains the hosting encryption key (omitted defaults: %s)",
  async (omitDefaults) => {
    const f = await externalFixture();
    if (omitDefaults)
      await writeFile(f.config, JSON.stringify({ dataDir: f.data }));
    const store = await HostingStore.open(f.data);
    const encrypted = store.encrypt({ token: "synthetic-token" }, "fixture");
    store.db
      .prepare("INSERT INTO credentials VALUES (?, ?, ?)")
      .run("fixture", "test", encrypted);
    await store.close();
    const key = await readFile(join(f.data, "credentials.key"));
    const original = await readFile(f.config);
    await writeFile(join(f.data, "artifact.txt"), "synthetic work");
    const release = await acquireDataLease(f.data);
    try {
      await expect(
        backupInstallation(f.root, f.archive, f.password),
      ).rejects.toThrow("运行实例");
    } finally {
      await release();
    }
    await backupInstallation(f.root, f.archive, f.password);
    const target = join(f.directory, "restored");
    await restoreInstallation(f.archive, target, f.password);
    expect(await readFile(f.config)).toEqual(original);
    expect(await readFile(join(f.data, "artifact.txt"), "utf8")).toBe(
      "synthetic work",
    );
    expect(await readFile(join(target, "data", "credentials.key"))).toEqual(
      key,
    );
    const restored = await HostingStore.open(join(target, "data"));
    try {
      const row = restored.db
        .prepare("SELECT value FROM credentials WHERE provider = ?")
        .get("fixture");
      expect(restored.decrypt(row?.value as Uint8Array, "fixture")).toEqual({
        token: "synthetic-token",
      });
    } finally {
      await restored.close();
    }
  },
);

test("external backup rejects missing keys, overlapping paths and linked trees before leasing", async () => {
  const f = await externalFixture();
  await writeFile(join(f.data, "hosting.sqlite"), "synthetic database");
  await expect(
    backupInstallation(f.root, f.archive, f.password),
  ).rejects.toThrow("credentials.key");
  await expect(readFile(join(f.data, "runtime.sqlite"))).rejects.toMatchObject({
    code: "ENOENT",
  });
  await writeFile(join(f.data, "credentials.key"), Buffer.alloc(31));
  await expect(
    backupInstallation(f.root, f.archive, f.password),
  ).rejects.toThrow("密钥");
  await rm(join(f.data, "hosting.sqlite"));
  await expect(
    backupInstallation(f.root, join(f.data, "backup"), f.password),
  ).rejects.toThrow("重叠");
  await symlink(
    f.root,
    join(f.data, "link"),
    process.platform === "win32" ? "junction" : "dir",
  );
  await expect(
    backupInstallation(f.root, f.archive, f.password),
  ).rejects.toThrow("链接");
  await expect(readFile(f.archive)).rejects.toMatchObject({ code: "ENOENT" });
});

test("external backup rejects configured remote computers without creating a source lease", async () => {
  const f = await externalFixture();
  await writeFile(
    f.config,
    JSON.stringify({
      dataDir: f.data,
      computer: {
        kind: "controller",
        id: "fixture",
        endpoint: "http://127.0.0.1:1234",
        credentialFile: "synthetic-token",
        network: "none",
      },
    }),
  );
  await expect(
    backupInstallation(f.root, f.archive, f.password),
  ).rejects.toThrow("本机");
  await expect(readFile(join(f.data, "runtime.sqlite"))).rejects.toMatchObject({
    code: "ENOENT",
  });
  await writeFile(
    f.config,
    JSON.stringify({
      dataDir: f.data,
      computer: {
        kind: "docker",
        id: "fixture",
        image: "fixture:1",
        volume: "fixture",
      },
    }),
  );
  await expect(
    backupInstallation(f.root, f.archive, f.password),
  ).rejects.toThrow("本机");
  await expect(readFile(join(f.data, "runtime.sqlite"))).rejects.toMatchObject({
    code: "ENOENT",
  });
});

test("external backup inspects stored remote computers only on the isolated snapshot", async () => {
  const f = await externalFixture();
  await writeFile(
    join(f.data, "computer.json"),
    JSON.stringify({ kind: "controller", id: "fixture" }),
  );
  await expect(
    backupInstallation(f.root, f.archive, f.password),
  ).rejects.toThrow("本机");
  await rm(join(f.data, "computer.json"));
  const database = join(f.data, "agent.sqlite");
  await restoreComputers(database, [
    {
      id: "fixture",
      name: "fixture",
      authorized: true,
      config: {
        kind: "controller",
        id: "fixture",
        endpoint: "http://127.0.0.1:1234",
        credentialFile: "synthetic-token",
        network: "none",
        timeoutSeconds: 120,
      },
    },
  ]);
  const original = await readFile(database);
  await expect(
    backupInstallation(f.root, f.archive, f.password),
  ).rejects.toThrow("本机");
  expect(await readFile(database)).toEqual(original);
  await expect(readFile(f.archive)).rejects.toMatchObject({ code: "ENOENT" });
});

test("process-only restore preserves a compatible backup and rejects isolated computers before publishing", async () => {
  const directory = await fixture();
  const root = join(directory, "host");
  const data = join(root, "data");
  await mkdir(data, { recursive: true });
  await writeFile(
    join(root, "persona.config.json"),
    JSON.stringify({ dataDir: data, computer: { kind: "process" } }),
  );
  await writeFile(join(data, "artifact.txt"), "synthetic work");
  const archive = join(directory, "backup.persona");
  const password = crypto.randomUUID();
  await backupInstallation(root, archive, password);

  const compatible = join(directory, "compatible");
  await restoreInstallation(archive, compatible, password, {
    requireProcessComputer: true,
  });
  expect(await readFile(join(compatible, "data", "artifact.txt"), "utf8")).toBe(
    "synthetic work",
  );

  await writeFile(
    join(root, "persona.config.json"),
    JSON.stringify({
      dataDir: data,
      computer: { kind: "process" },
      workComputer: {
        kind: "controller",
        id: "fixture",
        endpoint: "https://fixture.example",
        credentialFile: "synthetic-token",
        network: "none",
        timeoutSeconds: 120,
      },
    }),
  );
  const isolatedArchive = join(directory, "isolated.persona");
  await backupInstallation(root, isolatedArchive, password);
  const isolated = join(directory, "isolated");
  await expect(
    restoreInstallation(isolatedArchive, isolated, password, {
      requireProcessComputer: true,
    }),
  ).rejects.toThrow("隔离电脑");
  await expect(
    readFile(join(isolated, "persona.config.json")),
  ).rejects.toMatchObject({ code: "ENOENT" });

  await writeFile(
    join(root, "persona.config.json"),
    JSON.stringify({ dataDir: data, computer: { kind: "process" } }),
  );
  await writeFile(
    join(data, "computer.json"),
    JSON.stringify({ kind: "controller", id: "fixture" }),
  );
  const legacyArchive = join(directory, "legacy.persona");
  await backupInstallation(root, legacyArchive, password);
  const legacy = join(directory, "legacy");
  await expect(
    restoreInstallation(legacyArchive, legacy, password, {
      requireProcessComputer: true,
    }),
  ).rejects.toThrow("隔离电脑");
  await expect(
    readFile(join(legacy, "persona.config.json")),
  ).rejects.toMatchObject({ code: "ENOENT" });
});
