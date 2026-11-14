import { createInterface } from "node:readline";

const maxFrame = 1024;
const maxMessage = 512;

function boundedText(value, fallback) {
  if (typeof value !== "string" || !value) return fallback;
  return value.slice(0, maxMessage);
}

export async function runPreparationProtocol({
  input = process.stdin,
  output = process.stderr,
  loadOperation,
}) {
  const controller = new AbortController();
  const lines = createInterface({ input, crlfDelay: Infinity });
  let started = false;
  let settled = false;

  const send = (frame) => {
    output.write(`${JSON.stringify(frame)}\n`);
  };

  return new Promise((resolve) => {
    const finish = (code, result) => {
      if (settled) return;
      settled = true;
      if (result) send(result);
      lines.close();
      resolve(code);
    };

    send({ type: "ready" });
    lines.on("line", (line) => {
      if (settled || line.length > maxFrame) {
        finish(1, {
          type: "result",
          ok: false,
          error: "Comando de preparação inválido.",
        });
        return;
      }
      let message;
      try {
        message = JSON.parse(line);
      } catch {
        finish(1, {
          type: "result",
          ok: false,
          error: "Comando de preparação inválido.",
        });
        return;
      }
      if (message?.type === "cancel") {
        controller.abort();
        if (!started) {
          finish(2, {
            type: "result",
            ok: false,
            cancelled: true,
            error: "Preparação cancelada.",
          });
        }
        return;
      }
      if (message?.type !== "start") {
        finish(1, {
          type: "result",
          ok: false,
          error: "Comando de preparação inválido.",
        });
        return;
      }
      if (started) return;
      started = true;
      Promise.resolve()
        .then(loadOperation)
        .then((operation) =>
          operation.run(controller.signal, (progress) =>
            send({
              type: "progress",
              phase: boundedText(progress?.phase, "working"),
              message: boundedText(
                progress?.message,
                "Preparando as imagens do Docker…",
              ),
            }),
          ),
        )
        .then((value) => finish(0, { type: "result", ok: true, value }))
        .catch((error) => {
          if (controller.signal.aborted || error?.name === "AbortError") {
            finish(2, {
              type: "result",
              ok: false,
              cancelled: true,
              error: "Preparação cancelada.",
            });
            return;
          }
          finish(1, {
            type: "result",
            ok: false,
            error: boundedText(
              error?.message,
              "Não foi possível preparar as imagens do Docker.",
            ),
          });
        });
    });
    lines.once("close", () => {
      if (settled) return;
      controller.abort();
      finish(1, {
        type: "result",
        ok: false,
        error: "Preparação interrompida.",
      });
    });
  });
}
