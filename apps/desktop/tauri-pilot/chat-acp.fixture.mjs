import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { createRequire } from "node:module";
import { createInterface } from "node:readline";
import { portable } from "./layout.mjs";

const require = createRequire(
  new URL(
    portable
      ? "./runtime/package.json"
      : "../../../packages/agent/package.json",
    import.meta.url,
  ),
);
const { Client } = require("@modelcontextprotocol/sdk/client/index.js");
const {
  StreamableHTTPClientTransport,
} = require("@modelcontextprotocol/sdk/client/streamableHttp.js");
const send = (frame) => process.stdout.write(`${JSON.stringify(frame)}\n`);
let sessionId;
let tools = [];
let client;

// Test-only ACP peer: real product discovery, deterministic text, no model or tool mutations.
for await (const line of createInterface({ input: process.stdin })) {
  const { id, method, params } = JSON.parse(line);
  if (id === undefined) continue;
  try {
    let result;
    if (method === "initialize") {
      assert.equal(params.protocolVersion, 1);
      result = {
        protocolVersion: 1,
        agentCapabilities: { mcpCapabilities: { http: true } },
        agentInfo: { name: "portable-synthetic", version: "1.0.0" },
        authMethods: [],
      };
    } else if (method === "session/new") {
      assert.equal(params.mcpServers.length, 1);
      const server = params.mcpServers[0];
      assert.equal(server.name, "persona");
      assert.equal(server.type, "http");
      const url = new URL(server.url);
      assert.equal(url.protocol, "http:");
      assert.equal(url.hostname, "127.0.0.1");
      client = new Client({ name: "portable-synthetic", version: "1.0.0" });
      await client.connect(new StreamableHTTPClientTransport(url));
      tools = (await client.listTools()).tools;
      sessionId = randomUUID();
      result = { sessionId };
    } else if (method === "session/prompt") {
      assert.equal(params.sessionId, sessionId);
      assert.ok(tools.some((tool) => tool.name === "conversation_read"));
      const text = params.prompt.map((block) => block.text ?? "").join("\n");
      const marker = text.match(/portable-chat-[a-f0-9-]{36}/)?.[0];
      assert.ok(marker, "Missing synthetic turn marker");
      send({
        jsonrpc: "2.0",
        method: "session/update",
        params: {
          sessionId,
          update: {
            sessionUpdate: "agent_message_chunk",
            content: {
              type: "text",
              text: `Synthetic reply: ${marker}; Persona product tools discovered.`,
            },
          },
        },
      });
      result = { stopReason: "end_turn" };
    } else {
      throw new Error(`Unsupported synthetic ACP method: ${method}`);
    }
    send({ jsonrpc: "2.0", id, result });
  } catch (error) {
    send({
      jsonrpc: "2.0",
      id,
      error: { code: -32603, message: error.message },
    });
  }
}
await client?.close();
