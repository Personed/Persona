CREATE TABLE store_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    seq INTEGER NOT NULL,
    last_id INTEGER NOT NULL
) STRICT;
INSERT INTO store_state VALUES (1, 0, 0);

CREATE TABLE record_ids (
    id INTEGER PRIMARY KEY CHECK (id > 0),
    kind TEXT NOT NULL
) STRICT;

CREATE TABLE conversations (
    id INTEGER PRIMARY KEY REFERENCES record_ids(id),
    parent_id INTEGER NOT NULL,
    fork_at INTEGER NOT NULL,
    owner_conversation_id INTEGER NOT NULL,
    owner_task_id INTEGER NOT NULL,
    record TEXT NOT NULL
) STRICT;
CREATE INDEX conversations_by_owner ON conversations(owner_conversation_id, owner_task_id, id);
CREATE INDEX conversations_by_owner_task ON conversations(owner_task_id, id);

CREATE TABLE entries (
    id INTEGER PRIMARY KEY REFERENCES record_ids(id),
    conversation_id INTEGER NOT NULL,
    seq INTEGER NOT NULL,
    head INTEGER,
    record TEXT NOT NULL
) STRICT;
CREATE INDEX entries_by_conversation ON entries(conversation_id, id);
CREATE INDEX entries_by_head ON entries(conversation_id, id) WHERE head IS NOT NULL;

CREATE TABLE tasks (
    id INTEGER PRIMARY KEY REFERENCES record_ids(id),
    conversation_id INTEGER NOT NULL,
    kind TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending','running','waiting','sleeping','completing','terminal')),
    abort_requested INTEGER NOT NULL,
    background INTEGER NOT NULL,
    record TEXT NOT NULL
) STRICT;
CREATE INDEX tasks_by_conversation ON tasks(conversation_id, id);
CREATE INDEX tasks_by_status ON tasks(status, id);
CREATE INDEX tasks_to_reconcile ON tasks(id) WHERE status <> 'terminal';

CREATE TABLE submissions (
    id INTEGER PRIMARY KEY REFERENCES record_ids(id),
    conversation_id INTEGER NOT NULL,
    request_id TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued','placed','done','unanswered')),
    record TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX submissions_by_request ON submissions(conversation_id, request_id) WHERE request_id <> '';
CREATE INDEX submissions_by_conversation ON submissions(conversation_id, id);

-- Bots retain the persona.bots session document, the same representation used by pi-durable.
CREATE TABLE documents (
    id INTEGER PRIMARY KEY REFERENCES record_ids(id),
    kind TEXT NOT NULL,
    key TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_id INTEGER NOT NULL,
    history TEXT NOT NULL,
    fork TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    retired_at INTEGER
) STRICT;
CREATE UNIQUE INDEX documents_current ON documents(scope_kind, scope_id, kind, key) WHERE retired_at IS NULL;
CREATE INDEX documents_by_scope ON documents(scope_kind, scope_id, id);
CREATE TABLE document_revisions (
    document_id INTEGER REFERENCES documents(id),
    seq INTEGER,
    version INTEGER NOT NULL,
    value TEXT NOT NULL,
    PRIMARY KEY (document_id, seq)
) STRICT, WITHOUT ROWID;

CREATE TABLE call_journal (
    task_id INTEGER REFERENCES tasks(id),
    call_id TEXT,
    state TEXT NOT NULL CHECK (state IN ('intent','complete','uncertain')),
    intent TEXT NOT NULL,
    result TEXT CHECK ((state = 'complete') = (result IS NOT NULL)),
    PRIMARY KEY (task_id, call_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX journal_pending ON call_journal(task_id, call_id) WHERE state = 'intent';
