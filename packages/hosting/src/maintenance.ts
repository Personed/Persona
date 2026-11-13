import { spawn } from "node:child_process";
import {
  createCipheriv,
  createDecipheriv,
  randomBytes,
  scryptSync,
} from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import {
  appendFile,
  cp,
  lstat,
  mkdir,
  mkdtemp,
  open,
  readdir,
  readFile,
  rename,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import {
  basename,
  dirname,
  isAbsolute,
  join,
  relative,
  resolve,
  sep,
} from "node:path";
import { pipeline } from "node:stream/promises";
import {
  DockerComputer,
  hostCommand,
  sandboxNames,
} from "@persona/agent/computer/docker";
import {
  restoreComputers,
  storedComputers,
} from "@persona/agent/computer/storage";
import {
  type ComputerConfig,
  type DockerConfig,
  parseConfig,
  type RuntimeConfig,
} from "@persona/agent/config";
import { acquireDataLease } from "@persona/agent/lease";
import { create, extract } from "tar";
import { runCommand, writeConfiguration } from "./install.ts";

const magic = Buffer.from("PERSONA001");
const headerSize = magic.length + 16 + 12;

function contains(parent: string, child: string) {
  const distance = relative(parent, child);
  return (
    !isAbsolute(distance) &&
    distance !== ".." &&
    !distance.startsWith(`..${sep}`)
  );
}

async function checkPath(path: string, tree = false): Promise<void> {
  const info = await lstat(path).catch((error: NodeJS.ErrnoException) => {
    if (error.code !== "ENOENT") throw error;
    return undefined;
  });
  if (info?.isSymbolicLink() || (info?.isFile() && info.nlink > 1))
    throw new Error("迁移路径不能包含链接。");
  if (tree && info?.isDirectory()) {
    for (const entry of await readdir(path))
      await checkPath(join(path, entry), true);
  }
  if (!tree && dirname(path) !== path) await checkPath(dirname(path));
}

async function checkCredentialKey(data: string) {
  try {
    await stat(join(data, "hosting.sqlite"));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
    throw error;
  }
  const key = await stat(join(data, "credentials.key")).catch(
    (error: NodeJS.ErrnoException) => {
      if (error.code === "ENOENT")
        throw new Error("凭据数据库缺少 credentials.key。");
      throw error;
    },
  );
  if (!key.isFile() || key.size !== 32) throw new Error("凭据密钥文件无效。");
}

async function checkProcessComputerBinding(
  data: string,
  message = "该备份包含当前环境不支持的隔离电脑。",
) {
  const binding = await readFile(join(data, "computer.json"), "utf8").catch(
    (error: NodeJS.ErrnoException) => {
      if (error.code === "ENOENT") return undefined;
      throw error;
    },
  );
  if (binding === undefined) return;
  const previous = JSON.parse(binding);
  if (previous.kind !== "process" || previous.deployment === "container")
    throw new Error(message);
}

async function archiveTarget(
  directory: string,
  config: RuntimeConfig,
  computer: ComputerConfig,
): Promise<DockerConfig | undefined> {
  if (computer.kind === "process") return;
  if (computer.kind === "docker") return computer;
  const managed =
    config.workComputer?.kind === "controller"
      ? config.workComputer
      : config.computer;
  if (
    managed.kind !== "controller" ||
    managed.endpoint !== computer.endpoint ||
    managed.credentialFile !== computer.credentialFile
  )
    throw new Error("请先从远端电脑导出工作文件。");
  const controllerFile = join(directory, "controller.json");
  const controller = parseConfig(
    { computer: JSON.parse(await readFile(controllerFile, "utf8")).computer },
    controllerFile,
  ).computer;
  if (controller.kind !== "docker") throw new Error("工作电脑配置无效。");
  return computer.id === controller.id
    ? controller
    : { ...controller, id: computer.id, volume: `persona-${computer.id}-work` };
}

async function workArchive(
  directory: string,
  archive: string,
  restore: boolean,
) {
  const controllerFile = join(directory, "controller.json");
  let controller: { computer: unknown };
  try {
    controller = JSON.parse(await readFile(controllerFile, "utf8"));
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return;
    throw error;
  }
  const config = parseConfig(
    { computer: controller.computer },
    controllerFile,
  ).computer;
  if (config.kind !== "docker")
    throw new Error("此工作环境需要先导出工作文件。");
  const result = await archiveComputer(config, archive, restore);
  if (restore) {
    controller.computer = result.config;
    await writeConfiguration(controllerFile, controller);
  }
  return result;
}

function remapDockerComputer(config: DockerConfig) {
  const previous = config.volume;
  do {
    config.id = randomBytes(6).toString("hex");
    config.volume = `persona-${config.id}-work`;
  } while (config.volume === previous);
  return config;
}

async function dockerVolumeExists(config: DockerConfig) {
  const [program, ...args] = hostCommand(config, [
    "docker",
    "volume",
    "ls",
    "--filter",
    `name=${config.volume}`,
    "--format",
    "{{.Name}}",
  ]);
  if (!program) throw new Error("Docker 不可用。");
  const output = await runCommand(program, args);
  return output.split(/\r?\n/).includes(config.volume);
}

async function archiveComputer(
  config: DockerConfig,
  archive: string,
  restore: boolean,
) {
  const driver = new DockerComputer(config);
  if (!restore) {
    const state = await driver.inspect();
    if (state === "running")
      await driver.command([
        "docker",
        "container",
        "stop",
        "--time",
        "10",
        sandboxNames(config).container,
      ]);
  }
  if (restore) {
    remapDockerComputer(config);
    const [program, ...args] = hostCommand(config, [
      "docker",
      "volume",
      "create",
      config.volume,
    ]);
    if (!program) throw new Error("Docker 不可用。");
    await runCommand(program, args);
  }
  const command = hostCommand(config, [
    "docker",
    "run",
    "--rm",
    ...(restore ? ["-i"] : []),
    "--network=none",
    "--cap-drop=ALL",
    "--security-opt=no-new-privileges:true",
    "--user=1000:1000",
    "--mount",
    `type=volume,source=${config.volume},target=/home/persona${restore ? "" : ",readonly"}`,
    config.image,
    "tar",
    restore ? "xf" : "cf",
    "-",
    "-C",
    "/home/persona",
    ...(restore ? [] : ["."]),
  ]);
  const [program, ...args] = command;
  if (!program) throw new Error("工作文件归档命令缺失。");
  const child = spawn(program, args, {
    stdio: ["pipe", "pipe", "ignore"],
    windowsHide: true,
  });
  const exited = new Promise<void>((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (code) =>
      code === 0 ? resolve() : reject(new Error("工作文件归档未完成。")),
    );
  });
  const removeVolume = async () => {
    const [program, ...args] = hostCommand(config, [
      "docker",
      "volume",
      "rm",
      config.volume,
    ]);
    if (program) await runCommand(program, args);
  };
  try {
    if (restore) {
      child.stdout.resume();
      await Promise.all([
        pipeline(createReadStream(archive), child.stdin),
        exited,
      ]);
    } else {
      child.stdin.end();
      await Promise.all([
        pipeline(child.stdout, createWriteStream(archive, { mode: 0o600 })),
        exited,
      ]);
    }
  } catch (error) {
    if (restore) await removeVolume();
    throw error;
  }
  return { config, removeVolume };
}

