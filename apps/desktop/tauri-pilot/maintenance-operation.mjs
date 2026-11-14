import { cp, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { backupInstallation } from "./backup-maintenance.mjs";
import {
  backupPilotDockerInstallation,
  importPilotDockerBackup,
} from "./docker-maintenance.mjs";
import {
  assertNoDockerLinks,
  newPilotDockerProfilePaths,
  pilotDockerProfileMarker,
  pilotDockerProfilePaths,
  selectPilotDockerProfile,
} from "./docker-profile.mjs";
import { importPilotBackup } from "./import-backup.mjs";
import {
  assertNoLinks,
  newPilotProfilePaths,
  pilotProfileMarker,
  pilotProfilePaths,
  selectPilotProfile,
} from "./service.mjs";

export async function runMaintenanceOperation(
  { action, archive, password, runtime = "native" },
  options = {},
) {
  if (
    !["backup", "restore"].includes(action) ||
    !["native", "docker"].includes(runtime) ||
    !isAbsolute(archive) ||
    typeof password !== "string" ||
    password.length < 12
  )
    throw new Error("Invalid maintenance request");
  const docker = runtime === "docker";
  const profilePaths =
    options.profilePaths ??
    (docker ? pilotDockerProfilePaths : pilotProfilePaths);
  const newProfilePaths =
    options.newProfilePaths ??
    (docker ? newPilotDockerProfilePaths : newPilotProfilePaths);
  const selectProfile =
    options.selectProfile ??
    (docker ? selectPilotDockerProfile : selectPilotProfile);
  const assertNoProfileLinks = docker ? assertNoDockerLinks : assertNoLinks;
  const marker = docker ? pilotDockerProfileMarker : pilotProfileMarker;
  const backup =
    options.backup ??
    (docker ? backupPilotDockerInstallation : backupInstallation);
  const restore =
    options.restore ?? (docker ? importPilotDockerBackup : importPilotBackup);
  const paths = await profilePaths();
  for (let path = dirname(archive); ; path = dirname(path)) {
    await assertNoProfileLinks(path);
    if (dirname(path) === path) break;
  }
  if (action === "restore") {
    await mkdir(paths.lock, { mode: 0o700 });
    try {
      for (let path = paths.directory; ; path = dirname(path)) {
        await assertNoProfileLinks(path);
        if (dirname(path) === path) break;
      }
      await assertNoProfileLinks(paths.directory, true);
      if (
        (await readFile(join(paths.directory, "pilot-profile"), "utf8")) !==
        marker
      )
        throw new Error("Not an isolated pilot profile");
      const distance = relative(paths.directory, archive);
      if (
        !isAbsolute(distance) &&
        distance !== ".." &&
        !distance.startsWith(`..${sep}`)
      )
        throw new Error("Backup must be outside the profile");
      const destination = await newProfilePaths(paths.base);
      const restored = await restore(
        archive,
        password,
        destination.directory,
        docker ? { profileBase: paths.base } : undefined,
      );
      await selectProfile(restored ?? destination.directory, paths.base);
      return restored ?? destination.directory;
    } finally {
      await rm(paths.lock, { recursive: true });
    }
  }
  await mkdir(paths.lock, { mode: 0o700 });
  let stage;
  try {
    for (let path = paths.directory; ; path = dirname(path)) {
      await assertNoProfileLinks(path);
      if (dirname(path) === path) break;
    }
    await assertNoProfileLinks(paths.directory, true);
    if (
      (await readFile(join(paths.directory, "pilot-profile"), "utf8")) !==
      marker
    )
      throw new Error("Not an isolated pilot profile");
    const distance = relative(paths.directory, archive);
    if (
      !isAbsolute(distance) &&
      distance !== ".." &&
      !distance.startsWith(`..${sep}`)
    )
      throw new Error("Backup must be outside the profile");
    let configuration;
    try {
      configuration = JSON.parse(
        await readFile(join(paths.directory, "persona.config.json"), "utf8"),
      );
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
      configuration = JSON.parse(
        await readFile(join(paths.directory, "config.json"), "utf8"),
      );
    }
    if (
      typeof configuration.dataDir !== "string" ||
      resolve(paths.directory, configuration.dataDir) !==
        join(paths.directory, "data")
    )
      throw new Error("External profile data is not supported");
    // The profile lock excludes the service; preserve every file and the work-computer template.
    stage = await mkdtemp(join(tmpdir(), "persona-pilot-backup-"));
    const snapshot = join(stage, "installation");
    await cp(paths.directory, snapshot, { recursive: true });
    await writeFile(
      join(snapshot, "persona.config.json"),
      JSON.stringify({ ...configuration, dataDir: "data" }),
      { mode: 0o600 },
    );
    await backup(snapshot, archive, password);
  } finally {
    try {
      if (stage) await rm(stage, { recursive: true, force: true });
    } finally {
      await rm(paths.lock, { recursive: true });
    }
  }
}
