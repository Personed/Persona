import { spawn } from "node:child_process";
import { randomBytes, randomUUID } from "node:crypto";
import { once } from "node:events";
import { existsSync } from "node:fs";
import {
  lstat,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rename,
  rm,
  writeFile,
} from "node:fs/promises";
import { Agent, request } from "node:http";
import { createServer } from "node:net";
import { basename, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { dataDirectory, portable, staticDirectory } from "./layout.mjs";
import { stopOwnedChild } from "./stop-child.mjs";

export async function assertNoLinks(path, recursive = false) {
  const stat = await lstat(path);
  if (stat.isSymbolicLink() || (stat.isFile() && stat.nlink > 1))
    throw new Error("Test profile contains a symbolic link or hard link");
  if (recursive && stat.isDirectory())
    for (const name of await readdir(path))
      await assertNoLinks(join(path, name), true);
}

async function testBase(directory = dataDirectory) {
  const base = directory;
  for (let path = base; ; path = dirname(path)) {
    try {
      await assertNoLinks(path);
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
    if (dirname(path) === path) break;
  }
  await mkdir(base, { recursive: true });
  return base;
}

async function freshDirectory() {
  return mkdtemp(join(await testBase(), "service-"));
}

// No caller-selected paths: this capability owns only its freshly generated test root.
export async function createPersistentTestProfile() {
  const directory = await freshDirectory();
  let active;
  let closed = false;
  const validate = async () => {
    for (let path = dirname(directory); ; path = dirname(path)) {
      await assertNoLinks(path);
      if (dirname(path) === path) break;
    }
    await assertNoLinks(directory, true);
  };
  return {
    async start() {
      if (closed || active)
        throw new Error("Test profile is closed or still owned");
      const owner = Symbol();
      active = owner;
      try {
        await validate();
      } catch (error) {
        active = undefined;
        throw error;
      }
      // On uncertain startup/shutdown, retain ownership and evidence; never restart.
      const service = await startService(directory, true);
      const stop = service.stop;
      return {
        ...service,
        stop: async () => {
          const result = await stop();
          if (result.graceful && active === owner) active = undefined;
          return result;
        },
      };
    },
    async cleanup() {
      if (active) throw new Error("Test profile is still owned");
      if (closed) return;
      active = Symbol();
      try {
        await validate();
        await rm(directory, { recursive: true });
        closed = true;
      } finally {
        active = undefined;
      }
    },
  };
}

export async function startTestService() {
  return startService(await freshDirectory(), false);
}

export const pilotProfileMarker = "persona-tauri-pilot-local-v1\n";

const profileName =
  /^pilot-local-(?:v1|restore-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})$/;

export async function pilotProfilePaths(directory = dataDirectory) {
  const base = await testBase(directory);
  let name = "pilot-local-v1";
  const selectionPath = join(base, "pilot-local-current.json");
  try {
    await assertNoLinks(selectionPath);
    const selection = JSON.parse(await readFile(selectionPath, "utf8"));
    if (
      typeof selection.profile !== "string" ||
      !profileName.test(selection.profile)
    )
      throw new Error("The selected pilot profile is invalid");
    name = selection.profile;
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  return profilePaths(base, name);
}

export async function newPilotProfilePaths(directory = dataDirectory) {
  const base = await testBase(directory);
  return profilePaths(base, `pilot-local-restore-${randomUUID()}`);
}

function profilePaths(base, name) {
  return {
    base,
    directory: join(base, name),
    lock: join(base, `${name}.lock`),
  };
}

export async function selectPilotProfile(
  directory,
  baseDirectory = dataDirectory,
) {
  const base = await testBase(baseDirectory);
  const target = resolve(directory);
  const name = basename(target);
  if (dirname(target) !== resolve(base) || !profileName.test(name))
    throw new Error("Invalid pilot profile selection");
  await assertNoLinks(target, true);
  if (
    (await readFile(join(target, "pilot-profile"), "utf8")) !==
    pilotProfileMarker
  )
    throw new Error("Not an isolated pilot profile");
  const selection = join(base, "pilot-local-current.json");
  try {
    await assertNoLinks(selection);
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  const pending = `${selection}.${randomUUID()}.pending`;
  try {
    await writeFile(pending, JSON.stringify({ profile: name }), {
      flag: "wx",
      mode: 0o600,
    });
    await rename(pending, selection);
  } finally {
    await rm(pending, { force: true });
  }
  return profilePaths(base, name);
}

async function startLockedPersistentProfile(directory, lock) {
  // An uncertain shutdown leaves this lock intact. Never guess that its writer died.
  await mkdir(lock);
  // Startup failures retain the lock, rather than guessing whether work was issued.
  let created = false;
  try {
    await mkdir(directory, { mode: 0o700 });
    created = true;
  } catch (error) {
    if (error.code !== "EEXIST") throw error;
  }
  await assertNoLinks(directory, true);
  const identity = join(directory, "pilot-profile");
  if (created)
    await writeFile(identity, pilotProfileMarker, {
      flag: "wx",
      mode: 0o600,
    });
  if ((await readFile(identity, "utf8")) !== pilotProfileMarker)
    throw new Error("Not an isolated pilot profile");
  const service = await startService(directory, true);
  const stop = service.stop;
  let stopping;
  return {
    ...service,
    stop: () => {
      stopping ??= (async () => {
        const result = await stop();
        if (result.graceful) await rm(lock, { recursive: true });
        return result;
      })();
      return stopping;
    },
  };
}

export async function startPilotService() {
  const paths = await pilotProfilePaths();
  return startLockedPersistentProfile(paths.directory, paths.lock);
}

// Internal test affordance: product callers always use startPilotService().
export async function startPersistentService(directory) {
  const target = resolve(directory);
  const lock = join(dirname(target), `${basename(target)}.lock`);
  return startLockedPersistentProfile(target, lock);
}

async function reserveLoopbackPort() {
  for (;;) {
    // Windows may allocate low dynamic ports that Fetch rejects as unsafe.
    const port = 49152 + Math.floor(Math.random() * 16384);
    const reservation = createServer();
    try {
      reservation.listen(port, "127.0.0.1");
      await once(reservation, "listening");
    } catch (error) {
      reservation.close(() => undefined);
      if (error.code === "EADDRINUSE" || error.code === "EACCES") continue;
      throw error;
    }
    await new Promise((resolve, reject) =>
      reservation.close((error) => (error ? reject(error) : resolve())),
    );
    return port;
  }
}

async function savedProfile(directory) {
  const filename = join(directory, "persona.config.json");
  let value;
  try {
    value = JSON.parse(await readFile(filename, "utf8"));
  } catch (error) {
    if (error.code === "ENOENT") return {};
    throw error;
  }
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("Invalid imported profile configuration");
  if (
    (value.computer !== undefined && value.computer?.kind !== "process") ||
    (value.deployment !== undefined && value.deployment !== "native") ||
    value.publicUrl !== undefined ||
    value.desktopCredentialFile !== undefined ||
    (value.host !== undefined &&
      !["127.0.0.1", "localhost", "::1"].includes(value.host))
  )
    throw new Error("Imported profile is incompatible with the local pilot");
  return value;
}

function savedModel(profile) {
  if (profile.model === undefined) return undefined;
  if (
    typeof profile.model !== "object" ||
    profile.model === null ||
    typeof profile.model.provider !== "string" ||
    !profile.model.provider ||
    typeof profile.model.modelId !== "string" ||
    !profile.model.modelId
  )
    throw new Error("Invalid imported model configuration");
  return profile.model;
}

function savedWorkComputer(profile) {
  const value = profile.workComputer;
  if (value === undefined) return undefined;
  if (
    !value ||
    typeof value !== "object" ||
    value.kind !== "docker" ||
    value.ssh !== undefined ||
    typeof value.id !== "string" ||
    !/^[a-z0-9][a-z0-9_.-]{0,62}$/.test(value.id) ||
    typeof value.image !== "string" ||
    !/^[a-zA-Z0-9][a-zA-Z0-9_./:@-]+$/.test(value.image) ||
    typeof value.volume !== "string" ||
    !/^[a-z0-9][a-z0-9_.-]{0,62}$/.test(value.volume)
  )
    throw new Error("Invalid imported work computer configuration");
  return value;
}

// The Go service binary: an explicit override, then the bundled runtime, then PATH.
function serviceCommand() {
  if (process.env.PERSONA_SERVICE_BIN) return process.env.PERSONA_SERVICE_BIN;
  const name = process.platform === "win32" ? "persona.exe" : "persona";
  const bundled = fileURLToPath(new URL(`./runtime/${name}`, import.meta.url));
  return existsSync(bundled) ? bundled : name;
}

// The oRPC contract the service serves: bundled with the runtime, else the repo.
function contractDirectory() {
  if (process.env.PERSONA_CONTRACT_DIR) return process.env.PERSONA_CONTRACT_DIR;
  const bundled = fileURLToPath(
    new URL("./runtime/contracts/api", import.meta.url),
  );
  if (existsSync(bundled)) return bundled;
  return fileURLToPath(new URL("../../../contracts/api", import.meta.url));
}

// Pairing moved from Node IPC to the hosted desktop exchange (POST /hosting/desktop).
function pairDesktop(origin, credentialFile) {
  return (async () => {
    const credential = await readFile(credentialFile, "utf8");
    const url = new URL("/hosting/desktop", origin);
    const agent = new Agent({ proxyEnv: {} });
    try {
      return await new Promise((resolve, reject) => {
        const req = request(
          url,
          {
            agent,
            method: "POST",
            signal: AbortSignal.timeout(10_000),
            headers: { authorization: `Bearer ${credential}` },
          },
          (response) => {
            const chunks = [];
            response.on("error", reject);
            response.on("data", (chunk) => chunks.push(chunk));
            response.on("end", () => {
              try {
                if (response.statusCode !== 200)
                  throw new Error("Desktop pairing refused");
                const session = JSON.parse(
                  Buffer.concat(chunks).toString("utf8"),
                );
                if (
                  typeof session?.token !== "string" ||
                  !Number.isFinite(session?.expires)
                )
                  throw new Error("Invalid desktop session");
                resolve({ token: session.token, expires: session.expires });
              } catch (error) {
                reject(error);
              }
            });
          },
        );
        req.on("error", reject);
        req.end();
      });
    } finally {
      agent.destroy();
    }
  })();
}

async function startService(directory, retain) {
  let child;
  let exited;
  let stopping;
  const stop = () => {
    stopping ??= (async () => {
      const graceful = child
        ? await stopOwnedChild(child, exited, 30_000, 5_000, false)
        : true;
      // A killed writer cannot attest runtime cleanup; retain its disposable evidence.
      if (graceful && !retain)
        await rm(directory, { recursive: true, force: true });
      return { graceful, cleanup: graceful && !retain };
    })();
    return stopping;
  };
  try {
    const port = await reserveLoopbackPort();
    const config = join(directory, "config.json");
    const profile = await savedProfile(directory);
    const model = savedModel(profile);
    const workComputer = savedWorkComputer(profile);
    // The desktop exchanges this file for an admin session at POST /hosting/desktop.
    const desktopCredentialFile = join(directory, "desktop-credential");
    await writeFile(
      desktopCredentialFile,
      randomBytes(24).toString("base64url"),
      { mode: 0o600 },
    );
    await writeFile(
      config,
      JSON.stringify({
        ...profile,
        ...(model ? { model } : {}),
        ...(workComputer ? { workComputer } : {}),
        host: "127.0.0.1",
        port,
        dataDir: join(directory, "data"),
        staticDir: fileURLToPath(staticDirectory),
        desktopCredentialFile,
      }),
      { flag: retain ? "w" : "wx" },
    );
    // Smokes keep synthetic homes; normal desktop execution uses the OS user's CLI accounts.
    const home = join(directory, "home");
    await mkdir(home, { recursive: retain });
    const env = {
      ...(process.env.PERSONA_NATIVE_ACCOUNTS === "1"
        ? Object.fromEntries(
            Object.entries(process.env).filter(
              ([key]) => !/^(PERSONA_|SERVICE_)|^DATABASE_URL$/i.test(key),
            ),
          )
        : {
            HOME: home,
            USERPROFILE: home,
            APPDATA: home,
            LOCALAPPDATA: home,
            XDG_CONFIG_HOME: home,
            XDG_DATA_HOME: home,
            XDG_CACHE_HOME: home,
            TEMP: directory,
            TMP: directory,
          }),
      PERSONA_CONFIG: config,
    };
    if (portable) {
      env.PERSONA_WORKER_ENTRY = fileURLToPath(
        new URL(
          "./runtime/node_modules/@persona/agent/src/harness/worker.mjs",
          import.meta.url,
        ),
      );
      env.PERSONA_BRIDGE_ENTRY = fileURLToPath(
        new URL("./runtime/bridge.mjs", import.meta.url),
      );
    }
    // OS bootstrap variables only; never enumerate or print the environment.
    for (const key of [
      "SystemRoot",
      "WINDIR",
      "SystemDrive",
      "PATH",
      "COMSPEC",
      "PATHEXT",
    ])
      if (process.env[key]) env[key] = process.env[key];
    // The Go service stops on SIGTERM; it has no Node IPC channel.
    child = spawn(
      serviceCommand(),
      ["serve", "--contract", contractDirectory()],
      {
        env,
        cwd: directory,
        stdio: ["pipe", "pipe", "pipe"],
      },
    );
    exited = once(child, "exit");
    // Consume logs without leaking bootstrap/session credentials to the terminal.
    child.stderr.resume();
    await new Promise((resolve, reject) => {
      const timer = setTimeout(
        () => finish(new Error("Service readiness timed out")),
        90_000,
      );
      let output = "";
      const fail = () => finish(new Error("Service exited before ready"));
      const receive = (chunk) => {
        output = (output + chunk).slice(-4096);
        if (output.includes(`persona: listening on http://127.0.0.1:${port}`))
          finish();
      };
      const finish = (error) => {
        clearTimeout(timer);
        child.off("exit", fail);
        child.off("error", fail);
        child.stdout.off("data", receive);
        child.stdout.resume();
        if (error) reject(error);
        else resolve();
      };
      child.once("exit", fail);
      child.once("error", fail);
      child.stdout.on("data", receive);
    });
    return {
      origin: `http://127.0.0.1:${port}`,
      directory,
      pid: child.pid,
      exited,
      stop,
      pair: () =>
        new Promise((resolve, reject) => {
          const fail = () => reject(new Error("Service exited during pairing"));
          child.once("exit", fail);
          pairDesktop(`http://127.0.0.1:${port}`, desktopCredentialFile).then(
            (session) => {
              child.off("exit", fail);
              resolve(session);
            },
            (error) => {
              child.off("exit", fail);
              reject(error);
            },
          );
        }),
    };
  } catch (error) {
    await stop();
    throw error;
  }
}
