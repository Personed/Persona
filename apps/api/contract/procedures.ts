import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { z } from "zod";
import { createRouter } from "../src/router.ts";

// Error codes raised by helpers that handlers call by name.
// `error.code` forwards a WorkError, whose codes are fixed in work/state.ts.
const workErrors = ["NOT_FOUND", "CONFLICT", "BAD_REQUEST"];
const helpers: Record<string, string[]> = {
  botConversation: ["NOT_FOUND"],
  requireConfiguredModel: ["PRECONDITION_FAILED"],
  prepareMaterials: ["NOT_FOUND", "CONFLICT", "BAD_REQUEST"],
  watchValues: ["SERVICE_UNAVAILABLE"],
};
const sources = ["computer", "engines", "files", "mcp", "models", "work"];
const local = Object.fromEntries(
  sources.map((name) => [
    name,
    localHelpers(
      readFileSync(
        fileURLToPath(new URL(`../src/${name}.ts`, import.meta.url)),
        "utf8",
      ),
    ),
  ]),
);

function codesIn(text: string) {
  const codes = new Set<string>();
  for (const match of text.matchAll(/ORPCError\(\s*"([A-Z_]+)"/g))
    codes.add(match[1] as string);
  if (/ORPCError\(\s*(error\.code|\n\s*error instanceof WorkError)/.test(text))
    for (const code of workErrors) codes.add(code);
  return codes;
}

function localHelpers(source: string) {
  const result: Record<string, Set<string>> = {};
  const lines = source.split("\n");
  for (const [index, line] of lines.entries()) {
    const match = line.match(
      /^(\s+)(?:async function (\w+)|const (\w+) = async)/,
    );
    if (!match) continue;
    const indent = match[1] as string;
    const end = lines.findIndex(
      (next, at) => at > index && next.startsWith(`${indent}}`),
    );
    const body = lines.slice(index, end + 1).join("\n");
    const codes = codesIn(body);
    for (const [name, list] of Object.entries(helpers))
      if (new RegExp(`\\b${name}\\(`).test(body))
        for (const code of list) codes.add(code);
    result[(match[2] ?? match[3]) as string] = codes;
  }
  return result;
}

function handlerErrors(handler: string, module: string | undefined) {
  const codes = codesIn(handler);
  const known = { ...helpers, ...(module ? local[module] : {}) };
  for (const [name, list] of Object.entries(known))
    if (new RegExp(`\\b${name}\\(`).test(handler))
      for (const code of list) codes.add(code);
  return [...codes].sort();
}

type Procedure = {
  "~orpc": {
    inputSchema?: z.ZodType;
    handler: (...args: never[]) => unknown;
  };
};

function isProcedure(value: unknown): value is Procedure {
  return typeof value === "object" && value !== null && "~orpc" in value;
}

export function procedures() {
  const router = createRouter(
    ...([
      {},
      {},
      {},
      {},
      {},
      {},
      {},
      "http://127.0.0.1/mcp/oauth/callback",
    ] as unknown as Parameters<typeof createRouter>),
  );
  const list: Array<{
    path: string;
    kind: "unary" | "stream";
    input: unknown;
    errors: string[];
  }> = [];
  const visit = (node: object, path: string[]) => {
    for (const [key, value] of Object.entries(node)) {
      const next = [...path, key];
      if (isProcedure(value)) {
        const { handler, inputSchema } = value["~orpc"];
        list.push({
          path: next.join("."),
          kind:
            handler.constructor.name === "AsyncGeneratorFunction"
              ? "stream"
              : "unary",
          input: inputSchema
            ? z.toJSONSchema(inputSchema, {
                io: "input",
                unrepresentable: "any",
              })
            : null,
          errors: [
            ...new Set([
              ...(inputSchema ? ["BAD_REQUEST"] : []),
              ...handlerErrors(handler.toString(), path[0] ?? "models"),
              "INTERNAL_SERVER_ERROR",
            ]),
          ].sort(),
        });
      } else if (typeof value === "object" && value !== null)
        visit(value, next);
    }
  };
  visit(router, []);
  return list.sort((a, b) => a.path.localeCompare(b.path));
}
