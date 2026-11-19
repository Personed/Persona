// Package store defines the durable record store that replaces pi-durable's Storage.
//
// The records mirror pi-durable 1.5 (dist/types.d.ts) so the G2 importer can copy
// an existing SQLite database without translation. IDs are minted per store and
// are unique across kinds; Seq orders every committed write.
package store

import (
	"context"
	"encoding/json"
	"errors"
)

// IDs share one numeric space, minted by Store.MintID.
type (
	// ConversationID identifies a Conversation.
	ConversationID int64
	// EntryID identifies an Entry and orders entries within a conversation.
	EntryID int64
	// TaskID identifies a Task.
	TaskID int64
	// SubmissionID identifies a Submission.
	SubmissionID int64
	// DocumentID identifies a Document.
	DocumentID int64
	// Seq orders committed batches.
	Seq int64
)

// ErrNotFound reports a missing record. Reads return it instead of a nil record.
var ErrNotFound = errors.New("store: not found")

// Conversation is one durable transcript. Parent is set on forks, Owner on task-owned conversations.
type Conversation struct {
	ID     ConversationID `json:"id"`
	Parent *ForkPoint     `json:"parent,omitempty"`
	Owner  *TaskOwner     `json:"owner,omitempty"`
}

// ForkPoint is the entry a forked conversation branches from.
type ForkPoint struct {
	ConversationID ConversationID `json:"conversationId"`
	At             EntryID        `json:"at"`
}

// TaskOwner names the task that owns a conversation.
type TaskOwner struct {
	ConversationID ConversationID `json:"conversationId"`
	TaskID         TaskID         `json:"taskId"`
}

// Entry is one transcript record. Model holds model-visible messages; Data holds product data.
type Entry struct {
	ID             EntryID         `json:"id"`
	ConversationID ConversationID  `json:"conversationId"`
	Kind           string          `json:"kind"`
	Model          json.RawMessage `json:"model,omitempty"`
	Data           json.RawMessage `json:"data,omitempty"`
	Head           *EntryID        `json:"head,omitempty"`
	Edits          []ContextEdit   `json:"edits,omitempty"`
	ByTaskID       *TaskID         `json:"byTaskId,omitempty"`
}

// ContextEdit omits or replaces the model messages of an earlier entry.
type ContextEdit struct {
	Target   EntryID         `json:"target"`
	Action   string          `json:"action"` // "omit" or "replace"
	Messages json.RawMessage `json:"messages,omitempty"`
}

// TaskStatus is the lifecycle state of a durable task.
type TaskStatus string

// Task states, in lifecycle order.
const (
	TaskPending    TaskStatus = "pending"
	TaskRunning    TaskStatus = "running"
	TaskWaiting    TaskStatus = "waiting"
	TaskSleeping   TaskStatus = "sleeping"
	TaskCompleting TaskStatus = "completing"
	TaskTerminal   TaskStatus = "terminal"
)

// Task is one durable unit of work with a checkpoint and, once settled, an outcome.
type Task struct {
	ID             TaskID                     `json:"id"`
	ConversationID ConversationID             `json:"conversationId"`
	Kind           string                     `json:"kind"`
	Version        int                        `json:"version"`
	Input          json.RawMessage            `json:"input"`
	Background     bool                       `json:"background"`
	AbortRequested bool                       `json:"abortRequested"`
	State          TaskState                  `json:"state"`
	Memos          map[string]json.RawMessage `json:"memos,omitempty"`
}

