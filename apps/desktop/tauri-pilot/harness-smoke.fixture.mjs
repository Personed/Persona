import { writeFile } from "node:fs/promises";
import { join } from "node:path";

export default (pi) =>
  pi.registerCommand("pilot-smoke", {
    handler: async (_args, ctx) => {
      await writeFile(
        join(ctx.cwd, "synthetic-task.txt"),
        "isolated native harness completed",
      );
      ctx.ui.notify("pilot synthetic task completed", "info");
    },
  });
