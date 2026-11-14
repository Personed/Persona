import { randomUUID } from "node:crypto";
import {
  lstat,
  mkdir,
  readdir,
  readFile,
  rename,
  rm,
  rmdir,
  writeFile,
} from "node:fs/promises";
import { basename, dirname, join, resolve } from "node:path";
import * as layout from "./layout.mjs";

export const pilotDockerProfileMarker = "persona-tauri-pilot-docker-v1\n";

const profileName =
  /^pilot-docker-(?:v1|restore-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$/;

export async function assertNoDockerLinks(path, recursive = false) {
  const stat = await lstat(path);
  if (stat.isSymbolicLink() || (stat.isFile() && stat.nlink > 1))
    throw new Error("Docker profile contains a symbolic link or hard link");
  if (recursive && stat.isDirectory())
    for (const name of await readdir(path))
      await assertNoDockerLinks(join(path, name), true);
}

async function checkIfPresent(path, recursive = false) {
  try {
    await assertNoDockerLinks(path, recursive);
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
}

async function rejectExisting(path, message) {
  try {
    await lstat(path);
  } catch (error) {
    if (error.code === "ENOENT") return;
    throw error;
  }
  throw new Error(message);
}

async function profileBase(directory) {
  const base = resolve(directory);
  for (let path = base; ; path = dirname(path)) {
    await checkIfPresent(path);
    if (dirname(path) === path) break;
  }
  await mkdir(base, { recursive: true });
  return base;
}

async function profilePaths(base, name) {
  const directory = join(base, name);
  const lock = join(base, `${name}.lock`);
  await checkIfPresent(directory, true);
  await checkIfPresent(lock, true);
  return { base, directory, lock };
}

export async function pilotDockerProfilePaths(
  directory = layout.dataDirectory,
) {
  const base = await profileBase(directory);
  const pointer = join(base, "pilot-docker-current.json");
  let name = "pilot-docker-v1";
  try {
    await assertNoDockerLinks(pointer);
    const selection = JSON.parse(await readFile(pointer, "utf8"));
    if (
      !selection ||
      typeof selection.profile !== "string" ||
      !profileName.test(selection.profile)
    )
      throw new Error("Invalid Docker profile selection");
    name = selection.profile;
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  return profilePaths(base, name);
}

export async function newPilotDockerProfilePaths(
  directory = layout.dataDirectory,
) {
  return profilePaths(
    await profileBase(directory),
    `pilot-docker-restore-${randomUUID()}`,
  );
}

export async function selectPilotDockerProfile(
  profileDirectory,
  baseDirectory = layout.dataDirectory,
) {
  const base = await profileBase(baseDirectory);
  const target = resolve(profileDirectory);
  const name = basename(target);
  if (dirname(target) !== base || !profileName.test(name))
    throw new Error("Invalid Docker profile selection");
  await assertNoDockerLinks(target, true);
  if (
    (await readFile(join(target, "pilot-profile"), "utf8")) !==
    pilotDockerProfileMarker
  )
    throw new Error("Not an isolated Docker profile");
  const paths = await profilePaths(base, name);
  await rejectExisting(paths.lock, "Docker profile is still locked");
  const pointer = join(base, "pilot-docker-current.json");
  await checkIfPresent(pointer);
  const pending = `${pointer}.${randomUUID()}.pending`;
  try {
    await writeFile(pending, JSON.stringify({ profile: name }), {
      flag: "wx",
      mode: 0o600,
    });
    await rename(pending, pointer);
  } finally {
    await rm(pending, { force: true });
  }
  return paths;
}

export async function acquirePilotDockerProfile(
  directory = layout.dataDirectory,
) {
  const paths = await pilotDockerProfilePaths(directory);
  await mkdir(paths.lock, { mode: 0o700 });
  const owner = join(paths.lock, "owner");
  const token = randomUUID();
  try {
    await writeFile(owner, token, { flag: "wx", mode: 0o600 });
    let created = false;
    try {
      await mkdir(paths.directory, { mode: 0o700 });
      created = true;
    } catch (error) {
      if (error.code !== "EEXIST") throw error;
    }
    await assertNoDockerLinks(paths.directory, true);
    const marker = join(paths.directory, "pilot-profile");
    let existing;
    try {
      existing = await readFile(marker, "utf8");
    } catch (error) {
      if (error.code !== "ENOENT" || !created) throw error;
    }
    if (existing === undefined) {
      await writeFile(marker, pilotDockerProfileMarker, {
        flag: "wx",
        mode: 0o600,
      });
    } else if (existing !== pilotDockerProfileMarker) {
      throw new Error("Not an isolated Docker profile");
    }
    let released = false;
    return {
      paths,
      async release() {
        if (released) return;
        await assertNoDockerLinks(paths.lock, true);
        if ((await readFile(owner, "utf8")) !== token)
          throw new Error("Docker profile lock ownership changed");
        await rm(owner);
        await rmdir(paths.lock);
        released = true;
      },
    };
  } catch (error) {
    try {
      await assertNoDockerLinks(paths.lock, true);
      if ((await readFile(owner, "utf8")) === token) {
        await rm(owner);
        await rmdir(paths.lock);
      }
    } catch {
      // Keep an uncertain lock rather than risk removing another owner's state.
    }
    throw error;
  }
}
