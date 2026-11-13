import { EventEmitter } from "node:events";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Readable, Writable } from "node:stream";
import { create, extract } from "tar";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

const mocks = vi.hoisted(() => ({
  calls: [] as { program: string; args: string[] }[],
  execFile: vi.fn(),
  restoredTar: Buffer.alloc(0),
  spawn: vi.fn(),
  tar: Buffer.alloc(0),
  volumes: new Set<string>(),
}));

vi.mock("node:child_process", async () => {
  const { promisify } = await import("node:util");
  const execFile = Object.assign(vi.fn(), {
    [promisify.custom]: async (program: string, args: string[]) => {
      mocks.calls.push({ program, args: [...args] });
      if (program === "docker" && args[0] === "container" && args[1] === "ls")
        return { stdout: "", stderr: "" };
      return { stdout: "", stderr: "" };
    },
  });
  return { execFile, spawn: mocks.spawn };
});

const { backupInstallation, restoreInstallation } = await import(
  "../src/maintenance.ts"
);

function fakeChild(program: string, args: string[]) {
  mocks.calls.push({ program, args: [...args] });
  const child = new EventEmitter() as EventEmitter & {
    stdin: Writable;
    stdout: Readable;
    stderr: Readable;
  };
  const chunks: Buffer[] = [];
  let finished = false;
  const finish = () => {
    if (finished) return;
    finished = true;
    child.emit("exit", 0);
    child.emit("close", 0);
  };
  child.stdout = new Readable({ read() {} });
  child.stderr = new Readable({ read() {} });
  child.stdin = new Writable({
    write(chunk, _encoding, callback) {
      chunks.push(Buffer.from(chunk));
      callback();
    },
    final(callback) {
      mocks.restoredTar = Buffer.concat(chunks);
      callback();
      queueMicrotask(finish);
    },
  });
  queueMicrotask(() => {
    if (program !== "docker") {
      child.stdout.once("end", finish);
      child.stdout.push(null);
      return;
    }
    if (args[0] === "volume" && args[1] === "ls") {
      child.stdout.once("end", finish);
      child.stdout.push(`${[...mocks.volumes].join("\n")}\n`);
      child.stdout.push(null);
    } else if (args[0] === "volume" && args[1] === "create") {
      const name = args.indexOf("--name");
      const volume = (name >= 0 ? args[name + 1] : args[args.length - 1]) ?? "";
      mocks.volumes.add(volume);
      child.stdout.once("end", finish);
      child.stdout.push(null);
    } else if (args[0] === "volume" && args[1] === "rm") {
      mocks.volumes.delete(args[args.length - 1] ?? "");
      child.stdout.once("end", finish);
      child.stdout.push(null);
    } else if (args[0] === "run" && args.includes("cf")) {
      child.stdout.once("end", finish);
      child.stdout.push(mocks.tar);
      child.stdout.push(null);
    } else {
      child.stdout.push(null);
    }
  });
  return child;
}

beforeEach(() => {
  mocks.calls.length = 0;
  mocks.restoredTar = Buffer.alloc(0);
  mocks.tar = Buffer.alloc(0);
  mocks.volumes.clear();
  vi.clearAllMocks();
  mocks.spawn.mockImplementation(fakeChild);
});

afterEach(() => {
  vi.clearAllMocks();
});

async function fixture(volume: string) {
  const directory = await mkdtemp(join(tmpdir(), "persona-work-backup-"));
  const root = join(directory, "host");
  const data = join(root, "data");
  await mkdir(data, { recursive: true });
  await writeFile(join(data, "artifact.txt"), "synthetic work");
  await writeFile(
    join(root, "persona.config.json"),
    JSON.stringify({
      dataDir: "data",
      computer: { kind: "process" },
      workComputer: {
        kind: "docker",
        id: "source-template",
        image: "fixture:1",
        volume,
      },
    }),
  );
  const tarDirectory = await mkdtemp(join(tmpdir(), "persona-work-tar-"));
  await writeFile(join(tarDirectory, "artifact.txt"), "synthetic work");
  await create(
    { cwd: tarDirectory, file: join(tarDirectory, "work.tar"), gzip: false },
    ["artifact.txt"],
  );
  mocks.tar = await readFile(join(tarDirectory, "work.tar"));
  return { directory, root, tarDirectory };
}

