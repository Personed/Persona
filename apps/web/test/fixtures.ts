import { type BrowserContext, test as base, expect } from "@playwright/test";

export type { FileChooser, Page, TestInfo } from "@playwright/test";
export { expect };

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
