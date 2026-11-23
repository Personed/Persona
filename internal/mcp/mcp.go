// Package mcp stores managed MCP connections: their metadata, Bot grants and
// encrypted credential references.
//
// Connections live in the session-scoped "persona.mcp-connections" document and
// their secrets in the hosting credential store under "persona-mcp:<id>:<uuid>",
// both matching the TypeScript backend (packages/agent/src/mcp/connections.ts),
// so imported installations keep their connections. Inspecting and signing in
// run on the connection's work computer through a Runner.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Personed/Persona/internal/hosting"
	"github.com/Personed/Persona/internal/store"
)

// DocKind matches the pi-durable defineDoc kind.
const DocKind = "persona.mcp-connections"

// Grant gives one Bot a connection's tools; nil Tools grants every tool.
type Grant struct {
	BotID int64    `json:"botId"`
	Tools []string `json:"tools"`
}

// Connection is a managed MCP connection as the API shows it.
type Connection struct {
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	ComputerID           string          `json:"computerId"`
	Transport            string          `json:"transport"`
	Enabled              bool            `json:"enabled"`
	Grants               []Grant         `json:"grants"`
	Revision             int             `json:"revision"`
	Generation           int             `json:"generation"`
	Status               string          `json:"status,omitempty"`
	AuthorizationPending *bool           `json:"authorizationPending,omitempty"`
	CheckedAt            int64           `json:"checkedAt,omitempty"`
	Error                string          `json:"error,omitempty"`
	Tools                json.RawMessage `json:"tools,omitempty"`
	Capabilities         json.RawMessage `json:"capabilities,omitempty"`
}

type stored struct {
	Connection
	Credential string `json:"credential"`
}

type document struct {
	Items  []stored        `json:"items"`
	Native map[string]bool `json:"native,omitempty"`
}

// Secret is a connection's encrypted configuration and OAuth state.
type Secret struct {
	Config      json.RawMessage `json:"config"`
	OAuth       json.RawMessage `json:"oauth,omitempty"`
	RedirectURL string          `json:"redirectUrl,omitempty"`
	Pending     string          `json:"pending,omitempty"`
}

// Operation is one request to the work computer's MCP client: inspect,
// authorize or complete, with the connection's secret.
type Operation struct {
	Action string `json:"action"`
	Secret
	State             string `json:"state,omitempty"`
	AuthorizationCode string `json:"authorizationCode,omitempty"`
}

// Outcome is what the work computer's MCP client answers.
type Outcome struct {
	Tools            json.RawMessage `json:"tools,omitempty"`
	Capabilities     json.RawMessage `json:"capabilities,omitempty"`
	OAuth            json.RawMessage `json:"oauth,omitempty"`
	AuthorizationURL string          `json:"authorizationUrl,omitempty"`
}

// ErrAuthorization is the cause of a Runner failure that needs the user to sign in.
var ErrAuthorization = errors.New("mcp: authorization required")

// Runner runs an Operation on a work computer.
type Runner interface {
	Run(ctx context.Context, computerID string, op Operation) (Outcome, error)
}

// NativeItem is one connection the Bot's native Harness configures itself.
type NativeItem struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Enabled bool   `json:"enabled"`
	Status  string `json:"status"`
}

// NativeCatalog is the native Harness's own connections on the Bot's work computer.
type NativeCatalog struct {
	Provider    string       `json:"provider"`
	ComputerID  string       `json:"computerId"`
	Items       []NativeItem `json:"items"`
	Inheritance string       `json:"inheritance"`
	Readonly    bool         `json:"readonly"`
	Inherited   bool         `json:"inherited"`
}

// Client is an OAuth client the user registered by hand.
type Client struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// SaveInput saves a connection. Revision is 0 for a new one; nil Config keeps
// the stored configuration.
type SaveInput struct {
	ID         string          `json:"id"`
	Revision   int             `json:"revision"`
	Name       string          `json:"name"`
	ComputerID string          `json:"computerId"`
	Enabled    bool            `json:"enabled"`
	Config     json.RawMessage `json:"config"`
}

// ErrorCode classifies an Error like the TypeScript WorkError.
type ErrorCode string

// Error codes.
const (
	NotFound   ErrorCode = "NOT_FOUND"
	Conflict   ErrorCode = "CONFLICT"
	BadRequest ErrorCode = "BAD_REQUEST"
)

