import { readFile } from "node:fs/promises";
import { resolve } from "node:path";
import { buildContextRoot, releaseEntry } from "./layout.mjs";

export async function loadPreparationOperation({
  releaseFile = releaseEntry,
  contextRoot = buildContextRoot,
  execute,
} = {}) {
  const { prepareWorkImages } = await import(
    "../../../packages/hosting/src/install.ts"
  );
  let release;
  try {
    release = JSON.parse(await readFile(releaseFile, "utf8"));
    if (
      !release ||
      typeof release !== "object" ||
      typeof release.workImage !== "string" ||
      !release.workImage
    )
      throw new Error("invalid release");
  } catch {
    throw new Error("Não foi possível preparar as imagens do Docker.");
  }
  const normalized = { ...release };
  for (const key of ["workBuild", "proxyBuild", "serviceBuild"]) {
    const build = normalized[key];
    if (build && typeof build.context === "string")
      normalized[key] = {
        ...build,
        context: resolve(contextRoot, build.context),
      };
  }
  return {
    async run(signal, progress) {
      await prepareWorkImages(normalized, progress, signal, execute);
      return {
        image: normalized.workImage,
        ...(typeof normalized.proxyImage === "string"
          ? { proxyImage: normalized.proxyImage }
          : {}),
      };
    },
  };
}
