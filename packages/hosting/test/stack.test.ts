import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { expect, onTestFinished, test, vi } from "vitest";
import type { Release, runCommand } from "../src/install.ts";
import { LocalStack } from "../src/stack.ts";

const release: Release = {
  version: "test",
  serviceImage: "persona-service:test",
  workImage: "persona-work:test",
  controllerEntry: "controller",
  serverEntry: "server",
  bridgeEntry: "bridge",
  staticDir: "web",
};

async function fixture() {
  const directory = await mkdtemp(join(tmpdir(), "persona-stack-"));
  onTestFinished(() => rm(directory, { recursive: true, force: true }));
  const run = vi.fn<typeof runCommand>(async (_command, args) =>
    args[0] === "info" ? '"linux"' : "",
  );
  const ready = vi.fn(async (_origin: string, signal: AbortSignal) => {
    signal.throwIfAborted();
  });
  return {
    directory,
    run,
    ready,
    stack: new LocalStack(directory, release, run, ready),
  };
}

test("container hosting starts only on selection and resumes its owned installation", async () => {
  const { directory, run, ready, stack } = await fixture();
  expect(run).not.toHaveBeenCalled();
  const signal = new AbortController().signal;
  const [first, repeated] = await Promise.all([
    stack.start(() => {}, signal),
    stack.start(() => {}, signal),
  ]);
  expect(first).toEqual(repeated);
  expect(ready).toHaveBeenCalledOnce();
  expect(stack.running).toBe(true);
  const spec = JSON.parse(
    await readFile(join(directory, "compose.json"), "utf8"),
  );
  expect(spec.services.app.ports).toEqual([
    `127.0.0.1:${new URL(first.origin).port}:4310`,
  ]);
  expect(spec.services.app.depends_on.controller.condition).toBe(
    "service_healthy",
  );
  expect(spec.services.controller.user).toBe("0:0");
  expect(spec.services.controller.cap_drop).toEqual(["ALL"]);
  expect(spec.services.controller.cap_add).toEqual(["DAC_READ_SEARCH"]);
  expect(spec.services.app.cap_add).toBeUndefined();
  expect(
    spec.services.app.volumes.some((item: { source: string }) =>
      item.source.includes("docker.sock"),
    ),
  ).toBe(false);
  expect(
    spec.services.controller.volumes.some(
      (item: { source: string }) => item.source === "/var/run/docker.sock",
    ),
  ).toBe(true);
  await stack.stop();
  expect(stack.running).toBe(false);
  const resumed = new LocalStack(directory, release, run, ready);
  expect(await resumed.start(() => {}, signal)).toEqual(first);
  expect(
    JSON.parse(await readFile(join(directory, "compose.json"), "utf8")).name,
  ).toBe(spec.name);
  await resumed.stop();
});

test("relocating a restored profile updates its persisted loopback port", async () => {
  const { directory, run, ready } = await fixture();
  await mkdir(join(directory, "data"));
  const configFile = join(directory, "persona.config.json");
  const oldPort = 1;
  await writeFile(
    join(directory, "stack.json"),
    JSON.stringify({
      id: "persona-012345abcdef",
      port: oldPort,
      directory: join(directory, "original-profile"),
    }),
  );
  await writeFile(
    configFile,
    JSON.stringify({
      host: "127.0.0.1",
      port: oldPort,
      dataDir: join(directory, "data"),
      deployment: "container",
      computer: { kind: "process" },
      workComputer: {
        kind: "controller",
        id: "team-0123456789ab",
        endpoint: "http://controller:4311",
        credentialFile: "/control/controller-token",
        network: "none",
        timeoutSeconds: 120,
      },
    }),
  );
  const stack = new LocalStack(directory, release, run, ready);
  const started = await stack.start(() => {}, new AbortController().signal);
  const config = JSON.parse(await readFile(configFile, "utf8"));
  const metadata = JSON.parse(
    await readFile(join(directory, "stack.json"), "utf8"),
  );
  expect(config.port).toBe(Number(new URL(started.origin).port));
  expect(config.port).not.toBe(oldPort);
  expect(metadata.directory).toBe(directory);
  await stack.stop();
});

test("failed readiness stops the selected stack and can be retried", async () => {
  const { stack, run, ready } = await fixture();
  ready.mockRejectedValueOnce(new Error("Service interrupted"));
  await expect(
    stack.start(() => {}, new AbortController().signal),
  ).rejects.toThrow("Service interrupted");
  expect(stack.running).toBe(false);
  expect(run.mock.calls.some(([, args]) => args.includes("stop"))).toBe(true);
  await stack.start(() => {}, new AbortController().signal);
  await stack.stop();
  expect(run.mock.calls.flatMap(([, args]) => args)).not.toContain("down");
});