// Error is a validation or concurrency failure the caller can correct.
type Error struct {
	Code    ErrorCode
	Message string
}

func (e *Error) Error() string { return e.Message }

// Service manages connections through a store.Store and the credential store.
type Service struct {
	// Runner reaches the work computer; nil refuses Check and Login.
	Runner Runner
	// Catalog reads a Bot's native connections; nil leaves them unavailable.
	Catalog func(ctx context.Context, botID int64) (NativeCatalog, error)

	store       store.Store
	credentials hosting.Credentials
	now         func() time.Time
	// mu serializes read-modify-write cycles; store.Store has no conditional writes.
	mu sync.Mutex
}

// NewService returns a Service. Without credentials, Save fails as the
// TypeScript service does without a credential store. now defaults to time.Now.
func NewService(records store.Store, credentials hosting.Credentials, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: records, credentials: credentials, now: now}
}

var sessionScope = store.Scope{Kind: "session"}

func (s *Service) load(ctx context.Context) (store.Document, document, error) {
	var doc document
	meta, err := s.store.FindDocument(ctx, DocKind, sessionScope, "", store.Current)
	if errors.Is(err, store.ErrNotFound) {
		return store.Document{}, doc, nil
	}
	if err != nil {
		return store.Document{}, doc, err
	}
	value, err := s.store.Document(ctx, meta.ID, store.Current)
	if err != nil {
		return store.Document{}, doc, err
	}
	if err := json.Unmarshal(value.Value, &doc); err != nil {
		return store.Document{}, doc, fmt.Errorf("mcp: decode %s: %w", DocKind, err)
	}
	return meta, doc, nil
}

func (s *Service) save(ctx context.Context, meta store.Document, doc document) error {
	if doc.Items == nil {
		doc.Items = []stored{}
	}
	value, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	write := store.Write{Type: store.WriteDocumentChange, Document: &meta, Value: value}
	if meta.ID == 0 {
		id, err := s.store.MintID(ctx)
		if err != nil {
			return err
		}
		meta = store.Document{ID: store.DocumentID(id), Kind: DocKind, Scope: sessionScope}
		write = store.Write{Type: store.WriteDocumentCreate, Document: &meta, Value: value}
	}
	_, _, err = s.store.Commit(ctx, []store.Batch{{write}})
	return err
}

