import { createInterface } from "node:readline";
import { z } from "zod";
import { probeInstance } from "../../../packages/hosting/src/instance.ts";

// stderr is a private pipe. Never log input URLs, names or fetch error objects.
const send = (value) => process.stderr.write(`${JSON.stringify(value)}\n`);
const input = z.object({
  mode: z.literal("existing"),
  url: z.string().min(1),
  name: z.string().trim().min(1).max(80),
});
const controller = new AbortController();
let starting;
let stopping;
const stop = () => {
  controller.abort();
  if (stopping) return stopping;
  stopping = (async () => {
    await starting?.catch(() => {});
    send({ type: "stopped", mode: "graceful", cleanup: true });
    process.exit(0);
  })();
  return stopping;
};
const lines = createInterface({ input: process.stdin, crlfDelay: Infinity });
lines.on("line", (line) => {
  try {
    const message = JSON.parse(line);
    if (message.type === "stop") return stop();
    if (message.type !== "start" || starting || controller.signal.aborted)
      throw new Error("Invalid command");
    starting = (async () => {
      const value = input.parse(message.value);
      const result = await probeInstance(value.url, controller.signal);
      controller.signal.throwIfAborted();
      send({ type: "ready", origin: result.origin });
    })().catch((error) => {
      if (!controller.signal.aborted) {
        const message = [
          "远程连接需要 HTTPS；本机可以使用 HTTP。",
          "无法连接 Persona。",
        ].includes(error.message)
          ? error.message
          : "Não foi possível conectar ao Persona. Verifique o endereço e tente novamente.";
        send({ type: "failed", message });
      }
    });
  } catch {
    send({ type: "failed", message: "Dados de conexão inválidos." });
    stop();
  }
});
lines.once("close", stop);
process.once("SIGTERM", stop);
process.once("SIGINT", stop);
