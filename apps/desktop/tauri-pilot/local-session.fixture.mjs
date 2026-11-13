import { verifyLocalSession } from "./local-session.mjs";

process.stdin.once("data", async (bytes) => {
  try {
    await verifyLocalSession(
      JSON.parse(bytes.toString()).origin,
      "pilot-fictitious-session-only",
    );
    process.stdout.write("verified\n");
  } catch {
    process.exitCode = 1;
  }
});
