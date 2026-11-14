export async function stopOwnedChild(
  child,
  exited,
  gracefulMs = 30_000,
  termMs = 5_000,
) {
  if (child.exitCode !== null || child.signalCode !== null)
    return child.exitCode === 0 && child.signalCode === null;
  if (child.connected) child.send("shutdown", () => {});
  let force;
  let escalated = false;
  const timer = setTimeout(() => {
    escalated = true;
    child.kill("SIGTERM");
    force = setTimeout(() => child.kill("SIGKILL"), termMs);
  }, gracefulMs);
  try {
    const [code, signal] = await exited;
    return !escalated && code === 0 && signal === null;
  } finally {
    clearTimeout(timer);
    clearTimeout(force);
  }
}
