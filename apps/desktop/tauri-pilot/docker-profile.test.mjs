import assert from "node:assert/strict";
import {
  link,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rm,
  symlink,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { basename, join } from "node:path";
import { test } from "node:test";
import {
  acquirePilotDockerProfile,
  newPilotDockerProfilePaths,
  pilotDockerProfileMarker,
  pilotDockerProfilePaths,
  selectPilotDockerProfile,
} from "./docker-profile.mjs";

async function fixture(t) {
  const base = await mkdtemp(join(tmpdir(), "persona-docker-profile-"));
  t.after(() => rm(base, { recursive: true, force: true }));
  return base;
}

async function materialize(paths, marker = pilotDockerProfileMarker) {
  await mkdir(paths.directory);
  await writeFile(join(paths.directory, "pilot-profile"), marker);
  return paths;
}

test("Docker defaults and restore selections never touch the native namespace", async (t) => {
  const base = await fixture(t);
  const native = join(base, "pilot-local-current.json");
  const nativeValue = JSON.stringify({ profile: "pilot-local-v1" });
  await writeFile(native, nativeValue);
  const initial = await materialize(await pilotDockerProfilePaths(base));
  assert.equal(basename(initial.directory), "pilot-docker-v1");
  assert.equal(initial.lock, `${initial.directory}.lock`);
  await selectPilotDockerProfile(initial.directory, base);
  const restored = await materialize(await newPilotDockerProfilePaths(base));
  assert.match(
    basename(restored.directory),
    /^pilot-docker-restore-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
  );
  assert.notEqual(
    (await newPilotDockerProfilePaths(base)).directory,
    restored.directory,
  );
  await selectPilotDockerProfile(restored.directory, base);
  assert.deepEqual(await pilotDockerProfilePaths(base), restored);
  await selectPilotDockerProfile(initial.directory, base);
  assert.deepEqual(await pilotDockerProfilePaths(base), initial);
  assert.equal(await readFile(native, "utf8"), nativeValue);
  assert.equal(
    (await readdir(base)).some((name) => name.endsWith(".pending")),
    false,
  );
});

test("invalid targets and foreign markers preserve the published selection", async (t) => {
  const base = await fixture(t);
  const initial = await materialize(await pilotDockerProfilePaths(base));
  await selectPilotDockerProfile(initial.directory, base);
  const pointer = join(base, "pilot-docker-current.json");
  const before = await readFile(pointer, "utf8");
  for (const name of [
    "pilot-local-v1",
    "pilot-docker-restore-not-a-uuid",
    "pilot-docker-restore-00000000-0000-1000-8000-000000000000",
  ]) {
    await assert.rejects(selectPilotDockerProfile(join(base, name), base));
  }
  const foreign = await materialize(
    await newPilotDockerProfilePaths(base),
    "persona-tauri-pilot-local-v1\n",
  );
  await assert.rejects(
    selectPilotDockerProfile(foreign.directory, base),
    /isolated Docker/,
  );
  await writeFile(
    join(foreign.directory, "pilot-profile"),
    `${pilotDockerProfileMarker}extra`,
  );
  await assert.rejects(
    selectPilotDockerProfile(foreign.directory, base),
    /isolated Docker/,
  );
  const outside = await fixture(t);
  await assert.rejects(
    selectPilotDockerProfile(
      (await materialize(await pilotDockerProfilePaths(outside))).directory,
      base,
    ),
  );
  assert.equal(await readFile(pointer, "utf8"), before);
  assert.equal(
    (await readdir(base)).some((name) => name.endsWith(".pending")),
    false,
  );
});

test("locked Docker profiles cannot become the published selection", async (t) => {
  const base = await fixture(t);
  const current = await materialize(await pilotDockerProfilePaths(base));
  await selectPilotDockerProfile(current.directory, base);
  const before = await readFile(
    join(base, "pilot-docker-current.json"),
    "utf8",
  );
  const restore = await materialize(await newPilotDockerProfilePaths(base));
  await mkdir(restore.lock);
  await assert.rejects(
    selectPilotDockerProfile(restore.directory, base),
    /still locked/,
  );
  assert.equal(
    await readFile(join(base, "pilot-docker-current.json"), "utf8"),
    before,
  );
});

test("malformed and foreign pointers are rejected rather than falling back", async (t) => {
  const base = await fixture(t);
  for (const value of [
    "{",
    "null",
    "{}",
    '{"profile":"pilot-local-v1"}',
    '{"profile":"../pilot-docker-v1"}',
  ]) {
    await writeFile(join(base, "pilot-docker-current.json"), value);
    await assert.rejects(pilotDockerProfilePaths(base));
  }
});

test("failed atomic publication retains the old pointer and removes staging files", async (t) => {
  const base = await fixture(t);
  const restore = await materialize(await newPilotDockerProfilePaths(base));
  const pointer = join(base, "pilot-docker-current.json");
  await mkdir(pointer);
  await writeFile(join(pointer, "retained"), "old selection evidence");
  await assert.rejects(selectPilotDockerProfile(restore.directory, base));
  assert.equal(
    await readFile(join(pointer, "retained"), "utf8"),
    "old selection evidence",
  );
  assert.equal(
    (await readdir(base)).some((name) => name.endsWith(".pending")),
    false,
  );
});

test("hard-linked pointers and profile contents cannot be read or selected", async (t) => {
  const base = await fixture(t);
  const initial = await materialize(await pilotDockerProfilePaths(base));
  await selectPilotDockerProfile(initial.directory, base);
  const pointer = join(base, "pilot-docker-current.json");
  const before = await readFile(pointer, "utf8");
  const alias = join(base, "pointer-alias");
  await link(pointer, alias);
  await assert.rejects(pilotDockerProfilePaths(base), /hard link/);
  await assert.rejects(
    selectPilotDockerProfile(initial.directory, base),
    /hard link/,
  );
  assert.equal(await readFile(alias, "utf8"), before);
  await rm(alias);
  const restore = await materialize(await newPilotDockerProfilePaths(base));
  await link(
    join(restore.directory, "pilot-profile"),
    join(base, "marker-alias"),
  );
  await assert.rejects(
    selectPilotDockerProfile(restore.directory, base),
    /hard link/,
  );
  await link(pointer, join(initial.directory, "linked-data"));
  await assert.rejects(pilotDockerProfilePaths(base), /hard link/);
  assert.equal(await readFile(pointer, "utf8"), before);
});

test("directory symlinks are rejected at the base and profile boundaries", async (t) => {
  const base = await fixture(t);
  const actual = join(base, "actual");
  await mkdir(actual);
  const linked = join(base, "linked");
  await symlink(
    actual,
    linked,
    process.platform === "win32" ? "junction" : "dir",
  );
  await assert.rejects(pilotDockerProfilePaths(linked), /symbolic link/);
  const profile = join(base, "pilot-docker-v1");
  await symlink(
    actual,
    profile,
    process.platform === "win32" ? "junction" : "dir",
  );
  await assert.rejects(pilotDockerProfilePaths(base), /symbolic link/);
  await assert.rejects(
    selectPilotDockerProfile(profile, base),
    /symbolic link/,
  );
});

test("Docker profile acquisition is exclusive and releases only its owned lock", async (t) => {
  const base = await fixture(t);
  const first = await acquirePilotDockerProfile(base);
  assert.equal(
    await readFile(join(first.paths.directory, "pilot-profile"), "utf8"),
    pilotDockerProfileMarker,
  );
  await assert.rejects(acquirePilotDockerProfile(base), { code: "EEXIST" });
  await first.release();
  await first.release();
  const second = await acquirePilotDockerProfile(base);
  await second.release();
  assert.equal((await readdir(base)).includes("pilot-docker-v1.lock"), false);
});

test("Docker profile acquisition refuses an unmarked existing directory", async (t) => {
  const base = await fixture(t);
  const paths = await pilotDockerProfilePaths(base);
  await mkdir(paths.directory);
  await writeFile(join(paths.directory, "kept.txt"), "synthetic evidence");
  await assert.rejects(acquirePilotDockerProfile(base));
  assert.equal(
    await readFile(join(paths.directory, "kept.txt"), "utf8"),
    "synthetic evidence",
  );
  assert.equal((await readdir(base)).includes("pilot-docker-v1.lock"), false);
});
