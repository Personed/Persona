import { runPreparationProtocol } from "./prepare-protocol.mjs";

const code = await runPreparationProtocol({
  loadOperation: async () =>
    (await import("./prepare-operation.mjs")).loadPreparationOperation(),
});
process.exitCode = code;
