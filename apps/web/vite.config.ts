import { cp, mkdir } from "node:fs/promises";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig, type ProxyOptions } from "vite";

const api = "http://127.0.0.1:4310";

// The hosting origin guard only accepts the API origin. Present requests from
// the dev page itself as same-origin; other origins still reach the guard.
const proxy: ProxyOptions = {
  target: api,
  changeOrigin: true,
  configure(server) {
    server.on("proxyReq", (proxyReq, req) => {
      if (req.headers.origin === `http://${req.headers.host}`)
        proxyReq.setHeader("origin", api);
    });
  },
};

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
      "/hosting": proxy,
      "/rpc": proxy,
      "/files": proxy,
    },
  },
});
