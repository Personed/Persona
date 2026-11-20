import type { ChildProcess } from "node:child_process";
import { once } from "node:events";
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

export const test = base.extend({
  context: async ({ context }, use) => {
    await setTestLanguage(context);
    await use(context);
  },
});
