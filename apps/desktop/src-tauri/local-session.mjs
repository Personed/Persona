import { Agent, request } from "node:http";

export async function verifyLocalSession(origin, token) {
  const url = new URL(origin);
  if (
    url.protocol !== "http:" ||
    url.hostname !== "127.0.0.1" ||
    !url.port ||
    url.pathname !== "/" ||
    url.search ||
    url.hash ||
    url.username ||
    url.password
  )
    throw new Error("Invalid loopback origin");
  // Never use fetch/globalAgent: Node can initialize both from environment proxies.
  const agent = new Agent({ proxyEnv: {} });
  try {
    await new Promise((resolve, reject) => {
      const req = request(
        new URL("/hosting/session", url),
        {
          agent,
          signal: AbortSignal.timeout(10_000),
          headers: { authorization: `Bearer ${token}` },
        },
        (response) => {
          let size = 0;
          const chunks = [];
          response.on("error", reject);
          response.on("data", (chunk) => {
            size += chunk.length;
            if (size > 64 * 1024) {
              response.destroy();
              reject(new Error("Invalid session response"));
            } else chunks.push(chunk);
          });
          response.on("end", () => {
            try {
              if (
                response.statusCode !== 200 ||
                JSON.parse(Buffer.concat(chunks).toString("utf8")).device
                  ?.scope !== "admin"
              )
                throw new Error("Pairing verification failed");
              resolve();
            } catch {
              reject(new Error("Pairing verification failed"));
            }
          });
        },
      );
      req.on("error", reject);
      req.end();
    });
  } finally {
    agent.destroy();
  }
}
