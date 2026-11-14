import { fork } from "node:child_process";
import { once } from "node:events";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  type ElectronApplication,
  expect,
  type Page,
  test,
} from "@playwright/test";

async function captureWindow(
  desktop: ElectronApplication,
  page: Page,
  path: string,
) {
  await page.evaluate(
    () =>
      new Promise<void>((resolve) => {
        requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
      }),
  );
  const png = await desktop.evaluate(async ({ BrowserWindow }, url) => {
    const window = BrowserWindow.getAllWindows().find(
      (window) => window.webContents.getURL() === url,
    );
    if (!window) throw new Error("Window not found");
    return (await window.capturePage()).toPNG().toString("base64");
  }, page.url());
  await writeFile(path, Buffer.from(png, "base64"));
}

async function openConnectionSetup(desktop: ElectronApplication) {
  const opened = desktop.waitForEvent("window");
  await desktop.evaluate(({ Menu }) =>
    Menu.getApplicationMenu()
      ?.items[0]?.submenu?.items.find(
        (item) => item.label === "Conectar a outro Persona…",
      )
      ?.click(),
  );
  return opened;
}

test("native setup starts and resumes with Docker unavailable", async ({
  playwright,
}, testInfo) => {
  const directory = await mkdtemp(join(tmpdir(), "persona-native-"));
  const executablePath = createRequire(
    new URL("../package.json", import.meta.url),
  )("electron") as string;
  const args = [
    fileURLToPath(new URL("./entry.mjs", import.meta.url)),
    `--test-data-dir=${directory}`,
  ];
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      (entry): entry is [string, string] => entry[1] !== undefined,
    ),
  );
  delete env.ELECTRON_RUN_AS_NODE;
  env.PATH = directory;
  let desktop = await playwright._electron.launch({
    executablePath,
    args,
    env,
  });
  try {
    const localOpened = desktop.waitForEvent("window");
    const initialSetup = await desktop.firstWindow();
    await expect(
      initialSetup.getByRole("heading", { name: "Bem-vindo ao Persona" }),
    ).toBeVisible();
    await localOpened;
    const setup = await openConnectionSetup(desktop);
    await desktop.evaluate(({ nativeTheme }) => {
      nativeTheme.themeSource = "dark";
    });
    await setup.emulateMedia({ colorScheme: "dark" });
    await expect(setup.locator("body")).toHaveCSS(
      "background-color",
      "rgb(15, 15, 16)",
    );
    await setup.screenshot({
      path: testInfo.outputPath("service-choice-dark.png"),
    });
    await desktop.evaluate(({ nativeTheme }) => {
      nativeTheme.themeSource = "light";
    });
    await setup.emulateMedia({ colorScheme: "light" });
    await expect(setup.locator("body")).toHaveCSS(
      "background-color",
      "rgb(253, 253, 253)",
    );
    await setup.screenshot({
      path: testInfo.outputPath("service-choice-light.png"),
    });
    await expect(setup.getByRole("progressbar")).toBeHidden();
    await desktop.evaluate(({ BrowserWindow }) => {
      const window = BrowserWindow.getAllWindows()[0];
      window?.setSize(420, 640);
      window?.webContents.setZoomFactor(1.25);
    });
    await expect.poll(() => setup.evaluate(() => innerWidth)).toBeLessThan(400);
    await expect
      .poll(() =>
        setup
          .locator("main")
          .evaluate((main) => main.scrollWidth <= main.clientWidth),
      )
      .toBe(true);
    const next = setup.getByRole("button", { name: "Continuar", exact: true });
    await next.scrollIntoViewIfNeeded();
    await expect(next).toBeInViewport({ ratio: 1 });
    await captureWindow(
      desktop,
      setup,
      testInfo.outputPath("service-choice-zoom.png"),
    );
    await desktop.evaluate(({ BrowserWindow }) => {
      const window = BrowserWindow.getAllWindows()[0];
      window?.setSize(680, 760);
      window?.webContents.setZoomFactor(1);
    });
    await setup.getByLabel("Executar em Docker", { exact: false }).check();
    await setup.getByRole("button", { name: "Continuar", exact: true }).click();
    await expect(setup.getByRole("alert")).toContainText("Docker Desktop");
    await setup.getByLabel("Execução direta", { exact: false }).check();
    const opened = desktop.waitForEvent("window");
    await setup.getByRole("button", { name: "Continuar", exact: true }).click();
    const page = await opened;
    await expect(
      page.getByRole("heading", { name: "Onde o Bot irá trabalhar?" }),
    ).toBeVisible();
    await desktop.evaluate(({ BrowserWindow }) => {
      const window = BrowserWindow.getAllWindows().find((window) =>
        window.webContents.getURL().startsWith("http"),
      );
      window?.setSize(640, 680);
      window?.webContents.setZoomFactor(1.25);
    });
    await expect.poll(() => page.evaluate(() => innerWidth)).toBeLessThan(600);
    expect(
      await page
        .locator("main")
        .evaluate((main) => main.scrollWidth <= main.clientWidth),
    ).toBe(true);
    const continueSetup = page.getByRole("button", {
      name: "Autorizar e Continuar",
      exact: true,
    });
    await continueSetup.scrollIntoViewIfNeeded();
    await expect(continueSetup).toBeInViewport({ ratio: 1 });
    await captureWindow(
      desktop,
      page,
      testInfo.outputPath("computer-setup-zoom.png"),
    );
    const origin = new URL(page.url()).origin;
    await desktop.close();
    desktop = await playwright._electron.launch({ executablePath, args, env });
    const resumed = await desktop.waitForEvent("window", {
      predicate: (window) => window.url().startsWith(origin),
    });
    await expect(
      resumed.getByRole("heading", { name: "Onde o Bot irá trabalhar?" }),
    ).toBeVisible();
  } finally {
    await desktop.close();
    await rm(directory, { recursive: true, force: true });
  }
});

