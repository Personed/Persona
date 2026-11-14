import { randomUUID } from "node:crypto";
import { lstat, mkdir, readFile, rm, rmdir, writeFile } from "node:fs/promises";
import { isAbsolute, join, resolve } from "node:path";
import {
  backupInstallation as createBackup,
  restoreInstallation,
} from "./backup-maintenance.mjs";
import {
  assertNoDockerLinks,
  newPilotDockerProfilePaths,
  pilotDockerProfileMarker,
} from "./docker-profile.mjs";
import { releaseEntry } from "./layout.mjs";

async function loadRelease() {
  const value = JSON.parse(await readFile(releaseEntry, "utf8"));
  if (typeof value.workImage !== "string" || !value.workImage)
    throw new Error("Docker release unavailable");
  return value;
}

function exactKeys(value, expected) {
  return (
    value !== null &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    Object.keys(value).sort().join(",") === [...expected].sort().join(",")
  );
}

export async function validatePilotDockerInstallation(
  directory,
  {
    expectedDataDirectory = join(directory, "data"),
    release,
    computers,
    manifest,
  } = {},
) {
  release ??= await loadRelease();
  await assertNoDockerLinks(directory, true);
  if (
    (await readFile(join(directory, "pilot-profile"), "utf8")) !==
    pilotDockerProfileMarker
  )
    throw new Error("Not an isolated Docker profile");

  const filename = join(directory, "persona.config.json");
  const config = JSON.parse(await readFile(filename, "utf8"));
  const dataDirectory = isAbsolute(config.dataDir)
    ? resolve(config.dataDir)
    : resolve(directory, config.dataDir);
  const network = release.proxyImage ? "web" : "none";
  if (
    typeof release.serviceImage !== "string" ||
    !release.serviceImage ||
    typeof release.workImage !== "string" ||
    !release.workImage ||
    !exactKeys(config, [
      "host",
      "port",
      "dataDir",
      "deployment",
      "computer",
      "workComputer",
    ]) ||
    config.host !== "127.0.0.1" ||
    !Number.isInteger(config.port) ||
    config.port < 1 ||
    config.port > 65535 ||
    config.deployment !== "container" ||
    dataDirectory !== resolve(expectedDataDirectory) ||
    !exactKeys(config.computer, ["kind"]) ||
    config.computer.kind !== "process" ||
    !exactKeys(config.workComputer, [
      "kind",
      "id",
      "endpoint",
      "credentialFile",
      "network",
      "timeoutSeconds",
    ]) ||
    config.workComputer.kind !== "controller" ||
    config.workComputer.endpoint !== "http://controller:4311" ||
    config.workComputer.credentialFile !== "/control/controller-token" ||
    config.workComputer.network !== network ||
    !Number.isInteger(config.workComputer.timeoutSeconds) ||
    config.workComputer.timeoutSeconds < 1
  )
    throw new Error("Incompatible Docker pilot configuration");

  try {
    const binding = JSON.parse(
      await readFile(join(directory, "data", "computer.json"), "utf8"),
    );
    if (
      !exactKeys(binding, ["kind", "deployment"]) ||
      binding.kind !== "process" ||
      binding.deployment !== "container"
    )
      throw new Error("Incompatible Docker computer binding");
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }

  const controller = JSON.parse(
    await readFile(join(directory, "controller.json"), "utf8"),
  );
  const template = controller.computer;
  const expectedTemplateKeys = [
    "kind",
    "id",
    "image",
    "volume",
    "network",
    "cpus",
    "memoryMiB",
    "pids",
    "timeoutSeconds",
    ...(release.proxyImage ? ["proxyImage"] : []),
  ];
  if (
    !exactKeys(controller, ["host", "port", "credentialFile", "computer"]) ||
    controller.host !== "0.0.0.0" ||
    controller.port !== 4311 ||
    controller.credentialFile !== "controller-token" ||
    !exactKeys(template, expectedTemplateKeys) ||
    template.kind !== "docker" ||
    !/^(?:team-)?[a-f0-9]{12}$/.test(template.id) ||
    template.image !== release.workImage ||
    template.volume !== `persona-${template.id}-work` ||
    template.network !== network ||
    typeof template.cpus !== "number" ||
    template.cpus <= 0 ||
    !Number.isInteger(template.memoryMiB) ||
    template.memoryMiB < 128 ||
    !Number.isInteger(template.pids) ||
    template.pids < 16 ||
    !Number.isInteger(template.timeoutSeconds) ||
    template.timeoutSeconds < 1 ||
    (release.proxyImage && template.proxyImage !== release.proxyImage) ||
    config.workComputer.id !== template.id
  )
    throw new Error("Incompatible Docker work-computer template");

  for (const name of ["controller-token", "desktop-token"]) {
    if (
      !/^[a-f0-9]{64}$/.test(
        (await readFile(join(directory, name), "utf8")).trim(),
      )
    )
      throw new Error("Invalid Docker profile credential");
  }

  const stack = JSON.parse(
    await readFile(join(directory, "stack.json"), "utf8"),
  );
  if (
    !exactKeys(stack, ["id", "port", "directory"]) ||
    !/^persona-[a-f0-9]{12}$/.test(stack.id) ||
    !Number.isInteger(stack.port) ||
    stack.port < 1 ||
    stack.port > 65535 ||
    stack.port !== config.port ||
    typeof stack.directory !== "string" ||
    !isAbsolute(stack.directory)
  )
    throw new Error("Invalid Docker profile stack metadata");

  const service = JSON.parse(
    await readFile(join(directory, "service.json"), "utf8"),
  );
  if (
    !exactKeys(service, [
      "host",
      "port",
      "dataDir",
      "deployment",
      "computer",
      "workComputer",
      "publicUrl",
      "staticDir",
      "desktopCredentialFile",
    ]) ||
    service.host !== "0.0.0.0" ||
    service.port !== 4310 ||
    service.publicUrl !== `http://127.0.0.1:${stack.port}` ||
    service.dataDir !== "/data" ||
    service.deployment !== "container" ||
    service.staticDir !== "/app/apps/web/dist" ||
    service.desktopCredentialFile !== "/control/desktop-token" ||
    !exactKeys(service.computer, ["kind"]) ||
    service.computer.kind !== "process" ||
    !exactKeys(service.workComputer, [
      "kind",
      "id",
      "endpoint",
      "credentialFile",
      "network",
      "timeoutSeconds",
    ]) ||
    service.workComputer.kind !== "controller" ||
    service.workComputer.id !== config.workComputer.id ||
    service.workComputer.endpoint !== config.workComputer.endpoint ||
    service.workComputer.credentialFile !==
      config.workComputer.credentialFile ||
    service.workComputer.network !== config.workComputer.network ||
    service.workComputer.timeoutSeconds !== config.workComputer.timeoutSeconds
  )
    throw new Error("Incompatible Docker service configuration");

  const compose = JSON.parse(
    await readFile(join(directory, "compose.json"), "utf8"),
  );
  if (
    !exactKeys(compose, ["name", "services", "networks"]) ||
    compose.name !== stack.id ||
    !exactKeys(compose.services, ["app", "controller"]) ||
    compose.services.app?.image !== release.serviceImage ||
    compose.services.controller?.image !== release.serviceImage ||
    !exactKeys(compose.networks, ["control"]) ||
    !exactKeys(compose.networks.control, ["internal"]) ||
    compose.networks.control.internal !== true
  )
    throw new Error("Incompatible Docker images");

  if (computers) {
    if (!Array.isArray(computers))
      throw new Error("Incompatible saved Docker computers");
    const controllers = computers.filter(
      ({ config: computer }) => computer.kind !== "process",
    );
    if (
      controllers.some(
        ({ config: computer }) =>
          computer.kind !== "controller" ||
          computer.endpoint !== config.workComputer.endpoint ||
          computer.credentialFile !== config.workComputer.credentialFile ||
          computer.network !== network,
      )
    )
      throw new Error("Incompatible saved Docker computers");
  }

  if (computers && manifest !== undefined) {
    if (
      manifest.version !== 2 ||
      !Array.isArray(manifest.computers) ||
      !manifest.workComputer ||
      typeof manifest.workComputer !== "object" ||
      Array.isArray(manifest.workComputer)
    )
      throw new Error("Incompatible Docker backup manifest");
    const saved = computers
      .filter(({ config: computer }) => computer.kind !== "process")
      .map(({ id }) => id)
      .sort();
    const archived = manifest.computers
      .map((entry) => {
        if (
          !exactKeys(entry, ["id", "archive"]) ||
          typeof entry.id !== "string" ||
          typeof entry.archive !== "string"
        )
          throw new Error("Incompatible Docker backup manifest");
        return entry.id;
      })
      .sort();
    if (
      saved.join("\0") !== archived.join("\0") ||
      (manifest.workComputer.archive !== undefined &&
        manifest.workComputer.archive !== "work-computer.tar")
    )
      throw new Error("Incompatible Docker backup manifest");
  }
}

