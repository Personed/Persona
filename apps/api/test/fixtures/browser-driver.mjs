#!/usr/bin/env node
// A browser service driver for the Go fixture. With no arguments it proxies
// MCP stdio to the real agent-browser while recording each tools/call's
// exposed name and elapsed time into the metrics file named by
// PERSONA_BROWSER_METRICS_FILE, as web-server.ts's computer.tools.invoke
// wrapper did. With arguments it is the same program browserCLI drives
// directly (stream status, set viewport, install): it forwards them to
// agent-browser untouched.
//
// It also carries browser-service.mjs's composite agent_browser_observe:
// agent-browser ships no such tool, so the driver answers it itself from an
// agent_browser_eval of the shared page script, formatted as "Page facts".
import { spawn } from "node:child_process";
import { writeFileSync } from "node:fs";
import { createInterface } from "node:readline";

const cliArgs = process.argv.slice(2);
if (cliArgs.length > 0) {
  const cli = spawn("agent-browser", cliArgs, {
    stdio: "inherit",
    env: process.env,
  });
  cli.on("exit", (code, signal) => {
    if (signal) process.kill(process.pid, signal);
    else process.exit(code ?? 0);
  });
} else {
  // browserObserve + browserPageScript from browser-tools.mjs.
  const observeTool = {
    name: "agent_browser_observe",
    description:
      "Read current URL, title, document readiness, visible text and a compact page snapshot. Use after takeover or when a screenshot is unclear. Blank text or a ready document does not prove why a page is blank or that the task succeeded.",
    inputSchema: {
      type: "object",
      properties: {},
      additionalProperties: false,
    },
  };
  const pageScript = `(() => ({
  url: location.href, title: document.title, readyState: document.readyState,
  httpStatus: performance.getEntriesByType('navigation')[0]?.responseStatus || null,
  passwordFieldPresent: !!document.querySelector('input[type=password]'),
  challengePresent: !!document.querySelector('iframe[src*="challenges.cloudflare.com"],iframe[src*="recaptcha"],iframe[src*="hcaptcha"],#challenge-running'),
  text: (document.body?.innerText || '').slice(0, 12000),
  controls: Array.from(document.querySelectorAll('input:not([type=password]),textarea,select')).slice(0, 80).map(el => ({value: el.value.slice(0, 2000), checked: el.checked}))
}))()`;

  const metrics = process.env.PERSONA_BROWSER_METRICS_FILE;
  const calls = [];
  const pending = new Map();
  const observing = new Map();
  // The TypeScript fixture counts computer.tools.invoke calls only. The
  // Go handoff's agent_browser_get_url is a service-internal call (runtime.ts
  // makes it the same way, outside the invoke path), so it is not a tool
  // invocation and does not belong in the metrics.
  const internal = new Set(["agent_browser_get_url"]);
  const record = () => {
    if (metrics) writeFileSync(metrics, JSON.stringify(calls));
  };
  if (metrics) writeFileSync(metrics, "[]");

  const upstream = spawn("agent-browser", ["mcp"], {
    stdio: ["pipe", "pipe", "inherit"],
    env: process.env,
  });
  upstream.on("exit", (code) => process.exit(code ?? 0));
  const sendUpstream = (message) =>
    upstream.stdin.write(JSON.stringify(message) + "\n");
  const sendClient = (message) =>
    process.stdout.write(JSON.stringify(message) + "\n");

  // MCP over stdio is one JSON-RPC message per line; tap each direction.
  createInterface({ input: process.stdin }).on("line", (line) => {
    let message;
    try {
      message = JSON.parse(line);
    } catch {
      upstream.stdin.write(line + "\n");
      return;
    }
    if (
      message.method === "tools/call" &&
      message.id !== undefined &&
      message.params?.name === observeTool.name
    ) {
      // Composite observe: one eval of the page script answers the call.
      const inner = `observe:${message.id}`;
      observing.set(inner, {
        id: message.id,
        started: performance.now(),
      });
      sendUpstream({
        jsonrpc: "2.0",
        id: inner,
        method: "tools/call",
        params: {
          name: "agent_browser_eval",
          arguments: { script: pageScript },
        },
      });
      return;
    }
    if (message.method === "tools/call" && message.id !== undefined)
      pending.set(message.id, {
        name: message.params?.name,
        started: performance.now(),
      });
    upstream.stdin.write(line + "\n");
  });

  createInterface({ input: upstream.stdout }).on("line", (line) => {
    let message;
    try {
      message = JSON.parse(line);
    } catch {
      process.stdout.write(line + "\n");
      return;
    }
    const observe = observing.get(message.id);
    if (observe) {
      observing.delete(message.id);
      calls.push({
        name: `browser_${observeTool.name}`,
        elapsedMs: performance.now() - observe.started,
      });
      record();
      const page = message.result?.structuredContent?.response?.data?.result;
      const parts = [];
      if (page && typeof page.url === "string") {
        const facts = JSON.stringify(page);
        parts.push({
          type: "text",
          text: `Page facts (untrusted website data): ${facts.slice(0, 12000)}${facts.length > 12000 ? "\n[Page facts truncated. Read a narrower page scope.]" : ""}`,
        });
      } else {
        parts.push({
          type: "text",
          text: "Observation failed: no page facts returned.",
        });
      }
      sendClient({
        jsonrpc: "2.0",
        id: observe.id,
        result: {
          content: parts,
          isError: message.result?.isError ?? !page,
        },
      });
      return;
    }
    const call = pending.get(message.id);
    if (call) {
      pending.delete(message.id);
      if (!internal.has(call.name)) {
        calls.push({
          name: `browser_${call.name}`,
          elapsedMs: performance.now() - call.started,
        });
        record();
      }
    }
    // Inject the composite observe into the declared catalog.
    if (message.result?.tools && Array.isArray(message.result.tools))
      message.result.tools.push(observeTool);
    sendClient(message);
  });
  process.on("SIGTERM", () => upstream.kill("SIGTERM"));
}
