import { createInterface } from "node:readline";
import { startPilotDockerService } from "./docker-service.mjs";
import { verifyLocalSession } from "./local-session.mjs";
import { startPilotService, startTestService } from "./service.mjs";

// stderr is exclusively an inherited private protocol pipe, never a log sink.
const send = (message) => process.stderr.write(`${JSON.stringify(message)}\n`);
let starting;
let stopping;
let closing = false;
let runtime = "native";
const startAbort = new AbortController();
const stop = () => {
  closing = true;
  startAbort.abort();
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
const start = (runtime, persistent) => {
  starting ??=
    runtime === "docker"
      ? startPilotDockerService({
          signal: startAbort.signal,
          progress: (value) => send({ type: "progress", ...value }),
        })
      : persistent
        ? startPilotService()
        : startTestService();
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
    if (
      message.persistent !== undefined &&
      typeof message.persistent !== "boolean"
    )
      throw new Error("Invalid profile mode");
    runtime = message.runtime ?? "native";
    if (runtime !== "native" && runtime !== "docker")
      throw new Error("Invalid runtime");
    const service = await start(runtime, message.persistent === true);
    service.exited?.then(() => {
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
        ...(service.pid ? { servicePid: service.pid } : {}),
        ...session,
      });
  } catch (error) {
    const message =
      runtime !== "docker"
        ? undefined
        : error?.code === "EEXIST"
          ? "O perfil Docker está ativo ou bloqueado por um estado anterior não confirmado."
          : error instanceof AggregateError
            ? "Não foi possível confirmar a parada dos contêineres; o perfil permanece bloqueado."
            : typeof error?.message === "string" &&
                (error.message.startsWith(
                  "Não foi possível conectar ao Docker",
                ) ||
                  error.message.startsWith("Alterne o Docker") ||
                  error.message.startsWith("Arquivos de instalação") ||
                  error.message.startsWith("A etapa de instalação") ||
                  error.message.startsWith("Conexão com a área de trabalho"))
              ? error.message
              : "Não foi possível iniciar o perfil Docker isolado. Verifique o Docker Desktop e tente novamente.";
    send({ type: "failed", ...(message ? { message } : {}) });
    void stop();
  }
});
input.once("close", () => void stop());
process.once("SIGTERM", () => void stop());
process.once("SIGINT", () => void stop());
process.on("uncaughtException", () => void stop());
process.on("unhandledRejection", () => void stop());
