import { runMaintenanceProtocol } from "./maintenance-protocol.mjs";

process.exitCode = await runMaintenanceProtocol({
  loadOperation: async () =>
    (await import("./maintenance-operation.mjs")).runMaintenanceOperation,
});
