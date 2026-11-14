// The native owner supplies confirmed paths and credentials only over its private pipe.
export async function runMaintenanceProtocol({
  input = process.stdin,
  output = process.stderr,
  loadOperation,
}) {
  const send = (frame) => output.write(`${JSON.stringify(frame)}\n`);
  send({ type: "ready" });
  let frame = Buffer.alloc(0);
  try {
    for await (const chunk of input) {
      frame = Buffer.concat([frame, Buffer.from(chunk)]);
      if (frame.length > 65536) throw new Error("invalid frame");
      const end = frame.indexOf(10);
      if (end === -1) continue;
      if (end !== frame.length - 1) throw new Error("invalid frame");
      const request = JSON.parse(frame.subarray(0, end).toString("utf8"));
      frame.fill(0);
      if (
        request?.type !== "start" ||
        !["backup", "restore"].includes(request.action) ||
        typeof request.archive !== "string" ||
        !request.archive ||
        request.archive.includes("\0") ||
        typeof request.password !== "string" ||
        request.password.length < 12 ||
        (request.runtime !== undefined &&
          !["native", "docker"].includes(request.runtime)) ||
        Object.keys(request).some(
          (key) =>
            !["type", "action", "archive", "password", "runtime"].includes(key),
        )
      )
        throw new Error("invalid request");
      // Existing maintenance APIs are not cancellable. Always await their real outcome.
      const operation = await loadOperation();
      await operation(request);
      send({ type: "result", ok: true });
      return 0;
    }
  } catch {
    // Never echo native errors: they can contain paths, credentials or archive contents.
  } finally {
    frame.fill(0);
  }
  send({ type: "result", ok: false, error: "A manutenção não foi concluída." });
  return 1;
}