// commit applies edit to the document under mu and writes it when edit reports a change.
func (s *Service) commit(ctx context.Context, edit func(doc *document) (bool, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, doc, err := s.load(ctx)
	if err != nil {
		return err
	}
	changed, err := edit(&doc)
	if err != nil || !changed {
		return err
	}
	return s.save(ctx, meta, doc)
}

func find(doc *document, id string) *stored {
	index := slices.IndexFunc(doc.Items, func(item stored) bool { return item.ID == id })
	if index < 0 {
		return nil
	}
	return &doc.Items[index]
}

// List returns every connection without its credential reference.
func (s *Service) List(ctx context.Context) ([]Connection, error) {
	_, doc, err := s.load(ctx)
	items := make([]Connection, len(doc.Items))
	for i, item := range doc.Items {
		items[i] = item.Connection
	}
	return items, err
}

// Native reports whether a Bot inherits its native Harness connections,
// after setting it when enabled is not nil. It defaults to true.
func (s *Service) Native(ctx context.Context, botID int64, enabled *bool) (bool, error) {
	key := strconv.FormatInt(botID, 10)
	if enabled != nil {
		if err := s.commit(ctx, func(doc *document) (bool, error) {
			if doc.Native == nil {
				doc.Native = map[string]bool{}
			}
			doc.Native[key] = *enabled
			return true, nil
		}); err != nil {
			return false, err
		}
	}
	_, doc, err := s.load(ctx)
	if inherited, ok := doc.Native[key]; ok {
		return inherited, err
	}
	return true, err
}

// Save creates or reconfigures a connection. It stores the secret under a
// new credential reference and deletes the previous one once the document
// points at the new one.
func (s *Service) Save(ctx context.Context, input SaveInput) (Connection, error) {
	if s.credentials == nil {
		return Connection{}, &Error{BadRequest, "凭据存储不可用。"}
	}
	name, err := requiredText(input.Name, 128)
	if err != nil {
		return Connection{}, err
	}
	computerID, err := requiredText(input.ComputerID, 128)
	if err != nil {
		return Connection{}, err
	}
	if strings.ToLower(name) == "persona" {
		return Connection{}, &Error{BadRequest, "此名称已保留。"}
	}
	var current *Secret
	if input.Revision != 0 {
		_, secret, err := s.secret(ctx, input.ID)
		if err != nil {
			return Connection{}, err
		}
		current = &secret
	}
	config := input.Config
	if len(config) == 0 && current != nil {
		config = current.Config
	}
	if len(config) == 0 {
		return Connection{}, &Error{BadRequest, "请填写连接配置。"}
	}
	var parsed struct {
		Transport string `json:"transport"`
		URL       string `json:"url"`
		Command   string `json:"command"`
	}
	if err := json.Unmarshal(config, &parsed); err != nil {
		return Connection{}, &Error{BadRequest, "请填写连接配置。"}
	}
	if parsed.Transport == "http" {
		u, err := url.Parse(parsed.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return Connection{}, &Error{BadRequest, "MCP 地址无效。"}
		}
	} else if _, err := requiredText(parsed.Command, 4096); err != nil {
		return Connection{}, err
	}
	secret := Secret{Config: config}
	if len(input.Config) == 0 {
		secret = *current
	}
	credential := "persona-mcp:" + input.ID + ":" + newUUID()
	if err := s.put(ctx, credential, secret); err != nil {
		return Connection{}, err
	}
	var previous string
	var saved Connection
	err = s.commit(ctx, func(doc *document) (bool, error) {
		existing := find(doc, input.ID)
		revision, generation := 0, 0
		grants := []Grant{}
		if existing != nil {
			revision, generation, grants, previous = existing.Revision, existing.Generation, existing.Grants, existing.Credential
		}
		if revision != input.Revision {
			return false, &Error{Conflict, "内容已更新，请刷新后重试。"}
		}
		if slices.ContainsFunc(doc.Items, func(item stored) bool {
			return item.ID != input.ID && strings.EqualFold(item.Name, name)
		}) {
			return false, &Error{Conflict, "连接名称已存在。"}
		}
		next := stored{Connection: Connection{
			ID: input.ID, Name: name, ComputerID: computerID, Transport: parsed.Transport, Enabled: input.Enabled,
			Grants: grants, Revision: input.Revision + 1, Generation: generation + 1, Status: "configured",
		}, Credential: credential}
		if existing != nil {
			*existing = next
		} else {
			doc.Items = append(doc.Items, next)
		}
		saved = next.Connection
		return true, nil
	})
	if err != nil {
		_ = s.credentials.Delete(ctx, ref(credential))
		return Connection{}, err
	}
	if previous != "" {
		if err := s.credentials.Delete(ctx, ref(previous)); err != nil && !errors.Is(err, hosting.ErrNotFound) {
			return Connection{}, err
		}
	}
	return saved, nil
}

// Grant replaces a connection's Bot grants at its current revision.
func (s *Service) Grant(ctx context.Context, id string, revision int, grants []Grant) (Connection, error) {
	seen := map[int64]bool{}
	for _, grant := range grants {
		if seen[grant.BotID] {
			return Connection{}, &Error{BadRequest, "Bot 权限重复。"}
		}
		seen[grant.BotID] = true
	}
	var saved Connection
	err := s.commit(ctx, func(doc *document) (bool, error) {
		item := find(doc, id)
		if item == nil {
			return false, &Error{NotFound, "连接不存在。"}
		}
		if item.Revision != revision {
			return false, &Error{Conflict, "内容已更新，请刷新后重试。"}
		}
		item.Grants = make([]Grant, len(grants))
		for i, grant := range grants {
			item.Grants[i] = Grant{BotID: grant.BotID, Tools: unique(grant.Tools)}
		}
		item.Revision++
		saved = item.Connection
		return true, nil
	})
	return saved, err
}

// Remove deletes a connection at its current revision, then its credential.
func (s *Service) Remove(ctx context.Context, id string, revision int) error {
	var credential string
	err := s.commit(ctx, func(doc *document) (bool, error) {
		index := slices.IndexFunc(doc.Items, func(item stored) bool { return item.ID == id })
		if index < 0 {
			return false, &Error{NotFound, "连接不存在。"}
		}
		if doc.Items[index].Revision != revision {
			return false, &Error{Conflict, "内容已更新，请刷新后重试。"}
		}
		credential = doc.Items[index].Credential
		doc.Items = slices.Delete(doc.Items, index, index+1)
		return true, nil
	})
	if err != nil || s.credentials == nil {
		return err
	}
	if err := s.credentials.Delete(ctx, ref(credential)); err != nil && !errors.Is(err, hosting.ErrNotFound) {
		return err
	}
	return nil
}

// Check inspects a connection on its work computer and records its tools,
// or why it failed. Only the failure to reach the Runner is an error.
func (s *Service) Check(ctx context.Context, id string) (Connection, error) {
	item, secret, err := s.secret(ctx, id)
	if err != nil {
		return Connection{}, err
	}
	if !item.Enabled {
		return Connection{}, &Error{BadRequest, "连接已禁用。"}
	}
	if s.Runner == nil {
		return Connection{}, &Error{BadRequest, "连接执行服务不可用。"}
	}
	result, err := s.Runner.Run(ctx, item.ComputerID, Operation{Action: "inspect", Secret: secret})
	if err == nil && (len(result.Tools) == 0 || len(result.Capabilities) == 0) {
		err = errors.New("MCP 能力响应无效。")
	}
	if err == nil {
		next := secret
		next.OAuth = result.OAuth
		err = s.replace(ctx, item, next)
	}
	checked := s.now().UnixMilli()
	if err != nil {
		if ctx.Err() != nil {
			return Connection{}, ctx.Err()
		}
		authorization := errors.Is(err, ErrAuthorization)
		err = s.commit(ctx, func(doc *document) (bool, error) {
			live := find(doc, id)
			if live == nil || live.Revision != item.Revision {
				return false, nil
			}
			live.Status, live.CheckedAt, live.Error = "failed", checked, "连接失败。请检查程序、地址、网络和授权。"
			if authorization {
				live.Status, live.Error = "authorization_required", "请登录 MCP 服务。"
			}
			return true, nil
		})
	} else {
		err = s.commit(ctx, func(doc *document) (bool, error) {
			live := find(doc, id)
			if live == nil || live.Revision != item.Revision {
				return false, nil
			}
			live.Status, live.CheckedAt, live.Error = "ready", checked, ""
			live.Tools, live.Capabilities = result.Tools, result.Capabilities
			return true, nil
		})
	}
	if err != nil {
		return Connection{}, err
	}
	item, _, err = s.secret(ctx, id)
	return item.Connection, err
}

// Login starts a browser sign-in for an HTTP connection and returns the
// authorization URL, or nil when the server needs none and the connection
// was checked instead.
func (s *Service) Login(ctx context.Context, id, redirectURL string, client *Client) (*string, error) {
	item, secret, err := s.secret(ctx, id)
	if err != nil {
		return nil, err
	}
	var config struct {
		Transport string `json:"transport"`
	}
	_ = json.Unmarshal(secret.Config, &config)
	if !item.Enabled || config.Transport != "http" {
		return nil, &Error{BadRequest, "此连接不能使用浏览器登录。"}
	}
	if s.Runner == nil {
		return nil, &Error{BadRequest, "连接执行服务不可用。"}
	}
	state := id + "." + newUUID()
	oauth := json.RawMessage(`{}`)
	if client != nil {
		if oauth, err = json.Marshal(map[string]any{"client": client}); err != nil {
			return nil, err
		}
	}
	result, err := s.Runner.Run(ctx, item.ComputerID, Operation{Action: "authorize",
		Secret: Secret{Config: secret.Config, OAuth: oauth, RedirectURL: redirectURL}, State: state})
	if err != nil {
		return nil, err
	}
	next := secret
	next.OAuth, next.RedirectURL, next.Pending = result.OAuth, redirectURL, state
	if err := s.replace(ctx, item, next); err != nil {
		return nil, err
	}
	pending := result.AuthorizationURL != ""
	if err := s.commit(ctx, func(doc *document) (bool, error) {
		live := find(doc, id)
		if live == nil || live.Revision != item.Revision {
			return false, nil
		}
		live.AuthorizationPending = &pending
		return true, nil
	}); err != nil {
		return nil, err
	}
	if !pending {
		_, err := s.Check(ctx, id)
		return nil, err
	}
	return &result.AuthorizationURL, nil
}

// Logout cancels a pending sign-in and, unless cancel is set, forgets the
// OAuth state, then saves the connection again under a new credential.
func (s *Service) Logout(ctx context.Context, id string, cancel bool) error {
	item, secret, err := s.secret(ctx, id)
	if err != nil {
		return err
	}
	secret.Pending = ""
	if !cancel {
		secret.OAuth = nil
	}
	if err := s.replace(ctx, item, secret); err != nil {
		return err
	}
	_, err = s.Save(ctx, SaveInput{ID: id, Revision: item.Revision, Name: item.Name, ComputerID: item.ComputerID, Enabled: item.Enabled})
	return err
}

// Complete finishes the browser sign-in that state names with the
// authorization code from the OAuth callback, then checks the connection.
func (s *Service) Complete(ctx context.Context, state, code string) error {
	id, _, _ := strings.Cut(state, ".")
	item, secret, err := s.secret(ctx, id)
	if err != nil {
		return err
	}
	if secret.Pending == "" || secret.Pending != state || s.Runner == nil {
		return &Error{BadRequest, "授权请求已失效。"}
	}
	result, err := s.Runner.Run(ctx, item.ComputerID, Operation{Action: "complete",
		Secret: secret, State: state, AuthorizationCode: code})
	if err != nil {
		return err
	}
	current, currentSecret, err := s.secret(ctx, id)
	if err != nil {
		return err
	}
	if currentSecret.Pending != state || current.Revision != item.Revision {
		return &Error{Conflict, "授权已取消或连接已更改。"}
	}
	if err := s.replace(ctx, item, Secret{Config: secret.Config, OAuth: result.OAuth, RedirectURL: secret.RedirectURL}); err != nil {
		return err
	}
	if err := s.commit(ctx, func(doc *document) (bool, error) {
		live := find(doc, id)
		if live == nil || live.Revision != item.Revision {
			return false, nil
		}
		live.AuthorizationPending = nil
		return true, nil
	}); err != nil {
		return err
	}
	_, err = s.Check(ctx, id)
	return err
}

func ref(credential string) hosting.Ref { return hosting.Ref{Scope: "mcp", Name: credential} }

// apiKey is the pi-ai credential JSON the TypeScript service stores a secret in.
type apiKey struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

func (s *Service) put(ctx context.Context, credential string, secret Secret) error {
	key, err := json.Marshal(secret)
	if err != nil {
		return err
	}
	value, err := json.Marshal(apiKey{Type: "api_key", Key: string(key)})
	if err != nil {
		return err
	}
	return s.credentials.Put(ctx, hosting.Credential{Ref: ref(credential), Secret: value})
}

// secret returns a connection with its decrypted secret.
func (s *Service) secret(ctx context.Context, id string) (stored, Secret, error) {
	_, doc, err := s.load(ctx)
	if err != nil {
		return stored{}, Secret{}, err
	}
	item := find(&doc, id)
	if item == nil {
		return stored{}, Secret{}, &Error{NotFound, "连接不存在。"}
	}
	unavailable := &Error{BadRequest, "连接凭据不可用。"}
	if s.credentials == nil {
		return stored{}, Secret{}, unavailable
	}
	credential, err := s.credentials.Get(ctx, ref(item.Credential))
	if errors.Is(err, hosting.ErrNotFound) {
		return stored{}, Secret{}, unavailable
	}
	if err != nil {
		return stored{}, Secret{}, err
	}
	var value apiKey
	var secret Secret
	if json.Unmarshal(credential.Secret, &value) != nil || value.Type != "api_key" || value.Key == "" ||
		json.Unmarshal([]byte(value.Key), &secret) != nil {
		return stored{}, Secret{}, unavailable
	}
	return *item, secret, nil
}

// replace replaces the secret of a connection whose credential still exists.
func (s *Service) replace(ctx context.Context, item stored, secret Secret) error {
	if s.credentials == nil {
		return nil
	}
	if _, err := s.credentials.Get(ctx, ref(item.Credential)); errors.Is(err, hosting.ErrNotFound) {
		return &Error{Conflict, "连接已更改，请重新操作。"}
	} else if err != nil {
		return err
	}
	return s.put(ctx, item.Credential, secret)
}

func unique(values []string) []string {
	if values == nil {
		return nil
	}
	result := []string{}
	for _, value := range values {
		if !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func requiredText(value string, maximum int) (string, error) {
	text := strings.TrimSpace(value)
	if length := len(utf16.Encode([]rune(text))); length == 0 || length > maximum {
		return "", &Error{BadRequest, fmt.Sprintf("内容长度须为1至%d个字符。", maximum)}
	}
	return text, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
