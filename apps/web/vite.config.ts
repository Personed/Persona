import { cp, mkdir } from "node:fs/promises";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

let output = fileURLToPath(new URL("./dist", import.meta.url));

export default defineConfig({
  plugins: [
    {
      name: "distribution-notices",
      configResolved(config) {
        output = resolve(config.root, config.build.outDir);
      },
      async closeBundle() {
        const source = fileURLToPath(new URL("../../deploy", import.meta.url));
        await mkdir(output, { recursive: true });
        await cp(`${source}/NOTICE`, `${output}/NOTICE`);
        await cp(`${source}/licenses`, `${output}/licenses`, {
          recursive: true,
        });
      },
    },
  ],
  server: {
    proxy: {
      "/hosting": "http://127.0.0.1:4310",
      "/rpc": "http://127.0.0.1:4310",
      "/files": "http://127.0.0.1:4310",
    },
  },
});
