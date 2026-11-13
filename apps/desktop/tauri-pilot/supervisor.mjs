import { createInterface } from "node:readline";
import { verifyLocalSession } from "./local-session.mjs";
import { startTestService } from "./service.mjs";

// stderr is exclusively an inherited private protocol pipe, never a log sink.
const send = (message) => process.stderr.write(`${JSON.stringify(message)}\n`);
let starting;
let stopping;
let closing = false;
const stop = () => {
  closing = true;
  stopping ??= (async () => {
    const service = await starting?.catch(() => undefined);
    const outcome = service
      ? await service.stop()
      : { graceful: true, cleanup: false };
    send({
      type: "stopped",
      mode: outcome.graceful ? "graceful" : "forced",
      cleanup: outcome.cleanup,
    });
    process.exit(outcome.graceful ? 0 : 1);
  })().catch(() => {
    send({ type: "failed", phase: "shutdown", cleanup: false });
    process.exit(1);
  });
  return stopping;
};
const start = () => {
  starting ??= startTestService();
  return starting;
};
let announced = false;
const input = createInterface({ input: process.stdin, crlfDelay: Infinity });
input.on("line", async (line) => {
  try {
    if (line.length > 1024) throw new Error("Invalid frame");
    const message = JSON.parse(line);
    if (message.type === "stop") return void stop();
    if (message.type !== "start" || closing) throw new Error("Invalid command");
    if (announced) return;
    announced = true;
    const service = await start();
    service.exited.then(() => {
      if (!closing) {
        send({ type: "died" });
        void stop();
      }
    });
    const session = await service.pair();
    // Readiness combines the owned child's startup and its private pairing reply.
    await verifyLocalSession(service.origin, session.token);
    if (!closing)
      send({
        type: "ready",
        origin: service.origin,
        servicePid: service.pid,
        ...session,
      });
  } catch {
    send({ type: "failed" });
    void stop();
  }
});
input.once("close", () => void stop());
process.once("SIGTERM", () => void stop());
process.once("SIGINT", () => void stop());
process.on("uncaughtException", () => void stop());
process.on("unhandledRejection", () => void stop());