test("desktop opens setup, pairs a remote Persona and restores the connection", async ({
  playwright,
}, testInfo) => {
  const directory = await mkdtemp(join(tmpdir(), "persona-desktop-"));
  const worker = fork(
    fileURLToPath(
      new URL("../../api/test/fixtures/web-server.ts", import.meta.url),
    ),
    [join(directory, "agent.sqlite"), "0", "hosting"],
    { silent: true, execArgv: [] },
  );
  worker.stdout?.resume();
  worker.stderr?.resume();
  const [ready] = (await once(worker, "message")) as [
    { port: number; pairing: string },
  ];
  const executablePath = createRequire(
    new URL("../package.json", import.meta.url),
  )("electron") as string;
  const args = [
    fileURLToPath(new URL("./entry.mjs", import.meta.url)),
    `--test-data-dir=${join(directory, "profile")}`,
  ];
  await mkdir(join(directory, "profile"));
  const env = Object.fromEntries(
    Object.entries(process.env).filter(
      (entry): entry is [string, string] => entry[1] !== undefined,
    ),
  );
  delete env.ELECTRON_RUN_AS_NODE;
  env.PATH = directory;
  let desktop = await playwright._electron.launch({
    executablePath,
    args,
    env,
  });
  try {
    const localOpened = desktop.waitForEvent("window");
    const initialSetup = await desktop.firstWindow();
    await expect(
      initialSetup.getByRole("heading", { name: "Bem-vindo ao Persona" }),
    ).toBeVisible();
    await localOpened;
    const setup = await openConnectionSetup(desktop);
    await setup
      .getByLabel("Conectar a um Persona existente", { exact: false })
      .check();
    await setup
      .getByLabel("Endereço do Persona")
      .fill(`http://127.0.0.1:${ready.port}`);
    const opened = desktop.waitForEvent("window");
    await setup.getByRole("button", { name: "Continuar", exact: true }).click();
    const page = await opened;
    await expect(
      page.getByRole("heading", { name: "Conectar a este Persona" }),
    ).toBeVisible();
    await page.getByLabel("Código", { exact: true }).fill(ready.pairing);
    await page.getByRole("button", { name: "Conectar", exact: true }).click();
    await expect(
      page.getByRole("button", { name: "Persona", exact: true }),
    ).toHaveAttribute("aria-expanded", "true");
    await expect(page.locator(".app-titlebar, .app-rail")).toHaveCount(0);
    await expect(page.locator("#bot-details")).toBeVisible();
    await expect(page.locator(".chat-identity .bot-avatar")).toBeVisible();
    await captureWindow(
      desktop,
      page,
      testInfo.outputPath("workspace-native.png"),
    );
    await page.getByRole("combobox", { name: "Modelos", exact: true }).click();
    const selector = page.locator(".model-selector-popup");
    await expect(selector.getByRole("tab")).toHaveCount(5);
    await selector.getByRole("tab", { name: "OpenCode", exact: true }).click();
    await expect(
      selector.getByRole("button", { name: "Configurar conexão", exact: true }),
    ).toBeVisible();
    await expect(page.locator(".context-ring .engine-mark")).toHaveAttribute(
      "data-provider",
      "pi",
    );
    await captureWindow(
      desktop,
      page,
      testInfo.outputPath("model-provider-native.png"),
    );
    await selector
      .getByRole("button", { name: "Fechar seleção de modelo", exact: true })
      .click();
    const original = await desktop.evaluate(({ BrowserWindow }) =>
      BrowserWindow.getAllWindows()[0]?.getBounds(),
    );
    await page.getByRole("tab", { name: "Browser", exact: true }).click();
    await page.getByRole("button", { name: /Ativar\s*Navegador/ }).click();
    await expect(
      page.getByRole("img", { name: "Tela do computador de trabalho" }),
    ).toBeVisible({ timeout: 30_000 });
    await page.getByRole("button", { name: "Tela cheia", exact: true }).click();
    await expect
      .poll(() =>
        desktop.evaluate(({ BrowserWindow }) =>
          BrowserWindow.getAllWindows()[0]?.isFullScreen(),
        ),
      )
      .toBe(true);
    await expect
      .poll(() => page.evaluate(() => document.fullscreenElement?.className))
      .toBe("computer-viewer");
    await captureWindow(
      desktop,
      page,
      testInfo.outputPath("browser-native-fullscreen.png"),
    );
    await page.keyboard.press("Escape");
    await expect
      .poll(() => page.evaluate(() => document.fullscreenElement === null))
      .toBe(true);
    await expect
      .poll(() =>
        desktop.evaluate(({ BrowserWindow }) =>
          BrowserWindow.getAllWindows()[0]?.getBounds(),
        ),
      )
      .toEqual(original);
    await expect(
      page.getByRole("tab", { name: "Browser", exact: true }),
    ).toHaveAttribute("aria-selected", "true");
    await page
      .locator("#bot-details")
      .getByRole("button", { name: "Fechar", exact: true })
      .click();
    const environment = page.getByRole("button", {
      name: /Selecionar ambiente[:：]\s*Meu Persona/,
    });
    await expect(environment).toBeVisible();
    const left = (await environment.boundingBox())?.x;
    const resize = page.getByRole("separator", {
      name: "Ajustar largura dos contatos",
    });
    await resize.focus();
    await resize.press("End");
    expect((await environment.boundingBox())?.x).toBe(left);
    const plus = await page
      .getByRole("button", { name: "Novo Bot", exact: true })
      .boundingBox();
    const environmentBox = await environment.boundingBox();
    expect(environmentBox && plus && plus.y < environmentBox.y).toBe(true);
    await resize.press("Home");
    await expect(resize).toHaveAttribute("aria-valuenow", "96");
    await expect
      .poll(async () => {
        const sidebar = await page
          .locator('.workspace-panel[data-side="sidebar"]')
          .boundingBox();
        const trigger = await environment.boundingBox();
        if (!sidebar || !trigger) throw new Error("Environment menu missing");
        return Math.abs(
          trigger.x + trigger.width / 2 - sidebar.x - sidebar.width / 2,
        );
      })
      .toBeLessThan(1);
    const menuShown = desktop.evaluate(
      ({ Menu }) =>
        new Promise<void>((resolve) => {
          const menu = Menu.getApplicationMenu()?.items[0]?.submenu;
          menu?.once("menu-will-show", () => resolve());
        }),
    );
    await environment.click();
    await menuShown;
    await expect(environment).toHaveAttribute("aria-expanded", "true");
    await desktop.evaluate(({ Menu }) =>
      Menu.getApplicationMenu()?.items[0]?.submenu?.closePopup(),
    );
    await expect(environment).toHaveAttribute("aria-expanded", "false");
    for (const theme of ["dark", "light"] as const) {
      await page.emulateMedia({ colorScheme: theme });
      await expect(page.locator("html")).toHaveAttribute("data-theme", theme);
      expect(
        await page.evaluate(() =>
          getComputedStyle(document.documentElement)
            .getPropertyValue("--color-surface")
            .trim(),
        ),
      ).toBe(theme === "dark" ? "#141416" : "#fafafa");
      await captureWindow(
        desktop,
        page,
        testInfo.outputPath(`environment-${theme}.png`),
      );
    }
    expect(
      await page.evaluate(() => ({
        require: typeof Reflect.get(window, "require"),
        setup: typeof Reflect.get(window, "personaSetup"),
      })),
    ).toEqual({ require: "undefined", setup: "undefined" });
    const restricted = await page.evaluate(() =>
      window.personaDesktop?.prepare(),
    );
    expect(restricted?.ok).toBe(false);
    await page
      .getByRole("button", { name: "Configurações", exact: true })
      .click();
    const settings = page.getByRole("main", {
      name: "Configurações",
      exact: true,
    });
    await expect(settings.getByRole("tab")).toHaveText([
      "Geral",
      "Provedores",
      "Modelos",
    ]);
    for (const label of ["Escuro", "Claro"]) {
      await settings
        .getByRole("combobox", { name: "Aparência", exact: true })
        .click();
      await page.getByRole("option", { name: label, exact: true }).click();
      const background = await desktop.evaluate(({ BrowserWindow }) =>
        BrowserWindow.getAllWindows()[0]?.getBackgroundColor(),
      );
      const rgb = background
        ?.match(/[a-f0-9]{2}/gi)
        ?.slice(0, 3)
        .map((part) => Number.parseInt(part, 16));
      await expect(page.locator(".sidebar")).toHaveCSS(
        "background-color",
        `rgb(${rgb?.join(", ")})`,
      );
    }
    const nativeMenu = await desktop.evaluate(({ Menu }) =>
      Menu.getApplicationMenu()?.items[0]?.submenu?.items.map(
        (item) => item.label,
      ),
    );
    expect(nativeMenu).toContain("Conectar a outro Persona…");
    await desktop.evaluate(({ BrowserWindow }) =>
      BrowserWindow.getAllWindows()[0]?.webContents.setZoomFactor(2),
    );
    await expect(settings.getByRole("tablist")).toHaveAttribute(
      "data-orientation",
      "horizontal",
    );
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await captureWindow(
      desktop,
      page,
      testInfo.outputPath("settings-zoom.png"),
    );
    await desktop.close();
    desktop = await playwright._electron.launch({ executablePath, args, env });
    const reopened = await desktop.firstWindow();
    await expect(
      reopened.getByRole("button", { name: "Persona", exact: true }),
    ).toBeVisible();
    expect(reopened.url()).toBe(`http://127.0.0.1:${ready.port}/`);
    await desktop.evaluate(({ BrowserWindow }) =>
      BrowserWindow.getAllWindows()[0]?.webContents.setZoomFactor(1),
    );
    await reopened.getByRole("button", { name: "Uso", exact: true }).click();
    await expect(
      reopened.getByRole("main", { name: "Uso", exact: true }),
    ).toContainText("Nenhum registro de uso neste período.");
    await reopened.getByRole("button", { name: "Uso", exact: true }).click();
    await expect(
      reopened.getByRole("button", { name: "Persona", exact: true }),
    ).toBeVisible();
    await reopened.locator('input[type="file"]').setInputFiles({
      name: "photo.png",
      mimeType: "image/png",
      buffer: Buffer.from(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aX1kAAAAASUVORK5CYII=",
        "base64",
      ),
    });
    await expect(
      reopened.locator(".composer-attachments .attachment"),
    ).toHaveCount(1);
    await reopened.getByLabel("Mensagem", { exact: true }).fill("图片查看");
    await reopened.getByRole("button", { name: "Enviar", exact: true }).click();
    await reopened
      .getByLabel("Sua mensagem", { exact: true })
      .getByRole("button", { name: "Ver imagem photo.png", exact: true })
      .click();
    const viewer = reopened.getByRole("dialog", { name: "photo.png" });
    await expect(viewer).toBeVisible();
    const frame = await viewer.boundingBox();
    expect(frame?.y).toBe(48);
    await captureWindow(
      desktop,
      reopened,
      testInfo.outputPath("image-viewer-titlebar.png"),
    );
    await reopened.keyboard.press("Escape");
    const chooser = await openConnectionSetup(desktop);
    await expect(
      chooser.getByRole("heading", { name: "Bem-vindo ao Persona" }),
    ).toBeVisible();
    await chooser
      .getByLabel("Conectar a um Persona existente", { exact: false })
      .check();
    await chooser
      .getByRole("button", { name: "Meu Persona", exact: true })
      .click();
    await expect(chooser.getByLabel("Endereço do Persona")).toHaveValue(
      `http://127.0.0.1:${ready.port}`,
    );
  } finally {
    await desktop.close();
    const exited = once(worker, "exit");
    worker.disconnect();
    await exited;
    await rm(directory, { recursive: true, force: true });
  }
});
