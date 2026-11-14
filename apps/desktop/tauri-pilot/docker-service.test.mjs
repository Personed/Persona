import assert from "node:assert/strict";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { acquirePilotDockerProfile } from "./docker-profile.mjs";
import {
  loadPilotDockerRuntime,
  startPilotDockerService,
} from "./docker-service.mjs";

async function fixture(t) {
  const directory = await mkdtemp(join(tmpdir(), "persona-docker-service-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

function profileLease(directory) {
  return acquirePilotDockerProfile(directory);
}

test("the Docker backend loads the fixed release and shared LocalStack without starting Docker", async () => {
  const { LocalStack, release } = await loadPilotDockerRuntime();
  assert.equal(typeof LocalStack, "function");
  assert.equal(typeof release.workImage, "string");
  assert.ok(release.workBuild?.context.startsWith(process.cwd()));
});

test("Docker supervisor service retains its isolated profile until verified stop", async (t) => {
  const directory = await fixture(t);
  let stack;
  class FakeStack {
    running = false;
    stateUncertain = false;
    constructor(profile, release) {
      this.profile = profile;
      this.release = release;
      stack = this;
    }
    async start(progress, signal) {
      progress({ phase: "checking", message: "Verificação sintética" });
      signal.throwIfAborted();
      this.running = true;
      return { origin: "http://127.0.0.1:51001" };
    }
    async pair() {
      return { token: "a".repeat(64), expires: Date.now() + 60_000 };
    }
    async containersRunning() {
      return this.running;
    }
    async stop() {
      this.running = false;
    }
  }
  const progress = [];
  const service = await startPilotDockerService({
    directory,
    acquireProfile: profileLease,
    load: async () => ({ LocalStack: FakeStack, release: { version: "test" } }),
    progress: (value) => progress.push(value),
  });

  assert.equal(service.origin, "http://127.0.0.1:51001");
  assert.equal(progress[0].phase, "checking");
  assert.equal(stack.profile.endsWith("pilot-docker-v1"), true);
  await assert.rejects(profileLease(directory), { code: "EEXIST" });
  const session = await service.pair();
  assert.equal(session.token, "a".repeat(64));
  assert.ok(session.expires > Date.now());

  assert.deepEqual(await service.stop(), { graceful: true, cleanup: false });
  const lease = await profileLease(directory);
  assert.equal(
    await readFile(join(lease.paths.directory, "pilot-profile"), "utf8"),
    "persona-tauri-pilot-docker-v1\n",
  );
  await lease.release();
});

test("uncertain shutdown retains the Docker profile lock and does not retry stop", async (t) => {
  const directory = await fixture(t);
  let stopCalls = 0;
  class UncertainStack {
    running = true;
    stateUncertain = false;
    async start() {
      return { origin: "http://127.0.0.1:51002" };
    }
    async pair() {
      throw new Error("synthetic pair failure");
    }
    async stop() {
      stopCalls++;
      this.stateUncertain = true;
      throw new Error("synthetic daemon disconnect");
    }
  }
  await assert.rejects(
    startPilotDockerService({
      directory,
      acquireProfile: profileLease,
      load: async () => ({
        LocalStack: UncertainStack,
        release: { version: "test" },
      }),
      verify: async () => {},
    }),
    /estado dos contêineres não pôde ser confirmado/,
  );
  assert.equal(stopCalls, 1);
  await assert.rejects(profileLease(directory), { code: "EEXIST" });
});

test("cancellation before Docker startup releases only the newly acquired lock", async (t) => {
  const directory = await fixture(t);
  const controller = new AbortController();
  controller.abort();
  let loaded = false;
  await assert.rejects(
    startPilotDockerService({
      directory,
      signal: controller.signal,
      acquireProfile: profileLease,
      load: async () => {
        loaded = true;
        throw new Error("must not load");
      },
    }),
    { name: "AbortError" },
  );
  assert.equal(loaded, false);
  const lease = await profileLease(directory);
  await lease.release();
});