// TaskState holds the fields used by Status: Checkpoint while live, On/Policy while
// waiting, Until (Unix ms) while sleeping, Outcome once completing or terminal.
type TaskState struct {
	Status     TaskStatus      `json:"status"`
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`
	On         []TaskID        `json:"on,omitempty"`
	Policy     json.RawMessage `json:"policy,omitempty"`
	Until      int64           `json:"until,omitempty"`
	Outcome    *TaskOutcome    `json:"outcome,omitempty"`
}

// TaskOutcome is "completed" with a Result or "failed"/"aborted" with an Error.
type TaskOutcome struct {
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *OutcomeError   `json:"error,omitempty"`
}

// OutcomeError describes a failed task.
type OutcomeError struct {
	Message string          `json:"message"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// SubmissionStatus is the admission state of one user input.
type SubmissionStatus string

// Submission states.
const (
	SubmissionQueued     SubmissionStatus = "queued"
	SubmissionPlaced     SubmissionStatus = "placed"
	SubmissionDone       SubmissionStatus = "done"
	SubmissionUnanswered SubmissionStatus = "unanswered"
)

// Submission is one admitted input. RequestID deduplicates retries within a conversation.
type Submission struct {
	ID             SubmissionID     `json:"id"`
	ConversationID ConversationID   `json:"conversationId"`
	RequestID      string           `json:"requestId,omitempty"`
	Type           string           `json:"type"`
	Status         SubmissionStatus `json:"status"`
	Entry          *EntryID         `json:"entry,omitempty"`
	Answer         *EntryID         `json:"answer,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	Detail         json.RawMessage  `json:"detail,omitempty"`
	Payload        json.RawMessage  `json:"payload,omitempty"`
}

// Scope is where a document lives: "session", "conversation" or "task".
type Scope struct {
	Kind           string         `json:"kind"`
	ConversationID ConversationID `json:"conversationId,omitempty"`
	TaskID         TaskID         `json:"taskId,omitempty"`
}

// Document is a versioned JSON object. History is "latest" or "rewindable" for conversation scope.
type Document struct {
	ID        DocumentID `json:"id"`
	Kind      string     `json:"kind"`
	Key       string     `json:"key,omitempty"`
	Scope     Scope      `json:"scope"`
	History   string     `json:"history,omitempty"`
	Fork      string     `json:"fork,omitempty"`
	CreatedAt Seq        `json:"createdAt"`
	RetiredAt *Seq       `json:"retiredAt,omitempty"`
}

// DocumentValue is a document's materialized value at one point.
type DocumentValue struct {
	Document Document        `json:"document"`
	Version  int             `json:"version"`
	Value    json.RawMessage `json:"value"`
}

// Current addresses the latest committed state in document reads.
const Current Seq = -1

// WriteType names one write in a commit batch.
type WriteType string

// Write kinds accepted by Commit.
const (
	WriteConversation   WriteType = "conversation"
	WriteEntry          WriteType = "entry"
	WriteTask           WriteType = "task"
	WriteSubmission     WriteType = "submission"
	WriteDocumentCreate WriteType = "document.create"
	WriteDocumentCopy   WriteType = "document.copy"
	WriteDocumentChange WriteType = "document.change"
	WriteDocumentRetire WriteType = "document.retire"
)

// Write is one upsert. Exactly the field matching Type is set. Document changes carry
// either a full Value (base) or JSON patch Ops (delta).
type Write struct {
	Type         WriteType
	Conversation *Conversation
	Entry        *Entry
	Task         *Task
	Submission   *Submission
	Document     *Document
	CopyFrom     *DocumentID
	Value        json.RawMessage
	Ops          json.RawMessage
}

// Batch is applied atomically and receives one Seq.
type Batch []Write

// Page is one page of a scan. Next is empty on the last page.
type Page[T any] struct {
	Items []T
	Next  string
}

// Scan bounds a query. Cursor is the Next value of a previous page.
type Scan struct {
	Limit      int
	Cursor     string
	Descending bool
}

// Queries filter scans; zero fields match everything.
type (
	// ConversationQuery filters Conversations.
	ConversationQuery struct {
		OwnerConversationID ConversationID
		OwnerTaskID         TaskID
	}
	// EntryQuery filters Entries of one conversation; Min and Max are inclusive.
	EntryQuery struct {
		ConversationID ConversationID
		Min, Max       EntryID
	}
	// TaskQuery filters Tasks.
	TaskQuery struct {
		ConversationID ConversationID
		Kind           string
		Status         TaskStatus
		AbortRequested *bool
		Background     *bool
	}
	// SubmissionQuery filters Submissions.
	SubmissionQuery struct {
		ConversationID ConversationID
		Status         SubmissionStatus
	}
	// DocumentQuery lists documents in a scope at a point.
	DocumentQuery struct {
		Scope Scope
		At    Seq
		Kind  string
	}
)

// Store is the durable record store. Implementations must be safe for concurrent use
// and serialize commits. Commit applies batches in order; a refused batch stops the
// commit and returns the count stored before it with a non-nil error.
type Store interface {
	Commit(ctx context.Context, batches []Batch) (stored int, seqs []Seq, err error)
	MintID(ctx context.Context) (int64, error)
	// Watch delivers the Seq of every commit after the call until ctx ends.
	Watch(ctx context.Context) (<-chan Seq, error)

	Conversation(ctx context.Context, id ConversationID) (Conversation, error)
	Conversations(ctx context.Context, query ConversationQuery, scan Scan) (Page[Conversation], error)

	Entry(ctx context.Context, id EntryID) (Entry, Seq, error)
	// LatestHead finds the newest entry with Head at or before the given entry (0 = end).
	LatestHead(ctx context.Context, conversation ConversationID, atOrBefore EntryID) (Entry, error)
	Entries(ctx context.Context, query EntryQuery, scan Scan) (Page[Entry], error)

	Task(ctx context.Context, id TaskID) (Task, error)
	Tasks(ctx context.Context, query TaskQuery, scan Scan) (Page[Task], error)

	Submission(ctx context.Context, id SubmissionID) (Submission, error)
	SubmissionByRequest(ctx context.Context, conversation ConversationID, requestID string) (Submission, error)
	Submissions(ctx context.Context, query SubmissionQuery, scan Scan) (Page[Submission], error)

	FindDocument(ctx context.Context, kind string, scope Scope, key string, at Seq) (Document, error)
	Document(ctx context.Context, id DocumentID, at Seq) (DocumentValue, error)
	Documents(ctx context.Context, query DocumentQuery, scan Scan) (Page[Document], error)

	Close() error
}
