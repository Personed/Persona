export function installPrivateTransport() {
  if (!process.send || !process.connected)
    throw new Error("Private IPC required");
  const emit = process.emit;
  let pending = false;
  let delivered = false;
  let ready = false;
  const shutdown = () => {
    if (delivered) return;
    if (!ready) {
      pending = true;
      return;
    }
    delivered = true;
    emit.call(process, "message", "shutdown");
  };
  // Node IPC uses emit; gate shutdown until main.ts installs its async handler.
  process.emit = function (event, ...args) {
    if (event === "message" && args[0] === "shutdown") {
      shutdown();
      return true;
    }
    return emit.call(this, event, ...args);
  };
  process.once("disconnect", shutdown);
  process.parentPort = {
    postMessage: (message) => {
      if (process.connected) process.send(message);
    },
    on: (event, listener) => {
      if (event === "message") {
        process.on("message", (data) => {
          // main.ts already handles shutdown on process.message, never duplicate it.
          if (data !== "shutdown") listener({ data });
        });
        // main.ts registers its process.message shutdown handler before parentPort.
        ready = true;
        if (pending) queueMicrotask(shutdown);
      }
    },
  };
}
