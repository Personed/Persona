import assert from "node:assert/strict";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { PassThrough, Writable } from "node:stream";
import test from "node:test";
import { loadPreparationOperation } from "./prepare-operation.mjs";
import { runPreparationProtocol } from "./prepare-protocol.mjs";

function channel() {
  const input = new PassThrough();
  let output = "";
  const sink = new Writable({
    write(chunk, _encoding, done) {
      output += chunk.toString("utf8");
      done();
    },
  });
  return {
    input,
    sink,
    frames: () =>
      output
        .trim()
        .split("\n")
        .filter(Boolean)
        .map((line) => JSON.parse(line)),
  };
}

test("worker waits for start before loading or running the operation", async () => {
  const pipe = channel();
  let loaded = 0;
  let ran = 0;
  const result = runPreparationProtocol({
    input: pipe.input,
    output: pipe.sink,
    loadOperation: async () => {
      loaded += 1;
      return {
        run: async () => {
          ran += 1;
          return { image: "synthetic-work:test" };
        },
      };
    },
  });
  assert.deepEqual(pipe.frames(), [{ type: "ready" }]);
  pipe.input.write('{"type":"start"}\n');
  pipe.input.write('{"type":"start"}\n');
  assert.equal(await result, 0);
  assert.equal(loaded, 1);
  assert.equal(ran, 1);
  assert.deepEqual(pipe.frames().at(-1), {
    type: "result",
    ok: true,
    value: { image: "synthetic-work:test" },
  });
});

test("EOF and cancellation before start never load the operation", async () => {
  for (const action of ["eof", "cancel"]) {
    const pipe = channel();
    let loaded = 0;
    const result = runPreparationProtocol({
      input: pipe.input,
      output: pipe.sink,
      loadOperation: async () => {
        loaded += 1;
        return { run: async () => ({}) };
      },
    });
    if (action === "eof") pipe.input.end();
    else pipe.input.write('{"type":"cancel"}\n');
    await result;
    assert.equal(loaded, 0);
    assert.equal(pipe.frames()[0].type, "ready");
  }
});

test("cancellation aborts the running operation and is distinct", async () => {
  const pipe = channel();
  const result = runPreparationProtocol({
    input: pipe.input,
    output: pipe.sink,
    loadOperation: async () => ({
      run: async (signal) =>
        new Promise((_resolve, reject) => {
          signal.addEventListener(
            "abort",
            () => reject(new DOMException("Aborted", "AbortError")),
            { once: true },
          );
        }),
    }),
  });
  pipe.input.write('{"type":"start"}\n');
  await new Promise((resolve) => setImmediate(resolve));
  pipe.input.write('{"type":"cancel"}\n');
  assert.equal(await result, 2);
  assert.deepEqual(pipe.frames().at(-1), {
    type: "result",
    ok: false,
    cancelled: true,
    error: "Preparação cancelada.",
  });
});

test("preparation operation uses only the fixed release and synthetic executor", async () => {
  const directory = await mkdtemp(join(tmpdir(), "persona-prepare-release-"));
  try {
    const releaseFile = join(directory, "release.json");
    await writeFile(
      releaseFile,
      JSON.stringify({
        workImage: "synthetic-work:test",
        proxyImage: "synthetic-proxy:test",
        workBuild: {
          context: "work",
          dockerfile: "deploy/workspace.Dockerfile",
        },
        proxyBuild: {
          context: "work",
          dockerfile: "deploy/proxy.Dockerfile",
        },
      }),
    );
    const calls = [];
    const operation = await loadPreparationOperation({
      releaseFile,
      contextRoot: directory,
      execute: async (_command, args) => {
        calls.push(args);
        if (args[0] === "info") return '"linux"';
        if (
          args[0] === "image" &&
          args[1] === "inspect" &&
          args[2] === "synthetic-work:test"
        )
          throw new Error("missing synthetic image");
        return "";
      },
    });
    const value = await operation.run(new AbortController().signal, () => {});
    assert.deepEqual(value, {
      image: "synthetic-work:test",
      proxyImage: "synthetic-proxy:test",
    });
    assert.deepEqual(calls[0], ["info", "--format", "{{json .OSType}}"]);
    assert.deepEqual(
      calls.find((args) => args[0] === "build"),
      [
        "build",
        "-f",
        resolve(directory, "work/deploy/workspace.Dockerfile"),
        "-t",
        "synthetic-work:test",
        resolve(directory, "work"),
      ],
    );
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
});
