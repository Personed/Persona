import { access, readFile } from "node:fs/promises";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { acquirePilotDockerProfile } from "./docker-profile.mjs";
import { releaseEntry, runtimeDirectory } from "./layout.mjs";

async function hostingModule(name) {
  const packaged = join(
    runtimeDirectory,
    "node_modules",
    "@persona",
    "hosting",
    "src",
    name,
  );
  try {
    await access(packaged);
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
    return import(
      pathToFileURL(join(runtimeDirectory, "packages/hosting/src", name)).href
    );
  }
  return import(pathToFileURL(packaged).href);
}

export async function loadPilotDockerRuntime() {
  const [{ LocalStack }, { releaseSchema }] = await Promise.all([
    hostingModule("stack.ts"),
    hostingModule("install.ts"),
  ]);
  const release = releaseSchema.parse(
    JSON.parse(await readFile(releaseEntry, "utf8")),
  );
  for (const key of ["serviceBuild", "workBuild", "proxyBuild"])
    if (release[key]?.context)
      release[key] = {
        ...release[key],
        context: resolve(runtimeDirectory, release[key].context),
      };
  return { LocalStack, release };
}

function watchContainers(stack, signal, interval = 5000) {
  return new Promise((resolve) => {
    let timer;
    const finish = (result) => {
      clearTimeout(timer);
      signal.removeEventListener("abort", aborted);
      resolve(result);
    };
    const aborted = () => finish(undefined);
    const check = async () => {
      if (signal.aborted) return finish(undefined);
      try {
        if (!(await stack.containersRunning())) return finish("stopped");
      } catch {
        return finish("unknown");
      }
      timer = setTimeout(check, interval);
    };
    signal.addEventListener("abort", aborted, { once: true });
    timer = setTimeout(check, interval);
  });
}

export async function startPilotDockerService({
  directory,
  signal = new AbortController().signal,
  progress = () => {},
  acquireProfile = acquirePilotDockerProfile,
  load = loadPilotDockerRuntime,
} = {}) {
  const lease = await acquireProfile(directory);
  let stack;
  try {
    signal.throwIfAborted();
    const { LocalStack, release } = await load();
    stack = new LocalStack(lease.paths.directory, release);
    const { origin } = await stack.start(progress, signal);
    const session = await stack.pair();
    const monitor = new AbortController();
    const exited = watchContainers(stack, monitor.signal);
    let stopping;
    return {
      origin,
      pair: async () => session,
      exited,
      async stop() {
        stopping ??= (async () => {
          monitor.abort();
          await stack.stop();
          await lease.release();
          return { graceful: true, cleanup: false };
        })();
        return stopping;
      },
    };
  } catch (error) {
    let cleanupError;
    if (stack?.running && !stack.stateUncertain) {
      try {
        await stack.stop();
      } catch (cause) {
        cleanupError = cause;
      }
    }
    if (!stack?.stateUncertain && !cleanupError) await lease.release();
    if (cleanupError)
      throw new AggregateError(
        [error, cleanupError],
        "A inicialização Docker falhou e o estado dos contêineres não pôde ser confirmado.",
      );
    throw error;
  }
}
