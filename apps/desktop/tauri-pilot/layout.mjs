import { existsSync } from "node:fs";

export const portable = existsSync(
  new URL("./portable-test.json", import.meta.url),
);
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
