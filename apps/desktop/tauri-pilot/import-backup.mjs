import { lstat, mkdir, rm, writeFile } from "node:fs/promises";
import { basename, dirname, join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { restoreInstallation } from "./backup-maintenance.mjs";
import {
  assertNoLinks,
  pilotProfileMarker,
  pilotProfilePaths,
} from "./service.mjs";

function destinationPaths(destination) {
  const directory = resolve(destination);
  return {
    directory,
    lock: join(dirname(directory), `${basename(directory)}.lock`),
  };
}

export async function importPilotBackup(archive, password, destination) {
  if (
    typeof password !== "string" ||
    password.length < 12 ||
    password.length > 1024
  )
    throw new Error("A senha do backup deve ter entre 12 e 1024 caracteres.");
  const source = await lstat(archive);
  if (!source.isFile() || source.isSymbolicLink())
    throw new Error("O arquivo de backup é inválido.");
  const paths = destination
    ? destinationPaths(destination)
    : await pilotProfilePaths();
  await assertNoLinks(dirname(paths.directory));
  try {
    await lstat(paths.directory);
    throw new Error("O perfil persistente do piloto já existe.");
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  try {
    await mkdir(paths.lock, { mode: 0o700 });
  } catch (error) {
    if (error.code === "EEXIST")
      throw new Error("O perfil persistente do piloto está em uso.");
    throw error;
  }
  let published = false;
  try {
    await restoreInstallation(archive, paths.directory, password, {
      requireProcessComputer: true,
    });
    published = true;
    await writeFile(
      join(paths.directory, "pilot-profile"),
      pilotProfileMarker,
      { flag: "wx", mode: 0o600 },
    );
    await assertNoLinks(paths.directory, true);
    return paths.directory;
  } catch (error) {
    if (published) await rm(paths.directory, { recursive: true, force: true });
    throw error;
  } finally {
    await rm(paths.lock, { recursive: true, force: true });
  }
}

async function readPassword() {
  const chunks = [];
  let size = 0;
  for await (const chunk of process.stdin) {
    size += chunk.length;
    if (size > 1025) throw new Error("A senha do backup é inválida.");
    chunks.push(chunk);
  }
  const password = Buffer.concat(chunks)
    .toString("utf8")
    .replace(/[\r\n]+$/, "");
  if (password.length < 12 || password.length > 1024)
    throw new Error("A senha do backup deve ter entre 12 e 1024 caracteres.");
  return password;
}

async function main() {
  if (process.argv.length !== 3)
    throw new Error("Use: persona-tauri-pilot.exe --import-backup <arquivo>");
  const password = await readPassword();
  await importPilotBackup(process.argv[2], password);
  console.log("Backup importado no perfil isolado do piloto.");
}

if (
  process.argv[1] &&
  pathToFileURL(process.argv[1]).href === import.meta.url
) {
  try {
    await main();
  } catch (error) {
    console.error(
      error instanceof Error
        ? error.message
        : "A importação não foi concluída.",
    );
    process.exitCode = 1;
  }
}
