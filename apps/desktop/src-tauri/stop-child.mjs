export async function stopOwnedChild(
  child,
  exited,
  gracefulMs = 30_000,
  termMs = 5_000,
  ipc = true,
) {
  if (child.exitCode !== null || child.signalCode !== null)
    return child.exitCode === 0 && child.signalCode === null;
  if (ipc) {
    if (child.connected) child.send("shutdown", () => {});
  } else {
    // The Go service has no IPC channel: SIGTERM is its graceful path; on
    // Windows, which has no signals, closing its stdin ends it gracefully.
    try {
      child.stdin?.end();
    } catch {}
    if (process.platform !== "win32") child.kill("SIGTERM");
  }
  let force;
  let escalated = false;
  const timer = setTimeout(() => {
    escalated = true;
    child.kill(ipc ? "SIGTERM" : "SIGKILL");
    if (ipc) force = setTimeout(() => child.kill("SIGKILL"), termMs);
  }, gracefulMs);
  try {
    const [code, signal] = await exited;
    return !escalated && code === 0 && signal === null;
  } finally {
    clearTimeout(timer);
    clearTimeout(force);
  }
}