export async function backupInstallation(
  directory: string,
  output: string,
  password: string,
) {
  if (password.length < 12) throw new Error("备份密码至少需要 12 个字符。");
  const root = resolve(directory);
  const target = resolve(output);
  const distance = relative(root, target);
  if (
    !isAbsolute(distance) &&
    distance !== ".." &&
    !distance.startsWith(`..${sep}`)
  )
    throw new Error("请将备份保存到安装目录之外。");
  const configFile = join(root, "persona.config.json");
  const configuration = JSON.parse(await readFile(configFile, "utf8"));
  const config = parseConfig(configuration, configFile);
  const external = resolve(config.dataDir) !== join(root, "data");
  if (external) {
    if (
      config.computer.kind !== "process" ||
      config.workComputer ||
      config.deployment !== "native"
    )
      throw new Error("外部数据目录迁移仅支持本机工作电脑。");
    if (
      contains(root, config.dataDir) ||
      contains(config.dataDir, root) ||
      contains(config.dataDir, target)
    )
      throw new Error("迁移路径不能重叠。");
    await checkPath(root);
    await checkPath(root, true);
    await checkPath(config.dataDir);
    if (!(await stat(config.dataDir)).isDirectory())
      throw new Error("持久数据目录无效。");
    await checkPath(config.dataDir, true);
    await checkPath(target);
    await checkCredentialKey(config.dataDir);
    try {
      await lstat(join(root, "data"));
      throw new Error("安装目录已包含其他数据目录。");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
  }
  const release = await acquireDataLease(config.dataDir);
  let stage: string | undefined;
  try {
    stage = await mkdtemp(join(tmpdir(), "persona-backup-"));
    if (external && (contains(root, stage) || contains(config.dataDir, stage)))
      throw new Error("迁移路径不能重叠。");
    const installation = join(stage, "installation");
    const copy = {
      recursive: true,
      filter: (path: string) =>
        ![
          "runtime.sqlite",
          "runtime.sqlite-wal",
          "runtime.sqlite-shm",
        ].includes(basename(path)),
    };
    if (external) {
      // The lease settles Persona writers; catalog migrations must touch only the snapshot.
      await cp(root, installation, copy);
      await cp(config.dataDir, join(installation, "data"), copy);
      await checkProcessComputerBinding(
        join(installation, "data"),
        "外部数据目录迁移仅支持本机工作电脑。",
      );
      await writeConfiguration(join(installation, "persona.config.json"), {
        ...configuration,
        dataDir: "data",
      });
    }
    const computers = await storedComputers(
      external
        ? join(installation, "data", "agent.sqlite")
        : join(config.dataDir, "agent.sqlite"),
    );
    if (external && computers.some((item) => item.config.kind !== "process"))
      throw new Error("外部数据目录迁移仅支持本机工作电脑。");
    const computerArchives: { id: string; archive: string }[] = [];
    for (const [index, item] of computers.entries()) {
      if (
        item.config.kind === "controller" &&
        config.computer.kind === "controller" &&
        item.config.id === config.computer.id
      )
        continue;
      const target = await archiveTarget(root, config, item.config);
      if (!target) continue;
      const archive = `computer-${index}.tar`;
      await archiveComputer(target, join(stage, archive), false);
      computerArchives.push({ id: item.id, archive });
    }
    let workComputerArchive: string | undefined;
    if (config.workComputer?.kind === "docker") {
      if (config.workComputer.ssh)
        throw new Error("远程 Docker 工作电脑模板暂不支持迁移。");
      if (await dockerVolumeExists(config.workComputer)) {
        workComputerArchive = "work-computer.tar";
        await archiveComputer(
          config.workComputer,
          join(stage, workComputerArchive),
          false,
        );
      }
    }
    if (!external) await cp(root, installation, copy);
    if (config.computer.kind === "controller")
      await workArchive(root, join(stage, "work.tar"), false);
    const plaintext = join(stage, "backup.tar.gz");
    const entries = [
      "manifest.json",
      "installation",
      ...computerArchives.map((item) => item.archive),
      ...(workComputerArchive ? [workComputerArchive] : []),
    ];
    try {
      await stat(join(stage, "work.tar"));
      entries.push("work.tar");
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
    }
    await writeFile(
      join(stage, "manifest.json"),
      JSON.stringify({
        version: 2,
        created: Date.now(),
        work: entries.includes("work.tar"),
        computers: computerArchives,
        workComputer:
          config.workComputer?.kind === "docker"
            ? { archive: workComputerArchive }
            : undefined,
      }),
    );
    await create(
      { file: plaintext, cwd: stage, gzip: true, portable: true },
      entries,
    );
    const salt = randomBytes(16);
    const iv = randomBytes(12);
    const cipher = createCipheriv(
      "aes-256-gcm",
      scryptSync(password, salt, 32),
      iv,
    );
    cipher.setAAD(magic);
    const handle = await open(target, "wx", 0o600);
    await handle.write(Buffer.concat([magic, salt, iv]));
    await handle.close();
    try {
      await pipeline(
        createReadStream(plaintext),
        cipher,
        createWriteStream(target, { flags: "a" }),
      );
      await appendFile(target, cipher.getAuthTag());
    } catch (error) {
      await rm(target, { force: true });
      throw error;
    }
  } finally {
    try {
      if (stage) await rm(stage, { recursive: true, force: true });
    } finally {
      await release();
    }
  }
}

export async function restoreInstallation(
  archive: string,
  destination: string,
  password: string,
  options: { requireProcessComputer?: boolean } = {},
) {
  const target = resolve(destination);
  try {
    await lstat(target);
    throw new Error("请恢复到新的安装目录。");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
  }
  await mkdir(dirname(target), { recursive: true });
  const stage = await mkdtemp(join(dirname(target), ".persona-restore-"));
  let work: Awaited<ReturnType<typeof workArchive>>;
  const restoredVolumes: Awaited<ReturnType<typeof archiveComputer>>[] = [];
  try {
    const handle = await open(archive, "r");
    const size = (await handle.stat()).size;
    const header = Buffer.alloc(headerSize);
    const tag = Buffer.alloc(16);
    await handle.read(header, 0, header.length, 0);
    await handle.read(tag, 0, 16, size - 16);
    await handle.close();
    if (
      size <= headerSize + 16 ||
      !header.subarray(0, magic.length).equals(magic)
    )
      throw new Error("备份格式无效。");
    const decipher = createDecipheriv(
      "aes-256-gcm",
      scryptSync(
        password,
        header.subarray(magic.length, magic.length + 16),
        32,
      ),
      header.subarray(magic.length + 16),
    );
    decipher.setAAD(magic);
    decipher.setAuthTag(tag);
    const plaintext = join(stage, "backup.tar.gz");
    await pipeline(
      createReadStream(archive, { start: headerSize, end: size - 17 }),
      decipher,
      createWriteStream(plaintext, { mode: 0o600 }),
    );
    await extract({
      file: plaintext,
      cwd: stage,
      strict: true,
      preserveOwner: false,
      filter: (path, entry) =>
        !path.startsWith("/") &&
        !path.split(/[\\/]/).includes("..") &&
        (!("type" in entry) ||
          (entry.type !== "SymbolicLink" && entry.type !== "Link")),
    });
    const manifest = JSON.parse(
      await readFile(join(stage, "manifest.json"), "utf8"),
    );
    if (manifest.version !== 1 && manifest.version !== 2)
      throw new Error("备份版本不兼容。");
    const workComputerArchive = manifest.workComputer?.archive;
    if (
      manifest.workComputer !== undefined &&
      (typeof manifest.workComputer !== "object" ||
        manifest.workComputer === null ||
        (workComputerArchive !== undefined &&
          workComputerArchive !== "work-computer.tar"))
    )
      throw new Error("备份中的工作电脑模板无效。");
    const restored = join(stage, "installation");
    const filename = join(restored, "persona.config.json");
    const config = JSON.parse(await readFile(filename, "utf8"));
    const sourceConfig = parseConfig(config, filename);
    if (
      options.requireProcessComputer &&
      (sourceConfig.computer.kind !== "process" ||
        (sourceConfig.workComputer &&
          sourceConfig.workComputer.kind !== "docker") ||
        sourceConfig.deployment !== "native")
    )
      throw new Error("该备份包含当前环境不支持的隔离电脑。");
    if (manifest.workComputer !== undefined && !sourceConfig.workComputer)
      throw new Error("备份中的工作电脑模板无效。");
    config.computer ??= sourceConfig.computer;
    config.dataDir = join(target, "data");
    if (config.computer.kind === "controller")
      config.computer.credentialFile = join(target, "controller-token");
    const database = join(restored, "data", "agent.sqlite");
    await checkCredentialKey(join(restored, "data"));
    if (options.requireProcessComputer)
      await checkProcessComputerBinding(join(restored, "data"));
    const computers = await storedComputers(database);
    if (
      options.requireProcessComputer &&
      computers.some((item) => item.config.kind !== "process")
    )
      throw new Error("该备份包含当前环境不支持的隔离电脑。");
    for (const entry of manifest.computers ?? []) {
      if (!/^computer-[0-9]+[.]tar$/.test(entry.archive))
        throw new Error("电脑备份路径无效。");
      const item = computers.find((item) => item.id === entry.id);
      if (!item || item.config.kind === "process")
        throw new Error("电脑备份配置不完整。");
      const targetConfig = await archiveTarget(
        restored,
        sourceConfig,
        item.config,
      );
      if (!targetConfig) throw new Error("电脑备份配置不完整。");
      const volume = await archiveComputer(
        targetConfig,
        join(stage, entry.archive),
        true,
      );
      restoredVolumes.push(volume);
      item.config =
        item.config.kind === "controller"
          ? { ...item.config, id: volume.config.id }
          : volume.config;
    }
    if (sourceConfig.workComputer?.kind === "docker") {
      if (
        manifest.version !== 2 ||
        manifest.workComputer === undefined ||
        sourceConfig.workComputer.ssh
      )
        throw new Error("该备份未包含可安全迁移的 Docker 工作电脑模板。");
      if (workComputerArchive) {
        const volume = await archiveComputer(
          { ...sourceConfig.workComputer },
          join(stage, workComputerArchive),
          true,
        );
        restoredVolumes.push(volume);
        config.workComputer = volume.config;
      } else {
        config.workComputer = remapDockerComputer({
          ...sourceConfig.workComputer,
        });
      }
    }
    if (manifest.work) {
      const controllerFile = join(restored, "controller.json");
      const controller = JSON.parse(await readFile(controllerFile, "utf8"));
      controller.credentialFile = join(target, "controller-token");
      await writeConfiguration(controllerFile, controller);
      work = await workArchive(restored, join(stage, "work.tar"), true);
      if (!work || config.computer.kind !== "controller")
        throw new Error("备份的工作电脑配置不完整。");
      for (const item of computers) {
        if (
          item.config.kind === "controller" &&
          item.config.id === config.computer.id
        )
          item.config = {
            ...item.config,
            id: work.config.id,
            credentialFile: join(target, "controller-token"),
          };
      }
      config.computer.id = work.config.id;
      if (computers.length)
        await rm(join(restored, "data/computer.json"), { force: true });
      else
        await writeConfiguration(join(restored, "data/computer.json"), {
          kind: "controller",
          id: work.config.id,
          endpoint: config.computer.endpoint,
        });
    }
    await restoreComputers(database, computers);
    await writeConfiguration(filename, config);
    await rename(restored, target);
    work = undefined;
    restoredVolumes.length = 0;
  } finally {
    await Promise.all(restoredVolumes.map((volume) => volume.removeVolume()));
    await work?.removeVolume();
    await rm(stage, { recursive: true, force: true });
  }
}
