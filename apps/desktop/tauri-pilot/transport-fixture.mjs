import { writeFile } from "node:fs/promises";
import { installPrivateTransport } from "./service-transport.mjs";

installPrivateTransport();
if (process.argv[3] === "early-disconnect") {
  await new Promise((resolve) => {
    process.once("disconnect", resolve);
    process.send("initializing");
  });
}
let closing = false;
const close = async () => {
  if (closing) return;
  closing = true;
  // A real async cleanup boundary makes the historical duplicate-exit bug visible.
  await new Promise((resolve) => setImmediate(resolve));
  await writeFile(process.argv[2], "cleanup-complete", { flag: "wx" });
  if (process.connected) process.disconnect();
};
// Match both shutdown registrations in apps/api/src/main.ts.
process.on("message", (message) => {
  if (message === "shutdown") void close().then(() => process.exit());
});
process.parentPort.on("message", ({ data }) => {
  if (data === "shutdown") void close().then(() => process.exit());
  if (data === "pair-desktop")
    process.parentPort.postMessage("pairing-private");
});
if (process.connected) process.send("ready");
