import { existsSync } from "node:fs";
import { join } from "node:path";

export function ensureDockerPath() {
  if (process.platform !== "win32") return;
  const candidates = [
    process.env.LOCALAPPDATA
      ? join(
          process.env.LOCALAPPDATA,
          "Programs",
          "DockerDesktop",
          "resources",
          "bin",
        )
      : "",
    process.env.ProgramFiles
      ? join(process.env.ProgramFiles, "Docker", "Docker", "resources", "bin")
      : "",
    "C:\\Program Files\\Docker\\Docker\\resources\\bin",
  ].filter((dir): dir is string => Boolean(dir) && existsSync(dir));

  for (const dir of candidates) {
    const current = (process.env.PATH ?? "").split(";");
    if (!current.includes(dir)) {
      process.env.PATH = `${dir};${process.env.PATH ?? ""}`;
    }
  }
}
