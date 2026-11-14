import assert from "node:assert/strict";
import test from "node:test";
import { stopOwnedChild } from "./stop-child.mjs";

test("ignored shutdown and SIGTERM escalate to SIGKILL, and wait for confirmed exit", {
  timeout: 2000,
}, async () => {
  const signals = [];
  let confirmExit;
  let killed;
  const requestedKill = new Promise((resolve) => {
    killed = resolve;
  });
  const exited = new Promise((resolve) => {
    confirmExit = resolve;
  });
  const child = {
    exitCode: null,
    signalCode: null,
    connected: true,
    send: (message, callback) => {
      signals.push(message);
      callback();
    },
    kill: (signal) => {
      signals.push(signal);
      if (signal === "SIGKILL") killed();
    },
  };
  let settled = false;
  const stopping = stopOwnedChild(child, exited, 10, 10).then((graceful) => {
    settled = true;
    return graceful;
  });
  await requestedKill;
  assert.deepEqual(signals, ["shutdown", "SIGTERM", "SIGKILL"]);
  assert.equal(settled, false);
  confirmExit([null, "SIGKILL"]);
  assert.equal(await stopping, false);
  assert.equal(settled, true);
});
