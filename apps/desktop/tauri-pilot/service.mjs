import { fork } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:net";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { portable, staticDirectory } from "./layout.mjs";
import { stopOwnedChild } from "./stop-child.mjs";

export async function startTestService() {
  const base = fileURLToPath(new URL("./target/test-data/", import.meta.url));
  await mkdir(base, { recursive: true });
  const directory = await mkdtemp(join(base, "service-"));
  let child;
  let exited;
  let stopping;
  const stop = () => {
    stopping ??= (async () => {
      const graceful = child ? await stopOwnedChild(child, exited) : true;
      // A killed writer cannot attest runtime cleanup; retain its disposable evidence.
      if (graceful) await rm(directory, { recursive: true, force: true });
      return { graceful, cleanup: graceful };
    })();
    return stopping;
  };
  try {
    const reservation = createServer();
    reservation.listen(0, "127.0.0.1");
    await once(reservation, "listening");
    const port = reservation.address().port;
    await new Promise((resolve, reject) =>
      reservation.close((error) => (error ? reject(error) : resolve())),
    );
    const config = join(directory, "config.json");
    await writeFile(
      config,
      JSON.stringify({
        host: "127.0.0.1",
        port,
        dataDir: join(directory, "data"),
        staticDir: fileURLToPath(staticDirectory),
        computer: { kind: "process" },
      }),
      { flag: "wx" },
    );
    // Do not inherit Persona overrides, credentials, model keys or real CLI homes.
    const home = join(directory, "home");
    await mkdir(home);
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
