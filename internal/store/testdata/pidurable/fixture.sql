PRAGMA foreign_keys=OFF;
BEGIN;
CREATE TABLE durable_schema (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			version INTEGER NOT NULL CHECK (version >= 0)
		) STRICT;
INSERT INTO "durable_schema" VALUES(1,2);
CREATE TABLE durable_metadata (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			next_id TEXT NOT NULL,
			next_seq INTEGER NOT NULL
		) STRICT;
INSERT INTO "durable_metadata" VALUES(1,'16',7);
CREATE TABLE record_ids (
			id INTEGER PRIMARY KEY,
			record_type TEXT NOT NULL CHECK (record_type IN ('conversation', 'entry', 'task', 'submission', 'document'))
		) STRICT;
INSERT INTO "record_ids" VALUES(2,'conversation');
INSERT INTO "record_ids" VALUES(3,'entry');
INSERT INTO "record_ids" VALUES(4,'document');
INSERT INTO "record_ids" VALUES(5,'document');
INSERT INTO "record_ids" VALUES(6,'task');
INSERT INTO "record_ids" VALUES(7,'submission');
INSERT INTO "record_ids" VALUES(8,'entry');
INSERT INTO "record_ids" VALUES(9,'document');
INSERT INTO "record_ids" VALUES(10,'conversation');
INSERT INTO "record_ids" VALUES(11,'document');
INSERT INTO "record_ids" VALUES(12,'submission');
INSERT INTO "record_ids" VALUES(13,'conversation');
INSERT INTO "record_ids" VALUES(14,'document');
INSERT INTO "record_ids" VALUES(15,'document');
CREATE TABLE conversations (
			id INTEGER PRIMARY KEY,
			owner_conversation_id INTEGER,
			owner_task_id INTEGER,
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT;
INSERT INTO "conversations" VALUES(2,NULL,NULL,'{"id":2}');
INSERT INTO "conversations" VALUES(10,NULL,NULL,'{"id":10,"parent":{"conversationId":2,"at":3}}');
INSERT INTO "conversations" VALUES(13,2,6,'{"id":13,"owner":{"conversationId":2,"taskId":6}}');
CREATE TABLE entries (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL,
			head INTEGER,
			commit_seq INTEGER NOT NULL,
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT;
INSERT INTO "entries" VALUES(3,2,NULL,1,'{"id":3,"conversationId":2,"kind":"user","data":{"text":"hi 🙂"}}');
INSERT INTO "entries" VALUES(8,2,8,3,'{"id":8,"conversationId":2,"kind":"assistant","model":[{"role":"assistant","content":"ok"}],"head":8,"byTaskId":6}');
CREATE TABLE submissions (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL,
			request_id TEXT,
			status TEXT NOT NULL CHECK (status IN ('queued', 'placed', 'done', 'unanswered')),
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT;
INSERT INTO "submissions" VALUES(7,2,'"req-1"','done','{"trace":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},"id":7,"conversationId":2,"requestId":"req-1","type":"input","status":"done","entry":3,"answer":8}');
INSERT INTO "submissions" VALUES(12,2,NULL,'unanswered','{"id":12,"conversationId":2,"type":"write","status":"unanswered","reason":"stopped","detail":{"by":"user"}}');
CREATE TABLE documents (
			id INTEGER PRIMARY KEY,
			kind TEXT NOT NULL,
			family INTEGER NOT NULL CHECK (family IN (0, 1)),
			key_value TEXT NOT NULL,
			scope_kind TEXT NOT NULL CHECK (scope_kind IN ('session', 'conversation', 'task')),
			owner_id INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			retired_at INTEGER,
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT;
INSERT INTO "documents" VALUES(4,'"persona.bots"',0,'""','session',0,1,NULL,'{"id":4,"kind":"persona.bots","scope":{"kind":"session"},"createdAt":1}');
INSERT INTO "documents" VALUES(5,'"notes"',1,'"weekly"','conversation',2,1,6,'{"id":5,"kind":"notes","key":"weekly","scope":{"kind":"conversation","conversationId":2},"history":"rewindable","fork":"asOf","createdAt":1,"retiredAt":6}');
INSERT INTO "documents" VALUES(9,'"persona.engine-input"',0,'""','task',6,3,4,'{"id":9,"kind":"persona.engine-input","scope":{"kind":"task","taskId":6},"createdAt":3,"retiredAt":4}');
INSERT INTO "documents" VALUES(11,'"notes"',1,'"weekly"','conversation',10,4,NULL,'{"id":11,"kind":"notes","key":"weekly","scope":{"kind":"conversation","conversationId":10},"history":"rewindable","fork":"asOf","createdAt":4}');
INSERT INTO "documents" VALUES(14,'"persona.engine"',0,'""','conversation',2,4,NULL,'{"id":14,"kind":"persona.engine","scope":{"kind":"conversation","conversationId":2},"history":"latest","fork":"initial","createdAt":4}');
INSERT INTO "documents" VALUES(15,'"notes"',1,'"weekly"','conversation',2,6,NULL,'{"id":15,"kind":"notes","key":"weekly","scope":{"kind":"conversation","conversationId":2},"history":"rewindable","fork":"asOf","createdAt":6}');
CREATE TABLE document_revisions (
			document_id INTEGER NOT NULL,
			seq INTEGER NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('base', 'delta')),
			version INTEGER NOT NULL,
			content TEXT NOT NULL CHECK (json_valid(content)),
			PRIMARY KEY (document_id, seq)
		) STRICT;
INSERT INTO "document_revisions" VALUES(5,1,'base',2,'{"items":[1,2],"text":"a","remove":true}');
INSERT INTO "document_revisions" VALUES(5,2,'delta',2,'[["s",["done"],false],["d",["remove"]],["p",["items"],1,0,[3]],["a",["text"],"b🙂"]]');
INSERT INTO "document_revisions" VALUES(5,3,'base',3,'{"items":[9]}');
INSERT INTO "document_revisions" VALUES(11,4,'base',2,'{"items":[1,3,2],"text":"ab🙂","done":false}');
INSERT INTO "document_revisions" VALUES(14,4,'base',1,'{"config":{"provider":"pi","model":null},"revision":0}');
INSERT INTO "document_revisions" VALUES(14,5,'delta',1,'[["s",["revision"],1]]');
INSERT INTO "document_revisions" VALUES(4,6,'base',2,'{"bots":[]}');
INSERT INTO "document_revisions" VALUES(15,6,'base',3,'{"items":[]}');
CREATE TABLE "tasks" (
			id INTEGER PRIMARY KEY,
			conversation_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'waiting', 'sleeping', 'completing', 'terminal')),
			abort_requested INTEGER NOT NULL CHECK (abort_requested IN (0, 1)),
			background INTEGER NOT NULL CHECK (background IN (0, 1)),
			record TEXT NOT NULL CHECK (json_valid(record))
		) STRICT;
INSERT INTO "tasks" VALUES(6,2,'"persona.turn"','terminal',1,0,'{"trace":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},"links":[{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}],"id":6,"conversationId":2,"kind":"persona.turn","version":1,"input":{"text":"hi"},"background":false,"abortRequested":true,"abortReason":"restart","abandonOnRestart":true,"startedAt":1700000000000,"endedAt":1700000005000,"restarts":0,"state":{"status":"terminal","outcome":{"status":"aborted","reason":"stopped","result":{"partial":true}}}}');
CREATE INDEX conversations_by_owner_conversation ON conversations (owner_conversation_id, id);
CREATE INDEX conversations_by_owner_task ON conversations (owner_task_id, id);
CREATE INDEX entries_by_conversation ON entries (conversation_id, id DESC);
CREATE INDEX entry_heads_by_conversation ON entries (conversation_id, id DESC) WHERE head IS NOT NULL;
CREATE INDEX submissions_by_request ON submissions (conversation_id, request_id);
CREATE INDEX submissions_by_conversation ON submissions (conversation_id, id);
CREATE INDEX submissions_by_status ON submissions (status, id);
CREATE INDEX documents_by_address
			ON documents (kind, scope_kind, owner_id, family, key_value, created_at DESC, retired_at);
CREATE INDEX documents_by_scope ON documents (scope_kind, owner_id, id);
CREATE INDEX documents_by_scope_kind ON documents (scope_kind, owner_id, kind, id);
CREATE INDEX document_revisions_by_kind ON document_revisions (document_id, kind, seq DESC);
CREATE INDEX tasks_by_status ON tasks (status, id);
CREATE INDEX tasks_by_conversation ON tasks (conversation_id, id);
CREATE INDEX tasks_by_kind ON tasks (kind, id);
CREATE INDEX tasks_by_abort_requested ON tasks (abort_requested, id);
CREATE INDEX tasks_by_background ON tasks (background, id);
COMMIT;
