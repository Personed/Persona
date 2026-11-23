// Regenerate with: node internal/store/testdata/pidurable/generate.mjs
// Writes fixture.sql, a pi-durable database built by the installed pi-durable,
// and expected.json, the document values pi-durable reads back from it.
import { createRequire } from "node:module";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const require = createRequire(join(here, "../../../../packages/agent/package.json"));
const { openNodeSqliteStorage } = await import(
  pathToFileURL(require.resolve("@earendil-works/pi-durable/storage/sqlite/node")).href
);

const dir = mkdtempSync(join(tmpdir(), "pidurable-"));
const path = join(dir, "agent.sqlite");
const storage = await openNodeSqliteStorage(path);
const context = {};
const trace = { traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" };
const commit = async (...batches) => {
  const result = await storage.commit(batches, () => {}, context);
  if (result.refused !== undefined) throw result.refused;
};

await commit([
  { type: "conversation", value: { id: 2 } },
  { type: "entry", value: { id: 3, conversationId: 2, kind: "user", data: { text: "hi 🙂" } } },
  {
    type: "document.create",
    record: { id: 4, kind: "persona.bots", scope: { kind: "session" } },
    content: { kind: "base", version: 1, value: { bots: [1] } },
  },
  {
    type: "document.create",
    record: {
      id: 5, kind: "notes", key: "weekly", scope: { kind: "conversation", conversationId: 2 },
      history: "rewindable", fork: "asOf",
    },
    content: { kind: "base", version: 2, value: { items: [1, 2], text: "a", remove: true } },
  },
]);
await commit([
  {
    type: "task",
    value: {
      trace, links: [trace], id: 6, conversationId: 2, kind: "persona.turn", version: 1,
      input: { text: "hi" }, background: false, abortRequested: false, startedAt: 1700000000000,
      state: { status: "running", checkpoint: { step: 1 } }, memos: { tool: { ok: true } },
    },
  },
  {
    type: "submission",
    value: { trace, id: 7, conversationId: 2, requestId: "req-1", type: "input", status: "placed", entry: 3 },
  },
  {
    type: "document.change", id: 5,
    content: { kind: "delta", version: 2, ops: [["s", ["done"], false], ["d", ["remove"]], ["p", ["items"], 1, 0, [3]], ["a", ["text"], "b🙂"]] },
  },
  { type: "document.change", id: 4, content: { kind: "delta", version: 1, ops: [["p", ["bots"], 1, 0, [2]]] } },
]);
await commit([
  {
    type: "entry",
    value: { id: 8, conversationId: 2, kind: "assistant", model: [{ role: "assistant", content: "ok" }], head: 8, byTaskId: 6 },
  },
  {
    type: "task",
    value: {
      trace, links: [trace], id: 6, conversationId: 2, kind: "persona.turn", version: 1,
      input: { text: "hi" }, background: false, abortRequested: true, abortReason: "restart",
      abandonOnRestart: true, startedAt: 1700000000000, endedAt: 1700000005000, restarts: 0,
      state: { status: "terminal", outcome: { status: "aborted", reason: "stopped", result: { partial: true } } },
    },
  },
  {
    type: "submission",
    value: { trace, id: 7, conversationId: 2, requestId: "req-1", type: "input", status: "done", entry: 3, answer: 8 },
  },
  { type: "document.change", id: 5, content: { kind: "base", version: 3, value: { items: [9] } } },
  {
    type: "document.create",
    record: { id: 9, kind: "persona.engine-input", scope: { kind: "task", taskId: 6 } },
    content: { kind: "base", version: 1, value: { status: "queued", owner: null } },
  },
]);
await commit([
  { type: "conversation", value: { id: 10, parent: { conversationId: 2, at: 3 } } },
  {
    type: "document.copy",
    record: {
      id: 11, kind: "notes", key: "weekly", scope: { kind: "conversation", conversationId: 10 },
      history: "rewindable", fork: "asOf",
    },
    source: { id: 5, at: 2 },
  },
  { type: "document.retire", id: 9 },
  {
    type: "submission",
    value: { id: 12, conversationId: 2, type: "write", status: "unanswered", reason: "stopped", detail: { by: "user" } },
  },
  { type: "conversation", value: { id: 13, owner: { conversationId: 2, taskId: 6 } } },
  {
    type: "document.create",
    record: {
      id: 14, kind: "persona.engine", scope: { kind: "conversation", conversationId: 2 },
      history: "latest", fork: "initial",
    },
    content: { kind: "base", version: 1, value: { config: { provider: "pi", model: null }, revision: 0 } },
  },
]);
await commit(
  [{ type: "document.change", id: 14, content: { kind: "delta", version: 1, ops: [["s", ["revision"], 1]] } }],
  [
    { type: "document.change", id: 4, content: { kind: "base", version: 2, value: { bots: [] } } },
    { type: "document.retire", id: 5 },
    {
      type: "document.create",
      record: {
        id: 15, kind: "notes", key: "weekly", scope: { kind: "conversation", conversationId: 2 },
        history: "rewindable", fork: "asOf",
      },
      content: { kind: "base", version: 3, value: { items: [] } },
    },
  ],
);

const db = new DatabaseSync(path, { readOnly: true });
const lastSeq = Number(db.prepare("SELECT next_seq FROM durable_metadata").get().next_seq) - 1;
const expected = { lastSeq, documents: {} };
for (const { id } of db.prepare("SELECT id FROM documents ORDER BY id").all()) {
  const read = async (at) => {
    try {
      const stored = await storage.document(id, at, context);
      return stored === undefined ? null : { version: stored.version, value: stored.value };
    } catch {
      return "unavailable";
    }
  };
  const doc = { current: await read("current"), at: {} };
  for (let seq = 1; seq <= lastSeq; seq++) doc.at[seq] = await read(seq);
  expected.documents[id] = doc;
}
await storage.close(context);

const quote = (value) => {
  if (value === null) return "NULL";
  if (typeof value === "number" || typeof value === "bigint") return String(value);
  return `'${String(value).replaceAll("'", "''")}'`;
};
const lines = ["PRAGMA foreign_keys=OFF;", "BEGIN;"];
const objects = db
  .prepare("SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY type = 'index', rowid")
  .all();
for (const { type, name, sql } of objects) {
  lines.push(`${sql};`);
  if (type !== "table") continue;
  for (const row of db.prepare(`SELECT * FROM "${name}"`).all()) {
    lines.push(`INSERT INTO "${name}" VALUES(${Object.values(row).map(quote).join(",")});`);
  }
}
lines.push("COMMIT;");
db.close();
rmSync(dir, { recursive: true });
writeFileSync(join(here, "fixture.sql"), `${lines.join("\n")}\n`);
writeFileSync(join(here, "expected.json"), `${JSON.stringify(expected, null, 2)}\n`);
