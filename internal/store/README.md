# SQLite store

Open the Go runtime with `store.Open(ctx, filepath.Join(dataDir, "persona-go.sqlite"))`
and close it during shutdown. It leases the same `runtime.sqlite` in the
directory as the TypeScript service and backup code. The Go store uses a
separate SQLite database; it does not open or migrate a pi-durable database in
place. The one-shot importer owns copying legacy records.

- Each successful batch commits once and advances the sequence. On failure,
  earlier batches stay committed and later batches do not run. Entry records
  are immutable. Scans follow ID order, include fork ancestors up to each fork
  point, and return at most 200 records.
- Put a submission and its effects in the same batch. A duplicate nonempty
  request ID in the same conversation rejects the batch; read the original with
  `SubmissionByRequest`. A later write to that submission may update its state
  without changing its request ID or conversation.
- Bot metadata stays in the `persona.bots` session document. Rewindable
  conversation documents retain historical values. Other documents retain only
  the latest value. Commit sequences identify revisions; `Version` is the
  document schema version, which defaults to 1 and is preserved on normal
  updates. The importer can supply a schema version with `CommitVersioned`.
  A copy takes the source's pre-batch value and version. Changes accept a
  base object or decoded Chord delta tuples.
- Register a watch before taking the initial snapshot. Watches receive one
  sequence per committed batch or journal change, including when consumers
  fall behind. Cancel the watch context when done.

## Runtime recovery

On open, the store marks tool intents without results as `uncertain`. Before
accepting new work, page through `RecoveryTasks` and reconcile interrupted
running or completing tasks against their checkpoints, the engine, and
`Calls`. Waiting and sleeping tasks keep their original states. Storage never
starts a task or repeats a tool call.

Record a task-scoped call ID and its intent with `BeginCall` before invoking the
tool. Dispatch only if it returns `true`. A repeated ID returns its existing
record; return its saved result or surface the missing result without replay.
Persist the confirmed tool result with `CompleteCall`, including tool failures
that return a result. Settling an uncertain call requires confirmed evidence,
not a second execution.
