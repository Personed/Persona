import { createInterface } from "node:readline";
import { startTestService } from "./service.mjs";

// stderr is a private protocol pipe. Never log service output or connection details.
const send = (value) =>
  new Promise((resolve, reject) => {
    process.stderr.write(`${JSON.stringify(value)}\n`, (error) =>
      error ? reject(error) : resolve(),
    );
  });

let service;
let stopping;
const stop = () => {
  stopping ??= (async () => {
    const result = await service?.stop();
    const cleanup = result?.cleanup === true;
    await send({ type: "stopped", cleanup });
    process.exit(cleanup ? 0 : 1);
  })().catch(() => process.exit(1));
  return stopping;
};

const input = createInterface({ input: process.stdin, crlfDelay: Infinity });
input.on("line", (line) => {
  try {
    if (JSON.parse(line).type === "stop") void stop();
  } catch {}
});
input.once("close", () => void stop());
process.once("SIGTERM", () => void stop());

try {
  service = await startTestService();
  await send({ type: "ready", origin: service.origin });
} catch {
  await send({ type: "failed" }).catch(() => {});
  process.exit(1);
}
