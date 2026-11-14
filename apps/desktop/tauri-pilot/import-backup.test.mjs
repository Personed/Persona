import assert from "node:assert/strict";
import {
  access,
  mkdir,
  mkdtemp,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Readable } from "node:stream";
import { test } from "node:test";
import { backupInstallation } from "./backup-maintenance.mjs";
import { importPilotBackup, readPassword } from "./import-backup.mjs";
import { pilotProfileMarker, startPersistentService } from "./service.mjs";

for (const incompatible of [
  { publicUrl: "https://synthetic.example" },
  { desktopCredentialFile: "synthetic-credential" },
  { host: "0.0.0.0" },
  { deployment: "container" },
]) {
  test(`local pilot rejects ${Object.keys(incompatible)[0]} before publication and startup`, async () => {
    const root = await mkdtemp(join(tmpdir(), "persona-rejected-profile-"));
    try {
      const source = join(root, "source");
      await mkdir(join(source, "data"), { recursive: true });
      const configuration = JSON.stringify({
        dataDir: "data",
        computer: { kind: "process" },
        ...incompatible,
      });
      const filename = join(source, "persona.config.json");
      await writeFile(filename, configuration);
      await writeFile(join(source, "data", "synthetic.txt"), "synthetic only");
      const archive = join(root, "backup.persona");
      const password = "synthetic-password";
      await backupInstallation(source, archive, password);
      const destination = join(root, "imported");
      await assert.rejects(importPilotBackup(archive, password, destination));
      await assert.rejects(access(destination), { code: "ENOENT" });
      await assert.rejects(access(`${destination}.lock`), { code: "ENOENT" });
      assert.equal(await readFile(filename, "utf8"), configuration);
      assert.equal(
        await readFile(join(source, "data", "synthetic.txt"), "utf8"),
        "synthetic only",
      );
      await writeFile(join(source, "pilot-profile"), pilotProfileMarker);
      await assert.rejects(startPersistentService(source), /incompatible/);
      await assert.rejects(access(join(source, "config.json")), {
        code: "ENOENT",
      });
      assert.equal(await readFile(filename, "utf8"), configuration);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });
}

for (const [name, password] of [
  ["long", "x".repeat(2048)],
  ["UTF-16 minimum", "😀".repeat(6)],
  ["multibyte", "密".repeat(600)],
]) {
  test(`CLI backup imports with ${name} password`, async () => {
    const root = await mkdtemp(join(tmpdir(), "persona-password-test-"));
    try {
      const source = join(root, "source");
      await mkdir(join(source, "data"), { recursive: true });
      await writeFile(
        join(source, "persona.config.json"),
        JSON.stringify({
          dataDir: "data",
          computer: { kind: "process" },
        }),
      );
      await writeFile(join(source, "data", "synthetic.txt"), "synthetic only");
      const archive = join(root, "backup.persona");
      await backupInstallation(source, archive, password);
      const bytes = Buffer.from(password);
      const received = await readPassword(
        Readable.from([bytes.subarray(0, 1), bytes.subarray(1)]),
      );
      assert.equal(received, password);
      const destination = join(root, "imported");
      await importPilotBackup(archive, received, destination);
      assert.equal(
        await readFile(join(destination, "data", "synthetic.txt"), "utf8"),
        "synthetic only",
      );
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  });
}

test("short passwords remain invalid", async () => {
  await assert.rejects(
    readPassword(Readable.from([Buffer.from("😀".repeat(5))])),
  );
});
