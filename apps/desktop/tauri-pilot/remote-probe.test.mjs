import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { createServer } from "node:http";
import { createInterface } from "node:readline";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { hostingInstance, portable } from "./layout.mjs";

const { serverOrigin } = await import(hostingInstance.href);

function worker() {
  const env = {};
  for (const key of ["PATH", "SystemRoot", "WINDIR", "SystemDrive"])
    if (process.env[key]) env[key] = process.env[key];
  const child = spawn(
    process.execPath,
    [
      // Packaged workers must resolve every dependency without reading the checkout.
      ...(portable
        ? [
            "--permission",
            "--no-warnings",
            `--allow-fs-read=${fileURLToPath(new URL("./", import.meta.url))}`,
            ...(process.allowedNodeEnvironmentFlags.has("--allow-net")
              ? ["--allow-net"]
              : []),
          ]
        : []),
      fileURLToPath(new URL("./remote-probe.mjs", import.meta.url)),
    ],
    {
      env,
      stdio: ["pipe", "pipe", "pipe"],
    },
  );
  let stdout = "";
  child.stdout.on("data", (chunk) => {
    stdout += chunk;
  });
  const lines = createInterface({ input: child.stderr })[
    Symbol.asyncIterator
  ]();
  const exited = once(child, "exit");
  const send = (value) => child.stdin.write(`${JSON.stringify(value)}\n`);
  return {
    send,
    next: async () => {
      const line = await lines.next();
      assert.equal(line.done, false);
      return JSON.parse(line.value);
    },
    stop: async () => {
      send({ type: "stop" });
      assert.deepEqual(await exited, [0, null]);
      assert.equal(stdout, "");
    },
  };
}

test("shared remote policy allows HTTPS and only loopback HTTP", () => {
  assert.equal(serverOrigin("example.invalid/path"), "https://example.invalid");
  for (const host of ["localhost", "127.0.0.1", "[::1]"])
    assert.equal(
      serverOrigin(`http://${host}:1234/path`),
      `http://${host}:1234`,
    );
  for (const url of [
    "http://example.invalid",
    "https://user:private@example.invalid",
    "https://example.invalid?q=private",
    "https://example.invalid#private",
  ])
    assert.throws(() => serverOrigin(url));
});

test("real private worker probes controlled server, rejects redirects/schema/errors, and cancels pending fetch", {
  timeout: 30_000,
}, async () => {
  let mode = "ready";
  let requests = 0;
  let began;
  const server = createServer((req, res) => {
    requests++;
    assert.equal(req.url, "/hosting/info");
    assert.equal(req.headers.authorization, undefined);
    assert.equal(req.headers.cookie, undefined);
    if (mode === "slow") return began();
    if (mode === "redirect") {
      res.writeHead(302, { location: "/unexpected" });
      return res.end();
    }
    if (mode === "error") {
      res.writeHead(503);
      return res.end();
    }
    res.setHeader("content-type", "application/json");
    res.end(
      JSON.stringify({
        product: mode === "schema" ? "other" : "persona",
        version: "test",
      }),
    );
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const origin = `http://127.0.0.1:${server.address().port}`;
  try {
    for (mode of ["ready", "redirect", "schema", "error", "slow"]) {
      const control = worker();
      const started = new Promise((resolve) => {
        began = resolve;
      });
      try {
        control.send({
          type: "start",
          value: {
            mode: "existing",
            url: `${origin}/discarded/path`,
            name: " Controlled ",
          },
        });
        if (mode === "slow") {
          await started;
          await control.stop();
          assert.deepEqual(await control.next(), {
            type: "stopped",
            mode: "graceful",
            cleanup: true,
          });
        } else {
          const result = await control.next();
          if (mode === "ready")
            assert.deepEqual(result, { type: "ready", origin });
          else assert.equal(result.type, "failed");
          await control.stop();
        }
      } catch (error) {
        await control.stop();
        throw error;
      }
    }
    assert.equal(requests, 5, "redirect must not be followed");
    for (const value of [
      ...[
        "http://example.invalid",
        `${origin}?private`,
        `${origin}#private`,
      ].map((url) => ({ mode: "existing", url, name: "Controlled" })),
      {
        mode: "existing",
        url: `http://user:private@127.0.0.1:${server.address().port}`,
        name: "Controlled",
      },
      { mode: "existing", url: origin, name: " " },
      { mode: "existing", url: origin, name: "x".repeat(81) },
    ]) {
      const control = worker();
      control.send({ type: "start", value });
      const result = await control.next();
      assert.equal(result.type, "failed");
      assert.equal(JSON.stringify(result).includes("private"), false);
      await control.stop();
    }
    assert.equal(requests, 5, "invalid input must not reach the network");
  } finally {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
});
