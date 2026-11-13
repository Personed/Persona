import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { createServer } from "node:http";
import test from "node:test";
import { fileURLToPath } from "node:url";

test("direct session verification ignores environment proxy with fictitious credential", {
  timeout: 20_000,
}, async () => {
  let proxyRequests = 0;
  let directRequests = 0;
  const proxy = createServer((_req, res) => {
    proxyRequests++;
    res.writeHead(502).end();
  });
  proxy.on("connect", (_req, socket) => {
    proxyRequests++;
    socket.end();
  });
  const local = createServer((req, res) => {
    directRequests++;
    assert.equal(req.url, "/hosting/session");
    assert.equal(
      req.headers.authorization,
      "Bearer pilot-fictitious-session-only",
    );
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ device: { scope: "admin" } }));
  });
  proxy.listen(0, "127.0.0.1");
  local.listen(0, "127.0.0.1");
  await Promise.all([once(proxy, "listening"), once(local, "listening")]);
  try {
    const proxyUrl = `http://fictitious-user:fictitious-password@127.0.0.1:${proxy.address().port}`;
    const env = {
      NODE_USE_ENV_PROXY: "1",
      HTTP_PROXY: proxyUrl,
      HTTPS_PROXY: proxyUrl,
      http_proxy: proxyUrl,
      https_proxy: proxyUrl,
      NO_PROXY: "",
      no_proxy: "",
    };
    // Read only known OS bootstrap keys; never enumerate inherited values.
    for (const key of ["PATH", "SystemRoot", "WINDIR", "SystemDrive"])
      if (process.env[key]) env[key] = process.env[key];
    const child = spawn(
      process.execPath,
      [fileURLToPath(new URL("./local-session.fixture.mjs", import.meta.url))],
      {
        env,
        stdio: ["pipe", "pipe", "pipe"],
      },
    );
    const exited = once(child, "exit");
    let output = "";
    child.stdout.on("data", (chunk) => {
      output += chunk;
    });
    child.stderr.resume();
    child.stdin.end(
      `${JSON.stringify({ origin: `http://127.0.0.1:${local.address().port}` })}\n`,
    );
    assert.deepEqual(await exited, [0, null]);
    assert.equal(output, "verified\n");
    assert.equal(directRequests, 1);
    assert.equal(proxyRequests, 0);
  } finally {
    proxy.closeAllConnections();
    local.closeAllConnections();
    await Promise.all([
      new Promise((resolve) => proxy.close(resolve)),
      new Promise((resolve) => local.close(resolve)),
    ]);
  }
});
