import { test as base, expect } from "@playwright/test";

export type { FileChooser, Page, TestInfo } from "@playwright/test";
export { expect };

export const test = base.extend({
  context: async ({ context }, use) => {
    await context.addInitScript(() => {
      try {
        localStorage.setItem("persona:language:v1", "zh-CN");
      } catch {}
    });
    await use(context);
  },
});
