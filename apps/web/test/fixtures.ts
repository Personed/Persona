import type { ChildProcess } from "node:child_process";
import { once } from "node:events";
import { fileURLToPath } from "node:url";
import { type BrowserContext, test as base, expect } from "@playwright/test";

export type { FileChooser, Page, TestInfo } from "@playwright/test";
export { expect };

// Graceful disconnect is bounded: the fixture child's cleanup can outlast a
// test's remaining budget (or hang on a pending browser worker), which used to
// fail passing tests inside afterEach. Fall back to SIGKILL instead.
export async function stopWorker(worker: ChildProcess, timeoutMs = 15_000) {
  if (worker.exitCode !== null || worker.signalCode !== null) return;
  const exited = once(worker, "exit").catch(() => {});
  if (worker.connected) worker.disconnect();
  const timer = setTimeout(() => {
    if (worker.exitCode === null && worker.signalCode === null)
      worker.kill("SIGKILL");
  }, timeoutMs);
  await exited;
  clearTimeout(timer);
}

export async function setTestLanguage(context: BrowserContext) {
  await context.addInitScript(() => {
    try {
      localStorage.setItem("persona:language:v1", "zh-CN");
    } catch {}
  });
}

// PERSONA_WEB_SERVER=go runs the suite against the Go `persona serve`
// (apps/api/test/fixtures/go-server.ts) instead of the TypeScript service.
export const goService = process.env.PERSONA_WEB_SERVER === "go";

export function fixtureServer(name: "web-server" | "onboarding-server") {
  const file = goService && name === "web-server" ? "go-server" : name;
  return fileURLToPath(
    new URL(`../../api/test/fixtures/${file}.ts`, import.meta.url),
  );
}

const processComputer = {
  deployment: "native",
  error: null,
  id: "process",
  kind: "process",
  state: "running",
  os: "linux",
  arch: "x64",
  directory: "/tmp",
  boundary: "host",
  network: "host",
  name: "服务所在电脑",
  authorized: true,
  busy: false,
  capabilities: { commands: true, files: true, desktop: false },
};
const binding = { computerId: "process", revision: 0, requestId: "" };

// The Go service does not serve computer.* yet. Answer the reads the web needs
// to open a Bot with the TypeScript fixture's process computer, in the test
// layer only; every other computer procedure reaches Go unchanged.
async function stubComputer(context: BrowserContext) {
  await context.route(/\/rpc\/computer\/(get|setup|list)$/, async (route) => {
    const procedure = new URL(route.request().url()).pathname.split("/").pop();
    let json: unknown = { ...binding, computer: processComputer };
    if (procedure === "setup")
      json = { deployment: "native", canIsolate: false };
    if (procedure === "list") {
      const bots = await route.fetch({
        url: route
          .request()
          .url()
          .replace(/computer\/list$/, "bots/list"),
      });
      const { json: list } = (await bots.json()) as { json: { id: number }[] };
      json = {
        items: [processComputer],
        defaultId: null,
        bindings: Object.fromEntries(list.map(({ id }) => [id, binding])),
      };
    }
    await route.fulfill({ json: { json } });
  });
}

export const test = base.extend({
  context: async ({ context }, use) => {
    await setTestLanguage(context);
    if (goService) await stubComputer(context);
    await use(context);
  },
});
