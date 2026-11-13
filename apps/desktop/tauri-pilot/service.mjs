import { fork } from "node:child_process";
import { once } from "node:events";
import {
  lstat,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { createServer } from "node:net";
import { basename, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { portable, staticDirectory } from "./layout.mjs";
import { stopOwnedChild } from "./stop-child.mjs";

export async function assertNoLinks(path, recursive = false) {
  const stat = await lstat(path);
  if (stat.isSymbolicLink())
    throw new Error("Test profile contains a symbolic link");
  if (recursive && stat.isDirectory())
    for (const name of await readdir(path))
      await assertNoLinks(join(path, name), true);
}

async function testBase() {
  const base = fileURLToPath(new URL("./target/test-data/", import.meta.url));
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

export async function pilotProfilePaths() {
  const base = await testBase();
  return {
    base,
    directory: join(base, "pilot-local-v1"),
    lock: join(base, "pilot-local-v1.lock"),
  };
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

async function startService(directory, retain) {
  let child;
  let exited;
  let stopping;
  const stop = () => {
    stopping ??= (async () => {
      const graceful = child ? await stopOwnedChild(child, exited) : true;
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
    await writeFile(
      config,
      JSON.stringify({
        ...(model ? { model } : {}),
        ...(workComputer ? { workComputer } : {}),
        host: "127.0.0.1",
        port,
        dataDir: join(directory, "data"),
        staticDir: fileURLToPath(staticDirectory),
        computer: { kind: "process" },
      }),
      { flag: retain ? "w" : "wx" },
    );
    // Do not inherit Persona overrides, credentials, model keys or real CLI homes.
    const home = join(directory, "home");
    await mkdir(home, { recursive: retain });
    const env = {
      PERSONA_CONFIG: config,
      HOME: home,
      USERPROFILE: home,
      APPDATA: home,
      LOCALAPPDATA: home,
      XDG_CONFIG_HOME: home,
      XDG_DATA_HOME: home,
      XDG_CACHE_HOME: home,
      TEMP: directory,
      TMP: directory,
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
    child = fork(
      fileURLToPath(new URL("./service-child.mjs", import.meta.url)),
      [],
      {
        env,
        cwd: directory,
        execArgv: [],
        stdio: ["ignore", "pipe", "pipe", "ipc"],
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
        if (output.includes(`Persona: http://127.0.0.1:${port}`)) finish();
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
          const timer = setTimeout(
            () => finish(new Error("Pairing timed out")),
            10_000,
          );
          const fail = () => finish(new Error("Service exited during pairing"));
          const receive = (message) => {
            if (
              message?.type === "desktop-session" &&
              typeof message.token === "string" &&
              Number.isFinite(message.expires)
            )
              finish(undefined, {
                token: message.token,
                expires: message.expires,
              });
          };
          const finish = (error, session) => {
            clearTimeout(timer);
            child.off("message", receive);
            child.off("exit", fail);
            if (error) reject(error);
            else resolve(session);
          };
          child.on("message", receive);
          child.once("exit", fail);
          child.send("pair-desktop", (error) => {
            if (error) finish(error);
          });
        }),
    };
  } catch (error) {
    await stop();
    throw error;
  }
}