export async function backupPilotDockerInstallation(
  directory,
  archive,
  password,
) {
  return createBackup(directory, archive, password, {
    includeControllerTemplate: true,
    validateComputers: async (_config, computers) =>
      validatePilotDockerInstallation(directory, { computers }),
  });
}

export async function importPilotDockerBackup(
  archive,
  password,
  destination,
  { profileBase, release } = {},
) {
  if (typeof password !== "string" || password.length < 12)
    throw new Error("A senha do backup deve ter pelo menos 12 caracteres.");
  const source = await lstat(archive);
  if (!source.isFile() || source.isSymbolicLink() || source.nlink > 1)
    throw new Error("O arquivo de backup é inválido.");
  const paths = destination
    ? {
        directory: resolve(destination),
        lock: `${resolve(destination)}.lock`,
      }
    : await newPilotDockerProfilePaths(profileBase);
  await assertNoDockerLinks(join(paths.directory, ".."));
  try {
    await lstat(paths.directory);
    throw new Error("O perfil Docker de destino já existe.");
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  await mkdir(paths.lock, { mode: 0o700 }).catch((error) => {
    if (error.code === "EEXIST")
      throw new Error("O perfil Docker de destino já está bloqueado.");
    throw error;
  });
  const owner = join(paths.lock, "owner");
  const token = randomUUID();
  try {
    await writeFile(owner, token, { flag: "wx", mode: 0o600 });
  } catch (error) {
    await rmdir(paths.lock).catch(() => {});
    throw error;
  }
  try {
    await restoreInstallation(archive, paths.directory, password, {
      requirePilotDockerProfile: true,
      async validateInstallation(staged, _config, computers, manifest) {
        await validatePilotDockerInstallation(staged, {
          expectedDataDirectory: join(paths.directory, "data"),
          release,
          computers,
          manifest,
        });
      },
    });
    await assertNoDockerLinks(paths.directory, true);
    return paths.directory;
  } finally {
    await assertNoDockerLinks(paths.lock, true);
    if ((await readFile(owner, "utf8")) === token) {
      await rm(owner);
      await rmdir(paths.lock);
    }
  }
}

export { createBackup as backupInstallation };
