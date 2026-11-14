import { existsSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

export const installed = existsSync(
  new URL("./installer.json", import.meta.url),
);
export const dataDirectory = installed
  ? join(
      process.env.LOCALAPPDATA ??
        (() => {
          throw new Error("LOCALAPPDATA unavailable");
        })(),
      "app.persona.desktop",
    )
  : fileURLToPath(new URL("./target/test-data/", import.meta.url));

export const portable =
  installed || existsSync(new URL("./portable-test.json", import.meta.url));
export const runtimeDirectory = portable
  ? fileURLToPath(new URL("./runtime/", import.meta.url))
  : fileURLToPath(new URL("../../../", import.meta.url));
export const releaseEntry = portable
  ? new URL("./runtime/release.json", import.meta.url)
  : new URL("../../../deploy/release.json", import.meta.url);
export const buildContextRoot = runtimeDirectory;
export const runtimeEntry = new URL(
  portable ? "./runtime/server.mjs" : "../../api/src/main.ts",
  import.meta.url,
);
export const staticDirectory = new URL(
  portable ? "./runtime/web/" : "../../web/dist/",
  import.meta.url,
);
export const hostingInstance = new URL(
  portable
    ? "./runtime/hosting-instance.mjs"
    : "../../../packages/hosting/src/instance.ts",
  import.meta.url,
);
