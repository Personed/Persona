import { resolve } from "node:path";
import { expect, test, vi } from "vitest";
import {
  prepareWorkImages,
  type Release,
  type runCommand,
} from "../src/install.ts";

const release: Release = {
  version: "synthetic",
  workImage: "synthetic-work:test",
  controllerEntry: "controller.ts",
  serverEntry: "server.ts",
  bridgeEntry: "bridge.ts",
  staticDir: "web",
};
const info = ["info", "--format", "{{json .OSType}}"];

test("work images verify Linux and reuse existing work and proxy images", async () => {
  const run = vi.fn<typeof runCommand>(async (_command, args) =>
    args[0] === "info" ? '"linux"' : "",
  );
  const progress = vi.fn();
  const signal = new AbortController().signal;
  await prepareWorkImages(
    { ...release, proxyImage: "synthetic-proxy:test" },
    progress,
    signal,
    run,
  );
  expect(run.mock.calls).toEqual(
    [
      info,
      ["image", "inspect", release.workImage],
      ["image", "inspect", "synthetic-proxy:test"],
    ].map((args) => ["docker", args, { signal, timeout: 30_000 }]),
  );
  expect(progress.mock.calls.map(([value]) => value.phase)).toEqual([
    "checking",
    "image",
  ]);
});

test.each([true, false])(
  "missing work and proxy images use build when available, otherwise pull (build: %s)",
  async (build) => {
    const context = "synthetic-context";
    const recipe = { context, dockerfile: "Dockerfile.work" };
    const run = vi.fn<typeof runCommand>(async (_command, args) => {
      if (args[0] === "info") return '"linux"';
      if (args[0] === "image") throw new Error("synthetic missing image");
      return "";
    });
    const signal = new AbortController().signal;
    await prepareWorkImages(
      {
        ...release,
        proxyImage: "synthetic-proxy:test",
        ...(build ? { workBuild: recipe, proxyBuild: recipe } : {}),
      },
      () => {},
      signal,
      run,
    );
    expect(run.mock.calls).toEqual([
      ["docker", info, { signal, timeout: 30_000 }],
      ...[release.workImage, "synthetic-proxy:test"].flatMap((image) => [
        ["docker", ["image", "inspect", image], { signal, timeout: 30_000 }],
        [
          "docker",
          build
            ? [
                "build",
                "-f",
                resolve(context, recipe.dockerfile),
                "-t",
                image,
                context,
              ]
            : ["pull", image],
          { signal, timeout: 1_200_000 },
        ],
      ]),
    ]);
  },
);

test.each(["unavailable", "windows", "invalid JSON"])(
  "Docker verification failure prevents image preparation: %s",
  async (failure) => {
    const run = vi.fn<typeof runCommand>(async () => {
      if (failure === "unavailable") throw new Error("synthetic unavailable");
      return failure === "windows" ? '"windows"' : "invalid JSON";
    });
    const progress = vi.fn();
    await expect(
      prepareWorkImages(release, progress, undefined, run),
    ).rejects.toThrow(
      failure === "unavailable"
        ? "Não foi possível conectar ao Docker"
        : failure === "windows"
          ? "Linux"
          : /JSON/,
    );
    expect(run).toHaveBeenCalledExactlyOnceWith("docker", info, {
      signal: undefined,
      timeout: 30_000,
    });
    expect(progress.mock.calls.map(([value]) => value.phase)).toEqual([
      "checking",
    ]);
  },
);

test.each(["build", "pull"])(
  "failed %s stops before preparing the proxy",
  async (operation) => {
    const failure = new Error("synthetic preparation failure");
    const run = vi.fn<typeof runCommand>(async (_command, args) => {
      if (args[0] === "info") return '"linux"';
      if (args[0] === "image") throw new Error("synthetic missing image");
      throw failure;
    });
    await expect(
      prepareWorkImages(
        {
          ...release,
          proxyImage: "synthetic-proxy:test",
          ...(operation === "build"
            ? { workBuild: { context: "synthetic", dockerfile: "Dockerfile" } }
            : {}),
        },
        () => {},
        undefined,
        run,
      ),
    ).rejects.toBe(failure);
    expect(run.mock.calls.map(([, args]) => args[0])).toEqual([
      "info",
      "image",
      operation,
    ]);
  },
);

test.each(["before", "info", "inspect"])(
  "abort at %s prevents the next preparation step",
  async (at) => {
    const controller = new AbortController();
    const reason = new Error("synthetic cancellation");
    const run = vi.fn<typeof runCommand>(async (_command, args) => {
      if (at === "info" || args[0] === "image") {
        controller.abort(reason);
        throw reason;
      }
      return '"linux"';
    });
    if (at === "before") controller.abort(reason);
    await expect(
      prepareWorkImages(
        {
          ...release,
          workBuild: { context: "synthetic", dockerfile: "Dockerfile" },
          proxyImage: "synthetic-proxy:test",
        },
        () => {},
        controller.signal,
        run,
      ),
    ).rejects.toBe(reason);
    expect(run.mock.calls.map(([, args]) => args[0])).toEqual(
      at === "before" ? [] : at === "info" ? ["info"] : ["info", "image"],
    );
  },
);
