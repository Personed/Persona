import assert from "node:assert/strict";
import {
  access,
  link,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { PassThrough, Readable, Writable } from "node:stream";
import { test } from "node:test";
import { validatePilotDockerInstallation } from "./docker-maintenance.mjs";
import {
  newPilotDockerProfilePaths,
  pilotDockerProfileMarker,
  pilotDockerProfilePaths,
  selectPilotDockerProfile,
} from "./docker-profile.mjs";
import { runMaintenanceOperation } from "./maintenance-operation.mjs";
import { runMaintenanceProtocol } from "./maintenance-protocol.mjs";
import {
  newPilotProfilePaths,
  pilotProfileMarker,
  pilotProfilePaths,
  selectPilotProfile,
} from "./service.mjs";

const password = "synthetic-password";

async function dockerProfile(directory) {
  await mkdir(join(directory, "data"), { recursive: true });
  const workComputer = {
    kind: "controller",
    id: "team-0123456789ab",
    endpoint: "http://controller:4311",
    credentialFile: "/control/controller-token",
    network: "none",
    timeoutSeconds: 120,
  };
  const template = {
    kind: "docker",
    id: workComputer.id,
    image: "persona-work:test",
    volume: `persona-${workComputer.id}-work`,
    network: "none",
    cpus: 2,
    memoryMiB: 1024,
    pids: 256,
    timeoutSeconds: 120,
  };
  const stack = { id: "persona-abcdef123456", port: 43210, directory };
  await writeFile(join(directory, "pilot-profile"), pilotDockerProfileMarker);
  await writeFile(
    join(directory, "persona.config.json"),
    JSON.stringify({
      host: "127.0.0.1",
      port: stack.port,
      dataDir: join(directory, "data"),
      deployment: "container",
      computer: { kind: "process" },
      workComputer,
    }),
  );
  await writeFile(
    join(directory, "controller.json"),
    JSON.stringify({
      host: "0.0.0.0",
      port: 4311,
      credentialFile: "controller-token",
      computer: template,
    }),
  );
  await writeFile(join(directory, "stack.json"), JSON.stringify(stack));
  await writeFile(
    join(directory, "service.json"),
    JSON.stringify({
      host: "0.0.0.0",
      port: 4310,
      dataDir: "/data",
      deployment: "container",
      computer: { kind: "process" },
      workComputer,
      publicUrl: `http://127.0.0.1:${stack.port}`,
      staticDir: "/app/apps/web/dist",
      desktopCredentialFile: "/control/desktop-token",
    }),
  );
  await writeFile(
    join(directory, "compose.json"),
    JSON.stringify({
      name: stack.id,
      services: {
        app: { image: "persona-service:test" },
        controller: { image: "persona-service:test" },
      },
      networks: { control: { internal: true } },
    }),
  );
  for (const name of ["controller-token", "desktop-token"])
    await writeFile(join(directory, name), "a".repeat(64));
}

test("Docker profile validation accepts only the pilot container images and layout", async () => {
  const root = await mkdtemp(join(tmpdir(), "pilot-docker-validation-"));
  const directory = join(root, "pilot-docker-v1");
  const release = {
    serviceImage: "persona-service:test",
    workImage: "persona-work:test",
  };
  try {
    await dockerProfile(directory);
    await validatePilotDockerInstallation(directory, { release });
    await assert.rejects(
      validatePilotDockerInstallation(directory, {
        release,
        computers: [
          {
            id: "computer-1",
            config: {
              kind: "controller",
              endpoint: "http://untrusted:4311",
              credentialFile: "/control/controller-token",
              network: "none",
            },
          },
        ],
      }),
      /Incompatible saved Docker computers/,
    );
    const bindingPath = join(directory, "data", "computer.json");
    await writeFile(
      bindingPath,
      JSON.stringify({ kind: "controller", deployment: "container" }),
    );
    await assert.rejects(
      validatePilotDockerInstallation(directory, { release }),
      /Incompatible Docker computer binding/,
    );
    await rm(bindingPath);
    await validatePilotDockerInstallation(directory, {
      release,
      computers: [],
      manifest: { version: 2, computers: [], workComputer: {} },
    });
    const changedId = "abcdef123456";
    const configPath = join(directory, "persona.config.json");
    const controllerPath = join(directory, "controller.json");
    const servicePath = join(directory, "service.json");
    const config = JSON.parse(await readFile(configPath, "utf8"));
    const controller = JSON.parse(await readFile(controllerPath, "utf8"));
    const service = JSON.parse(await readFile(servicePath, "utf8"));
    config.workComputer.id = changedId;
    controller.computer.id = changedId;
    controller.computer.volume = `persona-${changedId}-work`;
    service.workComputer.id = changedId;
    await writeFile(configPath, JSON.stringify(config));
    await writeFile(controllerPath, JSON.stringify(controller));
    await writeFile(servicePath, JSON.stringify(service));
    await validatePilotDockerInstallation(directory, { release });
    await assert.rejects(
      validatePilotDockerInstallation(directory, {
        release,
        computers: [],
        manifest: { version: 1, computers: [], workComputer: {} },
      }),
      /Incompatible Docker backup manifest/,
    );
    await writeFile(
      join(directory, "compose.json"),
      JSON.stringify({
        name: "persona-abcdef123456",
        services: {
          app: { image: "untrusted:test" },
          controller: { image: release.serviceImage },
        },
      }),
    );
    await assert.rejects(
      validatePilotDockerInstallation(directory, { release }),
      /Incompatible Docker images/,
    );
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("Docker maintenance uses its profile namespace and retains the old selection on restore failure", async () => {
  const root = await mkdtemp(join(tmpdir(), "pilot-docker-maintenance-"));
  try {
    const paths = await pilotDockerProfilePaths(join(root, "profiles"));
    await dockerProfile(paths.directory);
    const archive = join(root, "synthetic.persona");
    let snapshot;
    await runMaintenanceOperation(
      { action: "backup", archive, password, runtime: "docker" },
      {
        profilePaths: async () => paths,
        backup: async (directory) => {
          snapshot = directory;
          assert.equal(
            JSON.parse(
              await readFile(join(directory, "persona.config.json"), "utf8"),
            ).dataDir,
            "data",
          );
        },
      },
    );
    assert.ok(snapshot);
    await writeFile(archive, "synthetic encrypted archive");
    const oldPointer = join(paths.base, "pilot-docker-current.json");
    await selectPilotDockerProfile(paths.directory, paths.base);
    const selectedBefore = await readFile(oldPointer, "utf8");
    await assert.rejects(
      readFile(join(paths.base, "pilot-local-current.json")),
    );

    let destination;
    await assert.rejects(
      runMaintenanceOperation(
        { action: "restore", archive, password, runtime: "docker" },
        {
          profilePaths: async () => paths,
          newProfilePaths: async () => {
            const value = await newPilotDockerProfilePaths(paths.base);
            destination = value.directory;
            return value;
          },
          restore: async () => {
            throw new Error("synthetic incompatible backup");
          },
        },
      ),
      /synthetic incompatible backup/,
    );
    assert.equal(await readFile(oldPointer, "utf8"), selectedBefore);
    assert.ok(destination?.startsWith(paths.base));

    let selectedDestination;
    await runMaintenanceOperation(
      { action: "restore", archive, password, runtime: "docker" },
      {
        profilePaths: async () => paths,
        newProfilePaths: async () => {
          const value = await newPilotDockerProfilePaths(paths.base);
          selectedDestination = value.directory;
          return value;
        },
        restore: async (_archive, _password, target) => {
          await mkdir(target);
          await writeFile(
            join(target, "pilot-profile"),
            pilotDockerProfileMarker,
          );
          return target;
        },
      },
    );
    const pointer = JSON.parse(await readFile(oldPointer, "utf8"));
    assert.equal(pointer.profile, selectedDestination.split(/[\\/]/).at(-1));
    assert.equal(
      await readFile(join(paths.directory, "pilot-profile"), "utf8"),
      pilotDockerProfileMarker,
    );
    await assert.rejects(
      readFile(join(paths.base, "pilot-local-current.json")),
    );
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("ready precedes start and operation loading; errors never echo secrets", async () => {
  const input = new PassThrough();
  const frames = [];
  let loaded = false;
  const output = new Writable({
    write(chunk, _encoding, done) {
      frames.push(JSON.parse(chunk));
      done();
    },
  });
  const pending = runMaintenanceProtocol({
    input,
    output,
    loadOperation: async () => {
      loaded = true;
      return async () => {
        throw new Error(password);
      };
    },
  });
  assert.deepEqual(frames, [{ type: "ready" }]);
  assert.equal(loaded, false);
  input.write(
    `${JSON.stringify({ type: "start", action: "backup", archive: "/synthetic", password })}\n`,
  );
  assert.equal(await pending, 1);
  assert.equal(loaded, true);
  assert.equal(JSON.stringify(frames).includes(password), false);
  input.destroy();
});

for (const value of [
  "",
  "not-json\n",
  `${"x".repeat(65537)}\n`,
  '{"type":"cancel"}\n',
]) {
  test(`reject malformed input (${value.length} bytes) without loading operation`, async () => {
    assert.equal(
      await runMaintenanceProtocol({
        input: Readable.from([value]),
        output: new Writable({
          write(_chunk, _encoding, done) {
            done();
          },
        }),
        loadOperation: () => {
          assert.fail("must not load");
        },
      }),
      1,
    );
  });
}

test("maintenance protocol rejects unknown runtime values before loading", async () => {
  const frames = [];
  assert.equal(
    await runMaintenanceProtocol({
      input: Readable.from([
        `${JSON.stringify({
          type: "start",
          action: "restore",
          archive: "/synthetic.persona",
          password,
          runtime: "remote",
        })}\n`,
      ]),
      output: new Writable({
        write(chunk, _encoding, done) {
          frames.push(JSON.parse(chunk));
          done();
        },
      }),
      loadOperation: () => assert.fail("must not load"),
    }),
    1,
  );
  assert.equal(JSON.stringify(frames).includes(password), false);
});

test("synthetic backup and restore roundtrip preserves source and refuses overwrite", async () => {
  const root = await mkdtemp(join(tmpdir(), "pilot-maintenance-"));
  try {
    const directory = join(root, "pilot-local-v1");
    const lock = `${directory}.lock`;
    const profilePaths = async () => ({ base: root, directory, lock });
    await mkdir(join(directory, "data"), { recursive: true });
    await writeFile(join(directory, "pilot-profile"), pilotProfileMarker);
    await writeFile(
      join(directory, "config.json"),
      JSON.stringify({ dataDir: "data", computer: { kind: "process" } }),
    );
    await writeFile(join(directory, "data", "synthetic.txt"), "synthetic only");
    const archive = join(root, "synthetic.persona");
    const request = { action: "backup", archive, password };
    await mkdir(lock);
    await assert.rejects(runMaintenanceOperation(request, { profilePaths }));
    await rm(lock, { recursive: true });
    await writeFile(join(directory, "pilot-profile"), "not pilot");
    await assert.rejects(runMaintenanceOperation(request, { profilePaths }));
    await assert.rejects(access(archive));
    await writeFile(join(directory, "pilot-profile"), pilotProfileMarker);
    await runMaintenanceOperation(request, { profilePaths });
    await assert.rejects(runMaintenanceOperation(request, { profilePaths }));
    const destination = join(root, "restored");
    const newProfilePaths = async () => ({
      base: root,
      directory: destination,
      lock: `${destination}.lock`,
    });
    let selected;
    const selectProfile = async (profile, base) => {
      selected = { profile, base };
    };
    request.action = "restore";
    await assert.rejects(
      runMaintenanceOperation(
        { ...request, password: "wrong-password" },
        { profilePaths, newProfilePaths, selectProfile },
      ),
    );
    await assert.rejects(access(destination));
    assert.equal(selected, undefined);
    await runMaintenanceOperation(request, {
      profilePaths,
      newProfilePaths,
      selectProfile,
    });
    assert.deepEqual(selected, { profile: destination, base: root });
    assert.equal(
      await readFile(join(destination, "pilot-profile"), "utf8"),
      pilotProfileMarker,
    );
    assert.equal(
      await readFile(join(destination, "data", "synthetic.txt"), "utf8"),
      "synthetic only",
    );
    assert.equal(
      await readFile(join(directory, "data", "synthetic.txt"), "utf8"),
      "synthetic only",
    );
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("restore switches to a new profile without replacing the current one", async () => {
  const root = await mkdtemp(join(tmpdir(), "pilot-profile-selection-"));
  try {
    const current = await pilotProfilePaths(root);
    const restored = await newPilotProfilePaths(root);
    await mkdir(current.directory, { recursive: true });
    await writeFile(
      join(current.directory, "pilot-profile"),
      pilotProfileMarker,
    );
    await mkdir(restored.directory, { recursive: true });
    await writeFile(
      join(restored.directory, "pilot-profile"),
      pilotProfileMarker,
    );

    await selectPilotProfile(restored.directory, root);

    assert.equal((await pilotProfilePaths(root)).directory, restored.directory);
    assert.equal(
      await readFile(join(current.directory, "pilot-profile"), "utf8"),
      pilotProfileMarker,
    );
    const replacement = await newPilotProfilePaths(root);
    await mkdir(replacement.directory, { recursive: true });
    await writeFile(
      join(replacement.directory, "pilot-profile"),
      pilotProfileMarker,
    );
    await selectPilotProfile(replacement.directory, root);
    assert.equal(
      (await pilotProfilePaths(root)).directory,
      replacement.directory,
    );
    assert.equal(
      await readFile(join(restored.directory, "pilot-profile"), "utf8"),
      pilotProfileMarker,
    );
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("selected profile pointer rejects hard links", async () => {
  const root = await mkdtemp(join(tmpdir(), "pilot-pointer-hardlink-"));
  try {
    const source = join(root, "selection-source");
    const pointer = join(root, "pilot-local-current.json");
    await writeFile(source, JSON.stringify({ profile: "pilot-local-v1" }));
    await link(source, pointer);

    await assert.rejects(pilotProfilePaths(root), /hard link/i);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("restore rejects a foreign marker before publication", async () => {
  const root = await mkdtemp(join(tmpdir(), "pilot-marker-"));
  try {
    const { backupInstallation } = await import("./backup-maintenance.mjs");
    const { importPilotBackup } = await import("./import-backup.mjs");
    const source = join(root, "source");
    await mkdir(join(source, "data"), { recursive: true });
    await writeFile(
      join(source, "persona.config.json"),
      JSON.stringify({ dataDir: "data", computer: { kind: "process" } }),
    );
    await writeFile(join(source, "pilot-profile"), "foreign-marker");
    const archive = join(root, "backup.persona");
    await backupInstallation(source, archive, password);
    const destination = join(root, "destination");
    await assert.rejects(importPilotBackup(archive, password, destination));
    await assert.rejects(access(destination));
    await assert.rejects(access(`${destination}.lock`));
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("snapshot preserves template and credentials, rejects external data and cleans up failures", async () => {
  const root = await mkdtemp(join(tmpdir(), "pilot-snapshot-"));
  let snapshot;
  try {
    const directory = join(root, "pilot");
    await mkdir(join(directory, "data"), { recursive: true });
    await writeFile(join(directory, "pilot-profile"), pilotProfileMarker);
    await writeFile(
      join(directory, "data", "credential"),
      "synthetic-credential",
    );
    const configuration = {
      dataDir: join(directory, "data"),
      computer: { kind: "process" },
      workComputer: {
        kind: "docker",
        id: "synthetic",
        image: "synthetic:1",
        volume: "synthetic-volume",
      },
    };
    const config = join(directory, "persona.config.json");
    await writeFile(config, JSON.stringify(configuration));
    const profilePaths = async () => ({ directory, lock: `${directory}.lock` });
    const request = {
      action: "backup",
      archive: join(root, "backup.persona"),
      password,
    };
    await assert.rejects(
      runMaintenanceOperation(request, {
        profilePaths,
        backup: async (source) => {
          snapshot = source;
          const copied = JSON.parse(
            await readFile(join(source, "persona.config.json"), "utf8"),
          );
          assert.deepEqual(copied.workComputer, configuration.workComputer);
          assert.equal(copied.dataDir, "data");
          assert.equal(
            await readFile(join(source, "data", "credential"), "utf8"),
            "synthetic-credential",
          );
          throw new Error("synthetic failure");
        },
      }),
      /synthetic failure/,
    );
    await assert.rejects(access(snapshot));
    assert.deepEqual(JSON.parse(await readFile(config, "utf8")), configuration);
    await writeFile(
      config,
      JSON.stringify({ ...configuration, dataDir: join(root, "external") }),
    );
    await assert.rejects(
      runMaintenanceOperation(request, {
        profilePaths,
        backup: async () => assert.fail("must not back up external data"),
      }),
      /External profile data/,
    );
    await assert.rejects(access(request.archive));
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("backup refuses hard-linked profile files before copying plaintext", async () => {
  const root = await mkdtemp(join(tmpdir(), "pilot-hardlink-"));
  try {
    const directory = join(root, "pilot");
    await mkdir(join(directory, "data"), { recursive: true });
    await writeFile(join(directory, "pilot-profile"), pilotProfileMarker);
    await writeFile(
      join(directory, "persona.config.json"),
      JSON.stringify({ dataDir: "data", computer: { kind: "process" } }),
    );
    const outside = join(root, "external-secret");
    await writeFile(outside, "synthetic secret");
    await link(outside, join(directory, "data", "linked-secret"));
    const archive = join(root, "backup.persona");

    await assert.rejects(
      runMaintenanceOperation(
        { action: "backup", archive, password },
        {
          profilePaths: async () => ({
            base: root,
            directory,
            lock: `${directory}.lock`,
          }),
          backup: async () => assert.fail("must not snapshot linked files"),
        },
      ),
      /symbolic link|hard link/i,
    );
    await assert.rejects(access(archive));
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
