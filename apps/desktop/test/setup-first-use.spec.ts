import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { expect, type Page, test } from "@playwright/test";

type SetupResponse = { ok: boolean; error?: string };
type SetupSubmission = { mode: "local"; runtime: string };

const setupHtmlPath = fileURLToPath(
  new URL("../src/setup.html", import.meta.url),
);
const setupScriptPath = fileURLToPath(
  new URL("../src/setup.js", import.meta.url),
);

async function mountSetupWithMockBridge(
  page: Page,
  responses: SetupResponse[],
) {
  // Loads production markup/script; only the native Tauri bridge is mocked.
  const html = (await readFile(setupHtmlPath, "utf8"))
    .replace(/<meta http-equiv="Content-Security-Policy"[^>]*>\s*/i, "")
    .replace(/<link\b[^>]*>\s*/gi, "")
    .replace(/<script src="setup\.js"><\/script>/i, "");

  await page.setContent(html);
  await page.evaluate((queuedResponses) => {
    const submissions: SetupSubmission[] = [];
    const listeners = new Set<
      (value: { phase: string; message: string }) => void
    >();
    Object.assign(window, {
      __setupSubmissions: submissions,
      personaSetup: {
        platform: "linux",
        state: async () => ({
          instances: [],
          selected: null,
          prepare: false,
          error: "",
        }),
        submit: async (value: SetupSubmission) => {
          submissions.push(value);
          return queuedResponses.shift() ?? { ok: true };
        },
        cancel: async () => undefined,
        docker: async () => undefined,
        progress: (
          listener: (value: { phase: string; message: string }) => void,
        ) => listeners.add(listener),
      },
    });
  }, responses);
  await page.addScriptTag({ path: setupScriptPath });
  await expect(page.locator("#continue")).toBeVisible();
}

async function submissions(page: Page) {
  return page.evaluate(
    () =>
      (window as typeof window & { __setupSubmissions: SetupSubmission[] })
        .__setupSubmissions,
  );
}

test("first-use local direct startup submits native mode through a mocked Tauri bridge", async ({
  page,
}) => {
  await mountSetupWithMockBridge(page, [{ ok: true }]);

  await expect(page.locator('[name="mode"][value="local"]')).toBeChecked();
  await expect(page.locator('[name="runtime"][value="native"]')).toBeChecked();
  await page.locator("#continue").click();

  await expect
    .poll(() => submissions(page))
    .toEqual([{ mode: "local", runtime: "native" }]);
  await expect(page.locator("#error")).toBeHidden();
  await expect(page.locator("#continue")).toBeEnabled();
  await expect(page.locator("#setup")).toHaveAttribute("aria-busy", "false");
  await expect(page.locator("#progress")).toBeHidden();
});

test("first-use local setup keeps the selected runtime after an error and retries it through a mocked Tauri bridge", async ({
  page,
}) => {
  await mountSetupWithMockBridge(page, [
    { ok: false, error: "Falha simulada ao iniciar o serviço." },
    { ok: true },
  ]);

  await page.locator('[name="runtime"][value="docker"]').check();
  await page.locator("#continue").click();

  await expect(page.locator("#error")).toContainText(
    "Falha simulada ao iniciar o serviço.",
  );
  await expect(page.locator('[name="mode"][value="local"]')).toBeChecked();
  await expect(page.locator('[name="runtime"][value="docker"]')).toBeChecked();
  await expect(page.locator("#local")).toBeEnabled();
  await expect(page.locator("#continue")).toHaveText("Tentar novamente");

  await page.locator("#continue").click();

  await expect
    .poll(() => submissions(page))
    .toEqual([
      { mode: "local", runtime: "docker" },
      { mode: "local", runtime: "docker" },
    ]);
  await expect(page.locator("#error")).toBeHidden();
  await expect(page.locator('[name="mode"][value="local"]')).toBeChecked();
  await expect(page.locator('[name="runtime"][value="docker"]')).toBeChecked();
  await expect(page.locator("#progress")).toBeHidden();
});
