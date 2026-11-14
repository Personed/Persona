import { spawn } from "node:child_process";

process.on("SIGTERM", () => {});
process.stdin.once("data", (bytes) => {
  const { phase, marker } = JSON.parse(bytes.toString());
  const writer = spawn(
    process.execPath,
    [
      "--input-type=module",
      "-e",
      `
    import { writeFileSync } from 'node:fs';
    process.on('SIGTERM', () => {});
    writeFileSync(process.argv[1], 'writer-started');
    console.log('started');
    setInterval(() => writeFileSync(process.argv[1], 'writer-alive'), 20);
  `,
      marker,
    ],
    {
      stdio: ["ignore", "pipe", phase === "leader-exit" ? "inherit" : "ignore"],
    },
  );
  writer.stdout.once("data", () => {
    if (phase === "leader-exit") {
      process.stderr.write(
        `${JSON.stringify({ type: "fixture-live", pid: writer.pid })}\n`,
        () => process.exit(0),
      );
      return;
    }
    process.stderr.write(
      `${JSON.stringify({ type: "fixture-live", pid: writer.pid })}\n`,
    );
    if (phase === "after") process.stderr.write('{"type":"ready"}\n');
    // A partial frame exercises a blocked read_until, not just an empty pipe.
    process.stderr.write('{"type":');
  });
});
setInterval(() => {}, 1000);
