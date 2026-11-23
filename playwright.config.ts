import { defineConfig } from "@playwright/test";
import { goGapPatterns } from "./apps/web/test/go-gaps.ts";

export default defineConfig({
  testDir: "./apps/web/test",
  testMatch: "**/*.spec.ts",
  grepInvert:
    process.env.PERSONA_WEB_SERVER === "go" ? goGapPatterns : undefined,
  workers: 1,
  reporter: [["list"], ["junit", { outputFile: "test-results/web.xml" }]],
  timeout: 30_000,
  use: {
    browserName: "chromium",
    viewport: { width: 1360, height: 900 },
    trace: "retain-on-failure",
  },
});