test("unconfirmed cleanup after startup failure blocks reuse until stop is verified", async () => {
  const { stack, run } = await fixture();
  let failStop = true;
  run.mockImplementation(async (_command, args) => {
    if (args[0] === "info") return '"linux"';
    if (args.includes("up")) throw new Error("synthetic partial startup");
    if (args.includes("stop") && failStop) {
      failStop = false;
      throw new Error("synthetic daemon disconnect");
    }
    return "";
  });

  await expect(
    stack.start(() => {}, new AbortController().signal),
  ).rejects.toThrow("não foi possível confirmar o encerramento");
  expect(stack.stateUncertain).toBe(true);
  expect(stack.running).toBe(true);
  const calls = run.mock.calls.length;
  await expect(
    stack.start(() => {}, new AbortController().signal),
  ).rejects.toThrow("estado do ambiente Docker é incerto");
  expect(run).toHaveBeenCalledTimes(calls);

  await stack.stop();
  expect(stack.stateUncertain).toBe(false);
  expect(stack.running).toBe(false);
  expect(run.mock.calls.some(([, args]) => args.includes("ps"))).toBe(true);
});

test("stop verifies scoped Compose state and retains uncertainty while containers run", async () => {
  const { stack, run } = await fixture();
  let running = true;
  run.mockImplementation(async (_command, args) => {
    if (args[0] === "info") return '"linux"';
    if (args[0] === "compose" && args.includes("ps"))
      return running ? '{"State":"running"}\n' : "";
    return "";
  });
  await stack.start(() => {}, new AbortController().signal);

  await expect(stack.stop()).rejects.toThrow("Não foi possível confirmar");
  expect(stack.stateUncertain).toBe(true);
  const calls = run.mock.calls.length;
  await expect(
    stack.start(() => {}, new AbortController().signal),
  ).rejects.toThrow("estado do ambiente Docker é incerto");
  expect(run).toHaveBeenCalledTimes(calls);

  running = false;
  await stack.stop();
  expect(stack.running).toBe(false);
});

test("container observation requires both owned Compose services to be running", async () => {
  const { stack, run } = await fixture();
  run.mockImplementation(async (_command, args) => {
    if (args[0] === "compose" && args.includes("ps"))
      return [
        { Service: "app", State: "running" },
        { Service: "controller", State: "running" },
      ]
        .map((value) => JSON.stringify(value))
        .join("\n");
    return "";
  });
  expect(await stack.containersRunning()).toBe(true);
  run.mockImplementation(async (_command, args) =>
    args[0] === "compose" && args.includes("ps")
      ? '{"Service":"app","State":"running"}\n{"Service":"controller","State":"exited"}'
      : "",
  );
  expect(await stack.containersRunning()).toBe(false);
});

test("desktop pairing uses a bounded direct loopback request", async () => {
  const directory = await mkdtemp(join(tmpdir(), "persona-stack-pair-"));
  const server = createServer((request, response) => {
    expect(request.method).toBe("POST");
    expect(request.url).toBe("/hosting/desktop");
    expect(request.headers.authorization).toMatch(/^Bearer [a-f0-9]{64}$/);
    response.writeHead(200, { "content-type": "application/json" });
    response.end(
      JSON.stringify({ token: "c".repeat(64), expires: Date.now() + 60_000 }),
    );
  });
  let listening = false;
  onTestFinished(async () => {
    if (listening)
      await new Promise<void>((resolve) => server.close(() => resolve()));
    await rm(directory, { recursive: true, force: true });
  });
  const run = vi.fn<typeof runCommand>(async (_command, args) =>
    args[0] === "info" ? '"linux"' : "",
  );
  const ready = vi.fn(async (origin: string, signal: AbortSignal) => {
    const port = Number(new URL(origin).port);
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(port, "127.0.0.1", resolve);
    });
    listening = true;
    signal.throwIfAborted();
  });
  const stack = new LocalStack(directory, release, run, ready);
  await stack.start(() => {}, new AbortController().signal);
  const session = await stack.pair();
  expect(session.token).toBe("c".repeat(64));
  await stack.stop();
});

test("an aborted choice does not start a service or prepare a replacement", async () => {
  const { stack, run } = await fixture();
  const controller = new AbortController();
  controller.abort();
  await expect(stack.start(() => {}, controller.signal)).rejects.toThrow();
  await stack.stop();
  expect(run).not.toHaveBeenCalled();
});