test("Docker work template backup archives read-only and restores into a new volume", async () => {
  const f = await fixture("source-volume");
  mocks.volumes.add("source-volume");
  const archive = join(f.directory, "backup.persona");
  const password = "synthetic-password";
  try {
    await backupInstallation(f.root, archive, password);
    const source = JSON.parse(
      await readFile(join(f.root, "persona.config.json"), "utf8"),
    );
    const backupRun = mocks.calls.find(
      (call) => call.args[0] === "run" && call.args.includes("cf"),
    );
    expect(backupRun).toBeDefined();
    expect(backupRun?.args).toContain(
      "type=volume,source=source-volume,target=/home/persona,readonly",
    );

    const restored = join(f.directory, "restored");
    await restoreInstallation(archive, restored, password, {
      requireProcessComputer: true,
    });
    const config = JSON.parse(
      await readFile(join(restored, "persona.config.json"), "utf8"),
    );
    expect(config.workComputer.id).not.toBe("source-template");
    expect(config.workComputer.volume).not.toBe("source-volume");
    expect(mocks.volumes.has("source-volume")).toBe(true);
    expect(mocks.volumes.has(config.workComputer.volume)).toBe(true);
    expect(
      mocks.calls.some(
        (call) =>
          call.args[0] === "run" &&
          call.args.includes("xf") &&
          call.args.some(
            (argument) =>
              argument ===
              `type=volume,source=${config.workComputer.volume},target=/home/persona`,
          ),
      ),
    ).toBe(true);
    expect(source.workComputer.volume).toBe("source-volume");

    const extracted = join(f.directory, "extracted");
    await mkdir(extracted);
    await writeFile(join(f.directory, "restored.tar"), mocks.restoredTar);
    await extract({ file: join(f.directory, "restored.tar"), cwd: extracted });
    expect(await readFile(join(extracted, "artifact.txt"), "utf8")).toBe(
      "synthetic work",
    );
  } finally {
    await rm(f.directory, { recursive: true, force: true });
    await rm(f.tarDirectory, { recursive: true, force: true });
  }
});

test("remote Docker work template backup is rejected", async () => {
  const f = await fixture("remote-volume");
  const archive = join(f.directory, "backup.persona");
  try {
    await writeFile(
      join(f.root, "persona.config.json"),
      JSON.stringify({
        dataDir: "data",
        computer: { kind: "process" },
        workComputer: {
          kind: "docker",
          id: "remote-template",
          image: "fixture:1",
          volume: "remote-volume",
          ssh: {
            host: "fixture.example",
            knownHostsFile: "known_hosts",
            hostKeyAlias: "fixture",
          },
        },
      }),
    );
    await expect(
      backupInstallation(f.root, archive, "synthetic-password"),
    ).rejects.toThrow("远程 Docker");
    await expect(readFile(archive)).rejects.toMatchObject({ code: "ENOENT" });
  } finally {
    await rm(f.directory, { recursive: true, force: true });
    await rm(f.tarDirectory, { recursive: true, force: true });
  }
});

test("Docker work template without a source volume restores a fresh unmapped volume", async () => {
  const f = await fixture("missing-volume");
  const archive = join(f.directory, "backup.persona");
  const password = "synthetic-password";
  try {
    await backupInstallation(f.root, archive, password);
    expect(
      mocks.calls.some(
        (call) => call.args[0] === "run" && call.args.includes("cf"),
      ),
    ).toBe(false);
    const restored = join(f.directory, "restored");
    await restoreInstallation(archive, restored, password, {
      requireProcessComputer: true,
    });
    const config = JSON.parse(
      await readFile(join(restored, "persona.config.json"), "utf8"),
    );
    expect(config.workComputer.id).not.toBe("source-template");
    expect(config.workComputer.volume).not.toBe("missing-volume");
    expect(mocks.volumes.size).toBe(0);
  } finally {
    await rm(f.directory, { recursive: true, force: true });
    await rm(f.tarDirectory, { recursive: true, force: true });
  }
});
