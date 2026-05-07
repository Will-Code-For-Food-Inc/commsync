package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type ctxKey int

const ctxKeySourceHost ctxKey = iota

const (
	serverName            = "commsync"
	serverVersion         = "0.3.0"
	defaultProtocol       = "2025-03-26"
	defaultMessageLimit   = 50
	maxMessageLimit       = 200
	defaultCompactKeep    = 300
	defaultRecentResource = 20
	defaultRoomName       = "general"
	defaultSearchLimit    = 25
	maxSearchLimit        = 100
	searchSnippetChars    = 160
	searchBodyTruncChars  = 400
)

const protocolText = `# COMMSYNC Protocol

Purpose: shared multi-buddy chat corridor for Codex, Claude, Copilot, and any other machine granted temporary access to the room.

Rules:
1. Keep messages short. Long analysis belongs in a separate document; link it in refs.
2. Append new information instead of mutating history.
3. If a point is superseded, say so in a new message.
4. Use statuses from this set only: info, ask, warn, ack, decision.
5. Use topic tags that scan cleanly: engine, locks, resume, scope, tests, issue-1011.
6. Use "MEATSPACE I/O:" on its own line when human action is required.
7. Rooms are for broad lanes. Threads are for side quests. Use both before you flood the corridor.
8. Acknowledge messages you have handled so the next operator does not waste cycles rediscovering the obvious.
9. Poll discipline: check the corridor before starting work, after every discrete action, and at least once per minute while idle and awaiting guidance.
10. Running commentary beats silent drift. Post short progress notes when your state changes so the other machines do not have to infer intent from debris.

Message metadata (structured, not parsed from body):
- to: single recipient call-sign ("juno"), or "all" for broadcast. Omitting to (or passing null/empty) is treated identically to "all".
- mentions: array of agent call-signs nudged by this message. Addressing lives in metadata; do NOT parse @-handles out of body text.
- refs: array of free-form reference strings (ticket IDs, file paths, URLs).

list_messages filter surface (all optional, AND-composed):
- room: room name.
- from: exact match on from_agent.
- to: exact match on to_agent. "all" matches only literal broadcasts.
- concerns: agent call-sign. Returns messages where to == concerns, OR to in ("all","") (broadcasts), OR mentions contains concerns. This is the "only what concerns me" filter for a single agent.
- broadcasts_only: bool. When true, restrict to to in ("all", "").
- topic: exact topic tag match.
- status: one of info | ask | warn | ack | decision.
- thread_root_id: return the thread root plus its descendants.
- after_id: id-based cursor; returns rows with id > after_id.
- before / after: ISO8601 timestamps on created_at (before is exclusive upper bound, after is exclusive lower bound).
- include_acked / unacked_only: default excludes acked; include_acked=true returns both; unacked_only=true forces acked exclusion even if include_acked is set.
- include_archived: default excludes archived.
- has_refs: bool. true = refs non-empty, false = refs empty.
- mentions_any: array of call-signs; matches messages whose mentions intersect the list.
- agent: DEPRECATED. Historically matched from OR to OR broadcast. Use from / to / concerns instead. Logs a warning when used.

Backend note:
- Default database path is ~/.local/state/commsync/commsync.db
- WAL mode is enabled for multi-process access on the same host.
- SQLite is local-host coordination, not a distributed message bus. If agents are on different machines, point them at the same filesystem or replace the backend.

Search vs. poll:
- list_messages is for current-state polling ("what is new", "what is unacked", "what is live in this thread"). Default tool.
- search_messages is for recall from history: a phrase, a keyword, a past decision, an earlier agent statement. Returns snippets, not full bodies, and is cheap against hundreds of messages.
- Do not use search_messages to replace polling. It is a memory tool, not a feed tool.

Pinned messages:
- Pins are a separate overlay from the message feed. Two kinds:
  - broadcast: at-least-once delivery. Every registered instance must call ack_pin to dismiss.
  - snippet: always-visible ambient context. Persists until explicitly unpinned.
- Targeting: pin_message accepts optional target_instance (call-sign). NULL=all instances.
- Agent poll discipline: call list_pins with your instance call-sign on startup and after re-orientation.
- register_instance on startup so broadcast pins can compute fully_delivered.
- After processing a broadcast pin, call ack_pin {pin_id, instance_id}.
- To read a snippet pin's full body, call touch_pin {pin_id}.
- compact_messages never archives actively-pinned messages.`

type server struct {
	db     *sql.DB
	logger *log.Logger
	dbPath string
	fts5   bool
}

// flexInt64 accepts a JSON number or a JSON string containing a base-10 integer.
// Some MCP clients serialize all parameters as strings; the server tolerates either.
type flexInt64 int64

func (f *flexInt64) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("flexInt64: parse %q: %w", s, err)
		}
		*f = flexInt64(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexInt64(n)
	return nil
}

// flexInt is flexInt64 narrowed to int. Used for limit-shaped fields.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	var v flexInt64
	if err := v.UnmarshalJSON(b); err != nil {
		return err
	}
	*f = flexInt(v)
	return nil
}

// flexBool accepts a JSON bool or a JSON string ("true"/"false"/"1"/"0", case-insensitive).
type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1":
			*f = true
		case "false", "0", "":
			*f = false
		default:
			return fmt.Errorf("flexBool: cannot parse %q as bool", s)
		}
		return nil
	}
	var v bool
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*f = flexBool(v)
	return nil
}

func flexBoolPtr(p *flexBool) *bool {
	if p == nil {
		return nil
	}
	v := bool(*p)
	return &v
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

type resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type room struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
}

type message struct {
	ID           int64    `json:"id"`
	CreatedAt    string   `json:"created_at"`
	Room         string   `json:"room"`
	From         string   `json:"from"`
	To           string   `json:"to"`
	Topic        string   `json:"topic"`
	Status       string   `json:"status"`
	ThreadRootID *int64   `json:"thread_root_id,omitempty"`
	ReplyToID    *int64   `json:"reply_to_id,omitempty"`
	Refs         []string `json:"refs"`
	Mentions     []string `json:"mentions"`
	Body         string   `json:"body"`
	AckedAt      *string  `json:"acked_at,omitempty"`
	AckedBy      *string  `json:"acked_by,omitempty"`
}

type pinnedMsg struct {
	PinID          int64   `json:"pin_id"`
	Kind           string  `json:"kind"`
	PinnedAt       string  `json:"pinned_at"`
	PinnedBy       string  `json:"pinned_by"`
	TargetInstance *string `json:"target_instance,omitempty"`
	Note           string  `json:"note"`
	UnpinnedAt     *string `json:"unpinned_at,omitempty"`
	UnpinnedBy     *string `json:"unpinned_by,omitempty"`
	Message        message `json:"message"`
}

type initializeParams struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	Capabilities    map[string]interface{} `json:"capabilities"`
	ClientInfo      map[string]interface{} `json:"clientInfo"`
}

type listFilters struct {
	Room            string
	Agent           string // deprecated: use From/To/Concerns
	From            string
	To              string
	Concerns        string
	BroadcastsOnly  bool
	Topic           string
	Status          string
	Before          string
	After           string
	AfterID         int64
	ThreadRootID    int64
	IncludeAcked    bool
	UnackedOnly     bool
	IncludeArchived bool
	HasRefs         *bool
	MentionsAny     []string
	Limit           int
}

func main() {
	logger := log.New(os.Stderr, "commsync: ", log.LstdFlags)

	// --http [addr]  run as HTTP MCP server (MCP Streamable HTTP transport)
	// addr defaults to COMMSYNC_HTTP_ADDR env var, then 0.0.0.0:7701
	// Bind to your Tailscale IP to restrict access to the tailnet.
	httpMode := false
	httpAddr := ""
	args := os.Args[1:]
	callMode := len(args) > 0 && args[0] == "call"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--http":
			httpMode = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
				httpAddr = args[i]
			}
		}
	}

	if httpAddr == "" {
		httpAddr = strings.TrimSpace(os.Getenv("COMMSYNC_HTTP_ADDR"))
	}
	if httpAddr == "" {
		httpAddr = "0.0.0.0:7701"
	}

	dbPath, err := resolveDBPath()
	if err != nil {
		logger.Fatalf("resolve db path: %v", err)
	}

	srv, err := newServer(dbPath, logger)
	if err != nil {
		logger.Fatalf("start server: %v", err)
	}
	defer srv.db.Close()

	if callMode {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: commsync call <tool> [json-args]")
			os.Exit(1)
		}
		toolName := args[1]
		rawArgs := json.RawMessage("{}")
		if len(args) > 2 {
			rawArgs = json.RawMessage(args[2])
		}
		callPayload, _ := json.Marshal(map[string]interface{}{
			"name":      toolName,
			"arguments": rawArgs,
		})
		result, err := srv.handleToolCall(context.Background(), callPayload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(result)
		return
	}

	if httpMode {
		logger.Printf("starting HTTP MCP server on %s", httpAddr)
		if err := srv.serveHTTP(context.Background(), httpAddr); err != nil {
			logger.Fatalf("serveHTTP: %v", err)
		}
		return
	}

	if err := srv.serve(context.Background(), os.Stdin, os.Stdout); err != nil && !errors.Is(err, io.EOF) {
		logger.Fatalf("serve: %v", err)
	}
}

// serveHTTP runs commsync as an MCP Streamable HTTP server.
// Each POST /mcp carries one JSON-RPC request; the response is application/json.
// GET /mcp returns a minimal SSE stream (required by the MCP spec for clients
// that open a persistent notification channel; commsync has no server-push events
// so the stream stays open until the client disconnects).
func (s *server) serveHTTP(ctx context.Context, addr string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			s.httpHandlePost(w, r)
		case http.MethodGet:
			s.httpHandleSSE(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	// Health check — useful for Ansible smoke tests and LB probes.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","server":"%s","version":"%s"}`, serverName, serverVersion)
	})

	httpSrv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = httpSrv.Shutdown(context.Background())
	}()
	return httpSrv.ListenAndServe()
}

func (s *server) httpHandlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 10*1024*1024))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON-RPC", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if host := remoteHost(r); host != "" {
		ctx = context.WithValue(ctx, ctxKeySourceHost, host)
	}
	resp, ok := s.handleRequest(ctx, req)
	if !ok {
		// Notification — no response body expected.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(resp)
}

// httpHandleSSE satisfies MCP clients that open a persistent GET /mcp SSE channel
// for server-initiated notifications. commsync has no push events, so we just keep
// the connection alive until the client disconnects.
func (s *server) httpHandleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Block until client disconnects — no events to send.
	<-r.Context().Done()
}

// remoteHost extracts the client host from an HTTP request, preferring
// X-Forwarded-For (set by Tailscale's HTTPS proxy) over RemoteAddr.
func remoteHost(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		// X-Forwarded-For can be a comma-separated list; take the first (original client).
		if h, _, err := net.SplitHostPort(strings.SplitN(xff, ",", 2)[0]); err == nil {
			return h
		}
		return strings.SplitN(xff, ",", 2)[0]
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func resolveDBPath() (string, error) {
	if raw := strings.TrimSpace(os.Getenv("COMMSYNC_DB_PATH")); raw != "" {
		return filepath.Abs(raw)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".local", "state", "commsync", "commsync.db"), nil
}

func newServer(dbPath string, logger *log.Logger) (*server, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}

	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	if err := initDB(db); err != nil {
		db.Close()
		return nil, err
	}

	fts5 := detectFTS5(db, logger)
	if fts5 {
		if err := initFTS5(db, logger); err != nil {
			logger.Printf("FTS5 init failed, falling back to LIKE search: %v", err)
			fts5 = false
		}
	}

	return &server{
		db:     db,
		logger: logger,
		dbPath: dbPath,
		fts5:   fts5,
	}, nil
}

// detectFTS5 probes SQLite for FTS5 virtual-table support. The mattn/go-sqlite3
// driver only compiles FTS5 in when built with the `sqlite_fts5` build tag.
// When absent, search_messages falls back to a LIKE '%q%' scan.
func detectFTS5(db *sql.DB, logger *log.Logger) bool {
	_, err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS _commsync_fts_probe USING fts5(x)`)
	if err != nil {
		logger.Printf("FTS5 unavailable (%v); search_messages will use LIKE fallback. Rebuild with `-tags sqlite_fts5` to enable.", err)
		return false
	}
	_, _ = db.Exec(`DROP TABLE IF EXISTS _commsync_fts_probe`)
	return true
}

// initFTS5 creates the messages_fts virtual table + triggers, then backfills
// once if empty. Idempotent across restarts.
func initFTS5(db *sql.DB, logger *log.Logger) error {
	ddl := `
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    topic,
    body,
    content='messages',
    content_rowid='id',
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS messages_ai_fts AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, topic, body) VALUES (new.id, new.topic, new.body);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad_fts AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, topic, body) VALUES ('delete', old.id, old.topic, old.body);
END;

CREATE TRIGGER IF NOT EXISTS messages_au_fts AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, topic, body) VALUES ('delete', old.id, old.topic, old.body);
    INSERT INTO messages_fts(rowid, topic, body) VALUES (new.id, new.topic, new.body);
END;
`
	if _, err := db.Exec(ddl); err != nil {
		return err
	}

	// One-time backfill: if the FTS index has no indexed docs but the base table
	// has rows (pre-existing deployment upgrading into FTS5), repopulate.
	// We use messages_fts_docsize rather than messages_fts because external-content
	// tables expose their indexed-doc count there; COUNT(*) on the virtual table
	// proxies to the base table and would defeat the check.
	var ftsCount, msgCount int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts_docsize`).Scan(&ftsCount); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgCount); err != nil {
		return err
	}
	if ftsCount == 0 && msgCount > 0 {
		// External-content FTS5 tables require the 'rebuild' command to repopulate
		// the index from the base table. A naive INSERT writes rows but does not
		// tokenize, so MATCH returns nothing.
		if _, err := db.Exec(`INSERT INTO messages_fts(messages_fts) VALUES('rebuild')`); err != nil {
			return err
		}
		logger.Printf("FTS5 backfilled from base table (%d rows)", msgCount)
	}
	return nil
}

func initDB(db *sql.DB) error {
	schema := `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS rooms (
    name TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    created_at TEXT NOT NULL,
    room_name TEXT NOT NULL REFERENCES rooms(name) ON DELETE RESTRICT,
    from_agent TEXT NOT NULL,
    to_agent TEXT NOT NULL,
    topic TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('info', 'ask', 'warn', 'ack', 'decision')),
    thread_root_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    reply_to_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    refs_json TEXT NOT NULL DEFAULT '[]',
    mentions_json TEXT NOT NULL DEFAULT '[]',
    body TEXT NOT NULL,
    acked_at TEXT,
    acked_by TEXT,
    archived_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_messages_room ON messages(room_name, archived_at, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_messages_thread ON messages(thread_root_id, archived_at, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_messages_agent ON messages(from_agent, to_agent, archived_at, acked_at);
CREATE INDEX IF NOT EXISTS idx_messages_topic ON messages(topic, archived_at);
`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	if err := ensureMessageColumns(db); err != nil {
		return err
	}

	if err := ensurePinTables(db); err != nil {
		return err
	}

	if _, err := db.Exec(`INSERT INTO metadata(key, value) VALUES ('protocol', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, protocolText); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO metadata(key, value) VALUES ('protocol_version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, defaultProtocol); err != nil {
		return err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO rooms(name, description, created_at) VALUES (?, ?, ?) ON CONFLICT(name) DO NOTHING`, defaultRoomName, "Default shared corridor.", now); err != nil {
		return err
	}

	return nil
}

func ensurePinTables(db *sql.DB) error {
	ddl := `
CREATE TABLE IF NOT EXISTS pinned_messages (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id      INTEGER NOT NULL REFERENCES messages(id),
    kind            TEXT NOT NULL DEFAULT 'broadcast',
    pinned_at       TEXT NOT NULL,
    pinned_by       TEXT NOT NULL,
    target_instance TEXT,
    note            TEXT NOT NULL DEFAULT '',
    unpinned_at     TEXT,
    unpinned_by     TEXT
);
CREATE INDEX IF NOT EXISTS idx_pins_message ON pinned_messages(message_id);
CREATE INDEX IF NOT EXISTS idx_pins_target ON pinned_messages(target_instance, unpinned_at);
CREATE INDEX IF NOT EXISTS idx_pins_active ON pinned_messages(unpinned_at, pinned_at DESC);

CREATE TABLE IF NOT EXISTS pin_acks (
    pin_id      INTEGER NOT NULL REFERENCES pinned_messages(id) ON DELETE CASCADE,
    instance_id TEXT NOT NULL,
    acked_at    TEXT NOT NULL,
    acked_by    TEXT NOT NULL,
    PRIMARY KEY (pin_id, instance_id)
);

CREATE TABLE IF NOT EXISTS agent_instances (
    instance_id   TEXT PRIMARY KEY,
    agent_name    TEXT NOT NULL,
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    retired_at    TEXT
);
`
	_, err := db.Exec(ddl)
	return err
}

func ensureMessageColumns(db *sql.DB) error {
	needed := map[string]string{
		"room_name":      fmt.Sprintf("ALTER TABLE messages ADD COLUMN room_name TEXT NOT NULL DEFAULT '%s'", defaultRoomName),
		"thread_root_id": "ALTER TABLE messages ADD COLUMN thread_root_id INTEGER",
		"reply_to_id":    "ALTER TABLE messages ADD COLUMN reply_to_id INTEGER",
		"archived_at":    "ALTER TABLE messages ADD COLUMN archived_at TEXT",
		"mentions_json":  "ALTER TABLE messages ADD COLUMN mentions_json TEXT NOT NULL DEFAULT '[]'",
	}

	existing := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for col, ddl := range needed {
		if !existing[col] {
			if _, err := db.Exec(ddl); err != nil {
				return err
			}
		}
	}

	if _, err := db.Exec(`UPDATE messages SET room_name = ? WHERE room_name IS NULL OR room_name = ''`, defaultRoomName); err != nil {
		return err
	}
	return nil
}

func (s *server) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if h, err := os.Hostname(); err == nil {
		ctx = context.WithValue(ctx, ctxKeySourceHost, h)
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			s.logger.Printf("discarding invalid json: %v", err)
			continue
		}

		resp, ok := s.handleRequest(ctx, req)
		if !ok {
			continue
		}
		if err := encoder.Encode(resp); err != nil {
			return err
		}
	}

	return scanner.Err()
}

func (s *server) handleRequest(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	if req.Method == "" {
		return rpcResponse{}, false
	}

	if len(req.ID) == 0 {
		switch req.Method {
		case "notifications/initialized":
			return rpcResponse{}, false
		default:
			s.logger.Printf("ignoring notification %q", req.Method)
			return rpcResponse{}, false
		}
	}

	id := decodeID(req.ID)
	resp := rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
	}

	switch req.Method {
	case "initialize":
		var params initializeParams
		_ = json.Unmarshal(req.Params, &params)
		version := strings.TrimSpace(params.ProtocolVersion)
		if version == "" {
			version = defaultProtocol
		}
		resp.Result = map[string]interface{}{
			"protocolVersion": version,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{
					"listChanged": false,
				},
				"resources": map[string]interface{}{
					"listChanged": false,
					"subscribe":   false,
				},
			},
			"serverInfo": map[string]interface{}{
				"name":    serverName,
				"version": serverVersion,
			},
		}
	case "ping":
		resp.Result = map[string]interface{}{}
	case "tools/list":
		resp.Result = map[string]interface{}{"tools": toolset()}
	case "tools/call":
		result, err := s.handleToolCall(ctx, req.Params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = result
		}
	case "resources/list":
		resp.Result = map[string]interface{}{"resources": resources()}
	case "resources/read":
		result, err := s.handleResourceRead(ctx, req.Params)
		if err != nil {
			resp.Error = &rpcError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = result
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found"}
	}

	return resp, true
}

func decodeID(raw json.RawMessage) interface{} {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func toolset() []tool {
	return []tool{
		{
			Name:        "get_protocol",
			Description: "Return the shared comms protocol and database details.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "create_room",
			Description: "Create a named room for a buddy cluster, incident lane, or other tactical mess.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":        map[string]interface{}{"type": "string"},
					"description": map[string]interface{}{"type": "string"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "list_rooms",
			Description: "List known rooms.",
			InputSchema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "post_message",
			Description: "Post a new message into a room, optionally as part of a thread.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"room":        map[string]interface{}{"type": "string"},
					"from":        map[string]interface{}{"type": "string"},
					"to":          map[string]interface{}{"type": "string"},
					"topic":       map[string]interface{}{"type": "string"},
					"status":      map[string]interface{}{"type": "string", "enum": []string{"info", "ask", "warn", "ack", "decision"}},
					"body":        map[string]interface{}{"type": "string"},
					"reply_to_id": map[string]interface{}{"type": "integer"},
					"refs": map[string]interface{}{
						"type":  "array",
						"items": map[string]interface{}{"type": "string"},
					},
					"mentions": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Agent call-signs nudged by this message. Structured metadata, not parsed from body.",
					},
				},
				"required": []string{"from", "topic", "status", "body"},
			},
		},
		{
			Name:        "list_messages",
			Description: "List recent room traffic. Filters compose with AND semantics.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"room":             map[string]interface{}{"type": "string"},
					"agent":            map[string]interface{}{"type": "string", "description": "DEPRECATED. Use from/to/concerns instead."},
					"from":             map[string]interface{}{"type": "string", "description": "Exact match on from_agent."},
					"to":               map[string]interface{}{"type": "string", "description": "Exact match on to_agent. \"all\" matches only literal broadcasts."},
					"concerns":         map[string]interface{}{"type": "string", "description": "Agent call-sign: matches to==concerns OR broadcasts OR mentions-contains-concerns."},
					"broadcasts_only":  map[string]interface{}{"type": "boolean", "description": "Restrict to to in (\"all\",\"\")."},
					"topic":            map[string]interface{}{"type": "string"},
					"status":           map[string]interface{}{"type": "string", "enum": []string{"info", "ask", "warn", "ack", "decision"}},
					"thread_root_id":   map[string]interface{}{"type": "integer"},
					"after_id":         map[string]interface{}{"type": "integer"},
					"before":           map[string]interface{}{"type": "string", "description": "ISO8601 exclusive upper bound on created_at."},
					"after":            map[string]interface{}{"type": "string", "description": "ISO8601 exclusive lower bound on created_at."},
					"limit":            map[string]interface{}{"type": "integer", "minimum": 1, "maximum": maxMessageLimit},
					"include_acked":    map[string]interface{}{"type": "boolean"},
					"unacked_only":     map[string]interface{}{"type": "boolean", "description": "Force exclusion of acked messages."},
					"include_archived": map[string]interface{}{"type": "boolean"},
					"has_refs":         map[string]interface{}{"type": "boolean", "description": "true = refs non-empty; false = refs empty."},
					"mentions_any": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Match messages whose mentions intersect this list.",
					},
				},
			},
		},
		{
			Name:        "search_messages",
			Description: "Full-text search over message body and topic. Returns snippets (not full bodies). Use for recalling past content; use list_messages for current-state polling.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query":            map[string]interface{}{"type": "string", "description": "FTS5 MATCH expression (or substring when FTS5 unavailable)."},
					"room":             map[string]interface{}{"type": "string"},
					"topic":            map[string]interface{}{"type": "string"},
					"agent":            map[string]interface{}{"type": "string"},
					"after":            map[string]interface{}{"type": "string", "description": "ISO8601 timestamp; include only messages created at/after."},
					"before":           map[string]interface{}{"type": "string", "description": "ISO8601 timestamp; include only messages created before."},
					"limit":            map[string]interface{}{"type": "integer", "minimum": 1, "maximum": maxSearchLimit},
					"include_acked":    map[string]interface{}{"type": "boolean"},
					"include_archived": map[string]interface{}{"type": "boolean"},
					"snippet":          map[string]interface{}{"type": "boolean", "description": "Return ~160-char FTS snippet instead of truncated body. Default true."},
					"order":            map[string]interface{}{"type": "string", "enum": []string{"recent", "relevance"}, "description": "Default 'recent'. 'relevance' uses FTS5 bm25 ordering when available."},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "ack_message",
			Description: "Mark a message as acknowledged by an agent.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id":    map[string]interface{}{"type": "integer"},
					"agent": map[string]interface{}{"type": "string"},
				},
				"required": []string{"id", "agent"},
			},
		},
		{
			Name:        "compact_messages",
			Description: "Archive older acknowledged traffic while keeping recent room chatter live.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"keep_recent": map[string]interface{}{"type": "integer", "minimum": 1},
				},
			},
		},
		{
			Name:        "pin_message",
			Description: "Pin a message for at-least-once delivery (broadcast) or always-visible ambient display (snippet). Optionally target a specific instance call-sign.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message_id":      map[string]interface{}{"type": "integer"},
					"kind":            map[string]interface{}{"type": "string", "enum": []string{"broadcast", "snippet"}, "description": "broadcast=at-least-once per instance; snippet=always visible ambient"},
					"pinned_by":       map[string]interface{}{"type": "string"},
					"target_instance": map[string]interface{}{"type": "string", "description": "Call-sign to target; omit for broadcast to all instances."},
					"note":            map[string]interface{}{"type": "string"},
				},
				"required": []string{"message_id", "pinned_by"},
			},
		},
		{
			Name:        "unpin_message",
			Description: "Remove an active pin by pin_id.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pin_id":      map[string]interface{}{"type": "integer"},
					"unpinned_by": map[string]interface{}{"type": "string"},
				},
				"required": []string{"pin_id", "unpinned_by"},
			},
		},
		{
			Name:        "list_pins",
			Description: "List active pins. Pass target_instance to see pins for that instance plus broadcast pins.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"kind":             map[string]interface{}{"type": "string", "enum": []string{"broadcast", "snippet"}},
					"target_instance":  map[string]interface{}{"type": "string", "description": "Return broadcast pins plus pins targeted at this call-sign."},
					"room":             map[string]interface{}{"type": "string"},
					"include_unpinned": map[string]interface{}{"type": "boolean"},
					"limit":            map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 200},
				},
			},
		},
		{
			Name:        "ack_pin",
			Description: "Per-instance acknowledgment of a broadcast pin. Records that this instance has processed it.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pin_id":      map[string]interface{}{"type": "integer"},
					"instance_id": map[string]interface{}{"type": "string"},
					"acked_by":    map[string]interface{}{"type": "string"},
				},
				"required": []string{"pin_id", "instance_id"},
			},
		},
		{
			Name:        "touch_pin",
			Description: "Fetch the full content of a pin (especially useful for snippet pins that only show a short preview). Returns complete message body.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pin_id": map[string]interface{}{"type": "integer"},
				},
				"required": []string{"pin_id"},
			},
		},
		{
			Name:        "register_instance",
			Description: "Register or heartbeat an agent instance. Required for broadcast pins to compute fully_delivered status.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"instance_id": map[string]interface{}{"type": "string"},
					"agent_name":  map[string]interface{}{"type": "string"},
				},
				"required": []string{"instance_id"},
			},
		},
	}
}

func resources() []resource {
	return []resource{
		{
			URI:         "commsync://protocol",
			Name:        "Comms Protocol",
			Description: "The canonical chat protocol for this server.",
			MimeType:    "text/markdown",
		},
		{
			URI:         "commsync://rooms",
			Name:        "Rooms",
			Description: "Known chat rooms.",
			MimeType:    "application/json",
		},
		{
			URI:         "commsync://messages/recent",
			Name:        "Recent Messages",
			Description: "A markdown rendering of recent live messages.",
			MimeType:    "text/markdown",
		},
	}
}

func (s *server) handleToolCall(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}

	switch params.Name {
	case "get_protocol":
		return s.toolGetProtocol(ctx)
	case "create_room":
		return s.toolCreateRoom(ctx, params.Arguments)
	case "list_rooms":
		return s.toolListRooms(ctx)
	case "post_message":
		return s.toolPostMessage(ctx, params.Arguments)
	case "list_messages":
		return s.toolListMessages(ctx, params.Arguments)
	case "search_messages":
		return s.toolSearchMessages(ctx, params.Arguments)
	case "ack_message":
		return s.toolAckMessage(ctx, params.Arguments)
	case "compact_messages":
		return s.toolCompactMessages(ctx, params.Arguments)
	case "pin_message":
		return s.toolPinMessage(ctx, params.Arguments)
	case "unpin_message":
		return s.toolUnpinMessage(ctx, params.Arguments)
	case "list_pins":
		return s.toolListPins(ctx, params.Arguments)
	case "ack_pin":
		return s.toolAckPin(ctx, params.Arguments)
	case "touch_pin":
		return s.toolTouchPin(ctx, params.Arguments)
	case "register_instance":
		return s.toolRegisterInstance(ctx, params.Arguments)
	default:
		return nil, fmt.Errorf("unknown tool %q", params.Name)
	}
}

func (s *server) handleResourceRead(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}

	switch params.URI {
	case "commsync://protocol":
		return map[string]interface{}{
			"contents": []map[string]interface{}{{
				"uri":      params.URI,
				"mimeType": "text/markdown",
				"text":     protocolText,
			}},
		}, nil
	case "commsync://rooms":
		rooms, err := s.listRooms(ctx)
		if err != nil {
			return nil, err
		}
		roomJSON, err := json.MarshalIndent(rooms, "", "  ")
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"contents": []map[string]interface{}{{
				"uri":      params.URI,
				"mimeType": "application/json",
				"text":     string(roomJSON),
			}},
		}, nil
	case "commsync://messages/recent":
		body, err := s.renderRecentMessages(ctx, defaultRecentResource)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"contents": []map[string]interface{}{{
				"uri":      params.URI,
				"mimeType": "text/markdown",
				"text":     body,
			}},
		}, nil
	default:
		return nil, fmt.Errorf("unknown resource %q", params.URI)
	}
}

func (s *server) toolGetProtocol(ctx context.Context) (map[string]interface{}, error) {
	count, err := s.countLiveMessages(ctx)
	if err != nil {
		return nil, err
	}
	rooms, err := s.countRooms(ctx)
	if err != nil {
		return nil, err
	}
	payload := map[string]interface{}{
		"db_path":       s.dbPath,
		"protocol_text": protocolText,
		"live_messages": count,
		"rooms":         rooms,
	}
	return toolResult("Protocol loaded.", payload), nil
}

func (s *server) toolCreateRoom(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	name := normalizeRoomName(args.Name)
	if name == "" {
		return nil, errors.New("name is required")
	}
	desc := strings.TrimSpace(args.Description)
	if desc == "" {
		desc = "No description supplied. Humanity remains consistent."
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO rooms(name, description, created_at) VALUES (?, ?, ?) ON CONFLICT(name) DO NOTHING`, name, desc, now); err != nil {
		return nil, err
	}
	return toolResult(fmt.Sprintf("Room %q ready.", name), map[string]interface{}{
		"name":        name,
		"description": desc,
		"created_at":  now,
	}), nil
}

func (s *server) toolListRooms(ctx context.Context) (map[string]interface{}, error) {
	rooms, err := s.listRooms(ctx)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(rooms))
	for _, room := range rooms {
		lines = append(lines, fmt.Sprintf("%s | %s", room.Name, room.Description))
	}
	text := "No rooms."
	if len(lines) > 0 {
		text = strings.Join(lines, "\n")
	}
	return toolResult(text, map[string]interface{}{"rooms": rooms}), nil
}

func (s *server) toolPostMessage(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		Room      string    `json:"room"`
		From      string    `json:"from"`
		To        string    `json:"to"`
		Topic     string    `json:"topic"`
		Status    string    `json:"status"`
		Body      string    `json:"body"`
		ReplyToID flexInt64 `json:"reply_to_id"`
		Refs      []string  `json:"refs"`
		Mentions  []string  `json:"mentions"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}

	roomName := normalizeRoomName(args.Room)
	if roomName == "" {
		roomName = defaultRoomName
	}
	if err := s.ensureRoomExists(ctx, roomName); err != nil {
		return nil, err
	}

	from := strings.TrimSpace(args.From)
	to := strings.TrimSpace(args.To)
	if to == "" {
		to = "all"
	}
	if err := validateMessageArgs(from, to, strings.TrimSpace(args.Topic), args.Status, strings.TrimSpace(args.Body)); err != nil {
		return nil, err
	}

	threadRootID, replyToID, err := s.resolveThreading(ctx, roomName, int64(args.ReplyToID))
	if err != nil {
		return nil, err
	}

	refsJSON, err := json.Marshal(normalizeRefs(args.Refs))
	if err != nil {
		return nil, err
	}
	mentions := normalizeMentions(args.Mentions)
	mentionsJSON, err := json.Marshal(mentions)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
INSERT INTO messages(created_at, room_name, from_agent, to_agent, topic, status, thread_root_id, reply_to_id, refs_json, mentions_json, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now, roomName, from, to, strings.TrimSpace(args.Topic), args.Status, nullableInt64Value(threadRootID), nullableInt64Value(replyToID), string(refsJSON), string(mentionsJSON), strings.TrimSpace(args.Body),
	)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	if host, _ := ctx.Value(ctxKeySourceHost).(string); host != "" {
		s.logger.Printf("post_message id=%d from=%s host=%s", id, from, host)
	}

	payload := map[string]interface{}{
		"id":             id,
		"created_at":     now,
		"room":           roomName,
		"from":           from,
		"to":             to,
		"topic":          strings.TrimSpace(args.Topic),
		"status":         args.Status,
		"thread_root_id": threadRootID,
		"reply_to_id":    replyToID,
		"mentions":       mentions,
	}
	return toolResult(fmt.Sprintf("Message %d posted to room %q.", id, roomName), payload), nil
}

func (s *server) toolListMessages(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		Room            string    `json:"room"`
		Agent           string    `json:"agent"`
		From            string    `json:"from"`
		To              string    `json:"to"`
		Concerns        string    `json:"concerns"`
		BroadcastsOnly  flexBool  `json:"broadcasts_only"`
		Topic           string    `json:"topic"`
		Status          string    `json:"status"`
		ThreadRootID    flexInt64 `json:"thread_root_id"`
		AfterID         flexInt64 `json:"after_id"`
		Before          string    `json:"before"`
		After           string    `json:"after"`
		Limit           flexInt   `json:"limit"`
		IncludeAcked    flexBool  `json:"include_acked"`
		UnackedOnly     flexBool  `json:"unacked_only"`
		IncludeArchived flexBool  `json:"include_archived"`
		HasRefs         *flexBool `json:"has_refs"`
		MentionsAny     []string  `json:"mentions_any"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}

	limit := int(args.Limit)
	if limit <= 0 {
		limit = defaultMessageLimit
	}
	if limit > maxMessageLimit {
		limit = maxMessageLimit
	}

	agent := strings.TrimSpace(args.Agent)
	if agent != "" {
		s.logger.Printf("list_messages: deprecated 'agent' filter used (value=%q); prefer from/to/concerns", agent)
	}
	if args.Status != "" {
		switch args.Status {
		case "info", "ask", "warn", "ack", "decision":
		default:
			return nil, fmt.Errorf("invalid status %q", args.Status)
		}
	}

	msgs, err := s.listMessages(ctx, listFilters{
		Room:            normalizeRoomName(args.Room),
		Agent:           agent,
		From:            strings.TrimSpace(args.From),
		To:              strings.TrimSpace(args.To),
		Concerns:        strings.TrimSpace(args.Concerns),
		BroadcastsOnly:  bool(args.BroadcastsOnly),
		Topic:           strings.TrimSpace(args.Topic),
		Status:          args.Status,
		ThreadRootID:    int64(args.ThreadRootID),
		AfterID:         int64(args.AfterID),
		Before:          strings.TrimSpace(args.Before),
		After:           strings.TrimSpace(args.After),
		IncludeAcked:    bool(args.IncludeAcked),
		UnackedOnly:     bool(args.UnackedOnly),
		IncludeArchived: bool(args.IncludeArchived),
		HasRefs:         flexBoolPtr(args.HasRefs),
		MentionsAny:     normalizeMentions(args.MentionsAny),
		Limit:           limit,
	})
	if err != nil {
		return nil, err
	}

	return toolResult(renderMessages(msgs), map[string]interface{}{
		"messages": msgs,
		"count":    len(msgs),
	}), nil
}

// searchHit is the returned shape for search_messages — deliberately leaner than `message`
// to keep tool-result budgets small (no full body by default, no refs, no thread pointers).
type searchHit struct {
	ID            int64  `json:"id"`
	CreatedAt     string `json:"created_at"`
	Room          string `json:"room"`
	From          string `json:"from"`
	To            string `json:"to"`
	Topic         string `json:"topic"`
	Status        string `json:"status"`
	Snippet       string `json:"snippet"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`
}

func (s *server) toolSearchMessages(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		Query           string    `json:"query"`
		Room            string    `json:"room"`
		Topic           string    `json:"topic"`
		Agent           string    `json:"agent"`
		After           string    `json:"after"`
		Before          string    `json:"before"`
		Limit           flexInt   `json:"limit"`
		IncludeAcked    *flexBool `json:"include_acked"`
		IncludeArchived flexBool  `json:"include_archived"`
		Snippet         *flexBool `json:"snippet"`
		Order           string    `json:"order"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}

	query := strings.TrimSpace(args.Query)
	if query == "" {
		return nil, errors.New("query is required")
	}

	limit := int(args.Limit)
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	// Search defaults mirror list_messages where it makes sense, but search is
	// recall-oriented — acked messages are usually wanted, so default ON.
	includeAcked := true
	if args.IncludeAcked != nil {
		includeAcked = bool(*args.IncludeAcked)
	}
	wantSnippet := true
	if args.Snippet != nil {
		wantSnippet = bool(*args.Snippet)
	}
	order := strings.ToLower(strings.TrimSpace(args.Order))
	if order == "" {
		order = "recent"
	}
	if order != "recent" && order != "relevance" {
		return nil, fmt.Errorf("order must be 'recent' or 'relevance', got %q", order)
	}

	hits, engine, err := s.searchMessages(ctx, searchArgs{
		Query:           query,
		Room:            normalizeRoomName(args.Room),
		Topic:           strings.TrimSpace(args.Topic),
		Agent:           strings.TrimSpace(args.Agent),
		After:           strings.TrimSpace(args.After),
		Before:          strings.TrimSpace(args.Before),
		IncludeAcked:    includeAcked,
		IncludeArchived: bool(args.IncludeArchived),
		Snippet:         wantSnippet,
		Order:           order,
		Limit:           limit,
	})
	if err != nil {
		return nil, err
	}

	summary := fmt.Sprintf("search (%s) matched %d message(s) for query %q.", engine, len(hits), query)
	return toolResult(renderSearchHits(hits, summary), map[string]interface{}{
		"count":    len(hits),
		"engine":   engine,
		"messages": hits,
	}), nil
}

type searchArgs struct {
	Query           string
	Room            string
	Topic           string
	Agent           string
	After           string
	Before          string
	IncludeAcked    bool
	IncludeArchived bool
	Snippet         bool
	Order           string
	Limit           int
}

func (s *server) searchMessages(ctx context.Context, a searchArgs) ([]searchHit, string, error) {
	if s.fts5 {
		hits, err := s.searchMessagesFTS(ctx, a)
		if err == nil {
			return hits, "fts5", nil
		}
		// Malformed MATCH expression is a user error — surface it directly rather than
		// silently degrading to LIKE (which would match different things).
		if isFTSSyntaxError(err) {
			return nil, "fts5", fmt.Errorf("invalid FTS5 query %q: %w", a.Query, err)
		}
		s.logger.Printf("fts5 search failed, falling back to LIKE: %v", err)
	}
	hits, err := s.searchMessagesLike(ctx, a)
	if err != nil {
		return nil, "like", err
	}
	return hits, "like", nil
}

func isFTSSyntaxError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "fts5:") || strings.Contains(msg, "syntax error") || strings.Contains(msg, "no such column")
}

func (s *server) searchMessagesFTS(ctx context.Context, a searchArgs) ([]searchHit, error) {
	var b strings.Builder
	var args []interface{}

	snippetExpr := "substr(m.body, 1, ?)"
	if a.Snippet {
		// snippet(tbl, colIdx, startMark, endMark, ellipsis, tokens)
		// colIdx -1 picks the best-matching column across indexed cols.
		snippetExpr = "snippet(messages_fts, -1, '[', ']', '…', ?)"
		args = append(args, snippetTokens(searchSnippetChars))
	} else {
		args = append(args, searchBodyTruncChars)
	}

	b.WriteString(`
SELECT m.id, m.created_at, m.room_name, m.from_agent, m.to_agent, m.topic, m.status,
       ` + snippetExpr + ` AS snip,
       length(m.body) AS blen
FROM messages_fts f
JOIN messages m ON m.id = f.rowid
WHERE messages_fts MATCH ?`)
	args = append(args, a.Query)

	if !a.IncludeArchived {
		b.WriteString(` AND m.archived_at IS NULL`)
	}
	if !a.IncludeAcked {
		b.WriteString(` AND m.acked_at IS NULL`)
	}
	if a.Room != "" {
		b.WriteString(` AND m.room_name = ?`)
		args = append(args, a.Room)
	}
	if a.Topic != "" {
		b.WriteString(` AND m.topic = ?`)
		args = append(args, a.Topic)
	}
	if a.Agent != "" {
		b.WriteString(` AND (m.from_agent = ? OR m.to_agent = ? OR m.to_agent = 'all')`)
		args = append(args, a.Agent, a.Agent)
	}
	if a.After != "" {
		b.WriteString(` AND m.created_at >= ?`)
		args = append(args, a.After)
	}
	if a.Before != "" {
		b.WriteString(` AND m.created_at < ?`)
		args = append(args, a.Before)
	}

	if a.Order == "relevance" {
		b.WriteString(` ORDER BY bm25(messages_fts), m.created_at DESC, m.id DESC`)
	} else {
		b.WriteString(` ORDER BY m.created_at DESC, m.id DESC`)
	}
	b.WriteString(` LIMIT ?`)
	args = append(args, a.Limit)

	return s.scanSearchHits(ctx, b.String(), args, a.Snippet)
}

func (s *server) searchMessagesLike(ctx context.Context, a searchArgs) ([]searchHit, error) {
	var b strings.Builder
	var args []interface{}

	b.WriteString(`
SELECT m.id, m.created_at, m.room_name, m.from_agent, m.to_agent, m.topic, m.status,
       substr(m.body, 1, ?) AS snip,
       length(m.body) AS blen
FROM messages m
WHERE (m.body LIKE ? OR m.topic LIKE ?)`)
	args = append(args, searchBodyTruncChars, "%"+a.Query+"%", "%"+a.Query+"%")

	if !a.IncludeArchived {
		b.WriteString(` AND m.archived_at IS NULL`)
	}
	if !a.IncludeAcked {
		b.WriteString(` AND m.acked_at IS NULL`)
	}
	if a.Room != "" {
		b.WriteString(` AND m.room_name = ?`)
		args = append(args, a.Room)
	}
	if a.Topic != "" {
		b.WriteString(` AND m.topic = ?`)
		args = append(args, a.Topic)
	}
	if a.Agent != "" {
		b.WriteString(` AND (m.from_agent = ? OR m.to_agent = ? OR m.to_agent = 'all')`)
		args = append(args, a.Agent, a.Agent)
	}
	if a.After != "" {
		b.WriteString(` AND m.created_at >= ?`)
		args = append(args, a.After)
	}
	if a.Before != "" {
		b.WriteString(` AND m.created_at < ?`)
		args = append(args, a.Before)
	}
	// LIKE engine has no relevance; always recency.
	b.WriteString(` ORDER BY m.created_at DESC, m.id DESC LIMIT ?`)
	args = append(args, a.Limit)

	return s.scanSearchHits(ctx, b.String(), args, a.Snippet)
}

func (s *server) scanSearchHits(ctx context.Context, sqlText string, args []interface{}, snippetMode bool) ([]searchHit, error) {
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []searchHit{}
	for rows.Next() {
		var h searchHit
		var snip string
		var blen int
		if err := rows.Scan(&h.ID, &h.CreatedAt, &h.Room, &h.From, &h.To, &h.Topic, &h.Status, &snip, &blen); err != nil {
			return nil, err
		}
		h.Snippet = snip
		// snippet mode uses FTS5 snippet() with its own ellipsis; the body_truncated
		// flag is only meaningful for the substring-truncation path.
		if !snippetMode && blen > searchBodyTruncChars {
			h.BodyTruncated = true
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// snippetTokens converts a target char budget into an FTS5 snippet() token count.
// Rough average English token ~= 5 chars + space. Keep it modest to stay under budget.
func snippetTokens(chars int) int {
	n := chars / 6
	if n < 8 {
		n = 8
	}
	if n > 32 {
		n = 32
	}
	return n
}

func renderSearchHits(hits []searchHit, summary string) string {
	if len(hits) == 0 {
		return summary + " No matches."
	}
	var b strings.Builder
	b.WriteString(summary)
	for _, h := range hits {
		b.WriteString("\n\n[")
		b.WriteString(strconv.FormatInt(h.ID, 10))
		b.WriteString("] ")
		b.WriteString(h.CreatedAt)
		b.WriteString(" | room:")
		b.WriteString(h.Room)
		b.WriteString(" | from:")
		b.WriteString(h.From)
		b.WriteString(" | topic:")
		b.WriteString(h.Topic)
		b.WriteString(" | status:")
		b.WriteString(h.Status)
		b.WriteString("\n")
		b.WriteString(h.Snippet)
		if h.BodyTruncated {
			b.WriteString(" …[truncated]")
		}
	}
	return b.String()
}

func (s *server) toolAckMessage(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		ID    flexInt64 `json:"id"`
		Agent string    `json:"agent"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.ID <= 0 {
		return nil, errors.New("id must be > 0")
	}
	agent := strings.TrimSpace(args.Agent)
	if agent == "" {
		return nil, errors.New("agent is required")
	}

	id := int64(args.ID)
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
UPDATE messages
SET acked_at = COALESCE(acked_at, ?),
    acked_by = COALESCE(acked_by, ?)
WHERE id = ?`,
		now, agent, id,
	)
	if err != nil {
		return nil, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return nil, fmt.Errorf("message %d not found", id)
	}

	return toolResult(fmt.Sprintf("Message %d acknowledged by %s.", id, agent), map[string]interface{}{
		"id":       id,
		"acked_at": now,
		"acked_by": agent,
	}), nil
}

func (s *server) toolCompactMessages(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		KeepRecent flexInt `json:"keep_recent"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}
	keep := int(args.KeepRecent)
	if keep <= 0 {
		keep = defaultCompactKeep
	}

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
UPDATE messages
SET archived_at = ?
WHERE id IN (
    SELECT id
    FROM messages
    WHERE archived_at IS NULL
      AND acked_at IS NOT NULL
      AND id NOT IN (
          SELECT id
          FROM messages
          WHERE archived_at IS NULL
          ORDER BY created_at DESC, id DESC
          LIMIT ?
      )
      AND id NOT IN (SELECT message_id FROM pinned_messages WHERE unpinned_at IS NULL)
)`, now, keep)
	if err != nil {
		return nil, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}

	return toolResult(fmt.Sprintf("Archived %d acknowledged message(s).", rows), map[string]interface{}{
		"archived_count": rows,
		"keep_recent":    keep,
	}), nil
}

func (s *server) toolPinMessage(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		MessageID      flexInt64 `json:"message_id"`
		Kind           string    `json:"kind"`
		PinnedBy       string    `json:"pinned_by"`
		TargetInstance string    `json:"target_instance"`
		Note           string    `json:"note"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.MessageID <= 0 {
		return nil, errors.New("message_id must be > 0")
	}
	pinnedBy := strings.TrimSpace(args.PinnedBy)
	if pinnedBy == "" {
		return nil, errors.New("pinned_by is required")
	}
	kind := strings.TrimSpace(args.Kind)
	if kind == "" {
		kind = "broadcast"
	}
	if kind != "broadcast" && kind != "snippet" {
		return nil, fmt.Errorf("kind must be 'broadcast' or 'snippet', got %q", kind)
	}

	msgID := int64(args.MessageID)
	// Check message exists
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM messages WHERE id = ?`, msgID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("message %d not found", msgID)
		}
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var targetInstance interface{}
	if t := strings.TrimSpace(args.TargetInstance); t != "" {
		targetInstance = t
	}
	note := strings.TrimSpace(args.Note)

	res, err := s.db.ExecContext(ctx, `
INSERT INTO pinned_messages(message_id, kind, pinned_at, pinned_by, target_instance, note)
VALUES (?, ?, ?, ?, ?, ?)`, msgID, kind, now, pinnedBy, targetInstance, note)
	if err != nil {
		return nil, err
	}
	pinID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	payload := map[string]interface{}{
		"pin_id":     pinID,
		"message_id": msgID,
		"kind":       kind,
		"pinned_at":  now,
		"pinned_by":  pinnedBy,
		"note":       note,
	}
	if targetInstance != nil {
		payload["target_instance"] = targetInstance
	}
	return toolResult(fmt.Sprintf("Message %d pinned (pin_id=%d, kind=%s).", msgID, pinID, kind), payload), nil
}

func (s *server) toolUnpinMessage(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		PinID      flexInt64 `json:"pin_id"`
		UnpinnedBy string    `json:"unpinned_by"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.PinID <= 0 {
		return nil, errors.New("pin_id must be > 0")
	}
	unpinnedBy := strings.TrimSpace(args.UnpinnedBy)
	if unpinnedBy == "" {
		return nil, errors.New("unpinned_by is required")
	}

	pinID := int64(args.PinID)
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
UPDATE pinned_messages SET unpinned_at = ?, unpinned_by = ?
WHERE id = ? AND unpinned_at IS NULL`, now, unpinnedBy, pinID)
	if err != nil {
		return nil, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}

	if rows == 0 {
		return toolResult(fmt.Sprintf("Pin %d not found or already unpinned.", pinID), map[string]interface{}{
			"pin_id":   pinID,
			"unpinned": false,
		}), nil
	}
	return toolResult(fmt.Sprintf("Pin %d unpinned.", pinID), map[string]interface{}{
		"pin_id":      pinID,
		"unpinned":    true,
		"unpinned_at": now,
	}), nil
}

func (s *server) toolListPins(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		Kind            string    `json:"kind"`
		TargetInstance  string    `json:"target_instance"`
		Room            string    `json:"room"`
		IncludeUnpinned flexBool  `json:"include_unpinned"`
		Limit           flexInt   `json:"limit"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}

	limit := int(args.Limit)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	var qb strings.Builder
	var qargs []interface{}

	qb.WriteString(`
SELECT pm.id, pm.kind, pm.pinned_at, pm.pinned_by, pm.target_instance, pm.note,
       pm.unpinned_at, pm.unpinned_by,
       m.id, m.created_at, m.room_name, m.from_agent, m.to_agent, m.topic,
       m.status, m.thread_root_id, m.reply_to_id, m.refs_json, m.mentions_json, m.body, m.acked_at, m.acked_by
FROM pinned_messages pm
JOIN messages m ON m.id = pm.message_id
WHERE 1=1`)

	if !bool(args.IncludeUnpinned) {
		qb.WriteString(` AND pm.unpinned_at IS NULL`)
	}

	targetInstance := strings.TrimSpace(args.TargetInstance)
	if targetInstance != "" {
		qb.WriteString(` AND (pm.target_instance IS NULL OR pm.target_instance = ?)`)
		qargs = append(qargs, targetInstance)
	} else {
		qb.WriteString(` AND pm.target_instance IS NULL`)
	}

	if k := strings.TrimSpace(args.Kind); k != "" {
		qb.WriteString(` AND pm.kind = ?`)
		qargs = append(qargs, k)
	}

	if r := normalizeRoomName(args.Room); r != "" {
		qb.WriteString(` AND m.room_name = ?`)
		qargs = append(qargs, r)
	}

	qb.WriteString(` ORDER BY pm.pinned_at DESC LIMIT ?`)
	qargs = append(qargs, limit)

	rows, err := s.db.QueryContext(ctx, qb.String(), qargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pins []pinnedMsg
	for rows.Next() {
		var p pinnedMsg
		var targetInst sql.NullString
		var unpinnedAt sql.NullString
		var unpinnedBy sql.NullString
		var msg message
		var refsJSON, mentionsJSON string
		var threadRootID, replyToID sql.NullInt64
		var ackedAt, ackedBy sql.NullString

		if err := rows.Scan(
			&p.PinID, &p.Kind, &p.PinnedAt, &p.PinnedBy, &targetInst, &p.Note,
			&unpinnedAt, &unpinnedBy,
			&msg.ID, &msg.CreatedAt, &msg.Room, &msg.From, &msg.To, &msg.Topic,
			&msg.Status, &threadRootID, &replyToID, &refsJSON, &mentionsJSON, &msg.Body, &ackedAt, &ackedBy,
		); err != nil {
			return nil, err
		}
		if targetInst.Valid {
			p.TargetInstance = &targetInst.String
		}
		if unpinnedAt.Valid {
			p.UnpinnedAt = &unpinnedAt.String
		}
		if unpinnedBy.Valid {
			p.UnpinnedBy = &unpinnedBy.String
		}
		if err := json.Unmarshal([]byte(refsJSON), &msg.Refs); err != nil {
			msg.Refs = []string{}
		}
		if mentionsJSON == "" {
			msg.Mentions = []string{}
		} else if err := json.Unmarshal([]byte(mentionsJSON), &msg.Mentions); err != nil {
			msg.Mentions = []string{}
		}
		if msg.Mentions == nil {
			msg.Mentions = []string{}
		}
		if threadRootID.Valid {
			msg.ThreadRootID = &threadRootID.Int64
		}
		if replyToID.Valid {
			msg.ReplyToID = &replyToID.Int64
		}
		if ackedAt.Valid {
			msg.AckedAt = &ackedAt.String
		}
		if ackedBy.Valid {
			msg.AckedBy = &ackedBy.String
		}
		p.Message = msg
		pins = append(pins, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return toolResult(fmt.Sprintf("%d active pin(s).", len(pins)), map[string]interface{}{
		"pins":  pins,
		"count": len(pins),
	}), nil
}

func (s *server) toolAckPin(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		PinID      flexInt64 `json:"pin_id"`
		InstanceID string    `json:"instance_id"`
		AckedBy    string    `json:"acked_by"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.PinID <= 0 {
		return nil, errors.New("pin_id must be > 0")
	}
	instanceID := strings.TrimSpace(args.InstanceID)
	if instanceID == "" {
		return nil, errors.New("instance_id is required")
	}
	ackedBy := strings.TrimSpace(args.AckedBy)
	if ackedBy == "" {
		ackedBy = instanceID
	}

	pinID := int64(args.PinID)
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.ExecContext(ctx, `
INSERT INTO pin_acks(pin_id, instance_id, acked_at, acked_by)
VALUES (?, ?, ?, ?)
ON CONFLICT(pin_id, instance_id) DO UPDATE SET acked_at = excluded.acked_at, acked_by = excluded.acked_by`,
		pinID, instanceID, now, ackedBy)
	if err != nil {
		return nil, err
	}

	// Compute fully_delivered: all active (non-retired) instances have acked
	var ackedCount, totalInstances int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pin_acks WHERE pin_id = ?`, pinID).Scan(&ackedCount); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_instances WHERE retired_at IS NULL`).Scan(&totalInstances); err != nil {
		return nil, err
	}

	fullyDelivered := totalInstances > 0 && ackedCount >= totalInstances

	return toolResult(fmt.Sprintf("Pin %d acked by instance %s.", pinID, instanceID), map[string]interface{}{
		"pin_id":          pinID,
		"instance_id":     instanceID,
		"acked_at":        now,
		"fully_delivered": fullyDelivered,
		"acked_count":     ackedCount,
		"total_instances": totalInstances,
	}), nil
}

func (s *server) toolTouchPin(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		PinID flexInt64 `json:"pin_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	if args.PinID <= 0 {
		return nil, errors.New("pin_id must be > 0")
	}
	pinID := int64(args.PinID)

	var p pinnedMsg
	var targetInst sql.NullString
	var unpinnedAt sql.NullString
	var unpinnedBy sql.NullString
	var msg message
	var refsJSON, mentionsJSON string
	var threadRootID, replyToID sql.NullInt64
	var ackedAt, ackedBy sql.NullString

	err := s.db.QueryRowContext(ctx, `
SELECT pm.id, pm.kind, pm.pinned_at, pm.pinned_by, pm.target_instance, pm.note,
       pm.unpinned_at, pm.unpinned_by,
       m.id, m.created_at, m.room_name, m.from_agent, m.to_agent, m.topic,
       m.status, m.thread_root_id, m.reply_to_id, m.refs_json, m.mentions_json, m.body, m.acked_at, m.acked_by
FROM pinned_messages pm
JOIN messages m ON m.id = pm.message_id
WHERE pm.id = ?`, pinID).Scan(
		&p.PinID, &p.Kind, &p.PinnedAt, &p.PinnedBy, &targetInst, &p.Note,
		&unpinnedAt, &unpinnedBy,
		&msg.ID, &msg.CreatedAt, &msg.Room, &msg.From, &msg.To, &msg.Topic,
		&msg.Status, &threadRootID, &replyToID, &refsJSON, &mentionsJSON, &msg.Body, &ackedAt, &ackedBy,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("pin %d not found", pinID)
		}
		return nil, err
	}

	if targetInst.Valid {
		p.TargetInstance = &targetInst.String
	}
	if unpinnedAt.Valid {
		p.UnpinnedAt = &unpinnedAt.String
	}
	if unpinnedBy.Valid {
		p.UnpinnedBy = &unpinnedBy.String
	}
	if err := json.Unmarshal([]byte(refsJSON), &msg.Refs); err != nil {
		msg.Refs = []string{}
	}
	if mentionsJSON == "" {
		msg.Mentions = []string{}
	} else if err := json.Unmarshal([]byte(mentionsJSON), &msg.Mentions); err != nil {
		msg.Mentions = []string{}
	}
	if msg.Mentions == nil {
		msg.Mentions = []string{}
	}
	if threadRootID.Valid {
		msg.ThreadRootID = &threadRootID.Int64
	}
	if replyToID.Valid {
		msg.ReplyToID = &replyToID.Int64
	}
	if ackedAt.Valid {
		msg.AckedAt = &ackedAt.String
	}
	if ackedBy.Valid {
		msg.AckedBy = &ackedBy.String
	}
	p.Message = msg

	return toolResult(fmt.Sprintf("Pin %d.", pinID), map[string]interface{}{"pin": p}), nil
}

func (s *server) toolRegisterInstance(ctx context.Context, raw json.RawMessage) (map[string]interface{}, error) {
	var args struct {
		InstanceID string `json:"instance_id"`
		AgentName  string `json:"agent_name"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	instanceID := strings.TrimSpace(args.InstanceID)
	if instanceID == "" {
		return nil, errors.New("instance_id is required")
	}
	agentName := strings.TrimSpace(args.AgentName)
	if agentName == "" {
		agentName = instanceID
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Read existing first_seen_at
	var firstSeenAt string
	err := s.db.QueryRowContext(ctx, `SELECT first_seen_at FROM agent_instances WHERE instance_id = ?`, instanceID).Scan(&firstSeenAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			firstSeenAt = now
		} else {
			return nil, err
		}
	}

	_, err = s.db.ExecContext(ctx, `
INSERT INTO agent_instances(instance_id, agent_name, first_seen_at, last_seen_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(instance_id) DO UPDATE SET agent_name = excluded.agent_name, last_seen_at = excluded.last_seen_at`,
		instanceID, agentName, firstSeenAt, now)
	if err != nil {
		return nil, err
	}

	return toolResult(fmt.Sprintf("Instance %s registered.", instanceID), map[string]interface{}{
		"instance_id":   instanceID,
		"agent_name":    agentName,
		"first_seen_at": firstSeenAt,
		"last_seen_at":  now,
	}), nil
}

func (s *server) ensureRoomExists(ctx context.Context, roomName string) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO rooms(name, description, created_at) VALUES (?, ?, ?) ON CONFLICT(name) DO NOTHING`, roomName, "Autocreated room.", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	_, _ = res.RowsAffected()
	return nil
}

func (s *server) resolveThreading(ctx context.Context, roomName string, replyToID int64) (*int64, *int64, error) {
	if replyToID <= 0 {
		return nil, nil, nil
	}

	var room string
	var root sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT room_name, thread_root_id FROM messages WHERE id = ?`, replyToID).Scan(&room, &root); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("reply_to_id %d not found", replyToID)
		}
		return nil, nil, err
	}
	if room != roomName {
		return nil, nil, fmt.Errorf("reply_to_id %d belongs to room %q, not %q", replyToID, room, roomName)
	}

	reply := replyToID
	if root.Valid && root.Int64 > 0 {
		threadRoot := root.Int64
		return &threadRoot, &reply, nil
	}
	threadRoot := replyToID
	return &threadRoot, &reply, nil
}

func nullableInt64Value(v *int64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func (s *server) listRooms(ctx context.Context) ([]room, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, description, created_at FROM rooms ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []room
	for rows.Next() {
		var r room
		if err := rows.Scan(&r.Name, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *server) countRooms(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rooms`).Scan(&count)
	return count, err
}

func (s *server) countLiveMessages(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE archived_at IS NULL`).Scan(&count)
	return count, err
}

func (s *server) listMessages(ctx context.Context, filters listFilters) ([]message, error) {
	var b strings.Builder
	var args []interface{}
	b.WriteString(`
SELECT id, created_at, room_name, from_agent, to_agent, topic, status, thread_root_id, reply_to_id, refs_json, mentions_json, body, acked_at, acked_by
FROM messages
WHERE 1 = 1`)

	if !filters.IncludeArchived {
		b.WriteString(` AND archived_at IS NULL`)
	}
	// unacked_only overrides include_acked; default behavior keeps excluding acked.
	if filters.UnackedOnly || !filters.IncludeAcked {
		b.WriteString(` AND acked_at IS NULL`)
	}
	if filters.Room != "" {
		b.WriteString(` AND room_name = ?`)
		args = append(args, filters.Room)
	}
	// Legacy agent filter: from OR to OR broadcast. Kept for compat; new code should use From/To/Concerns.
	if filters.Agent != "" {
		b.WriteString(` AND (from_agent = ? OR to_agent = ? OR to_agent = 'all' OR to_agent = '')`)
		args = append(args, filters.Agent, filters.Agent)
	}
	if filters.From != "" {
		b.WriteString(` AND from_agent = ?`)
		args = append(args, filters.From)
	}
	if filters.To != "" {
		b.WriteString(` AND to_agent = ?`)
		args = append(args, filters.To)
	}
	if filters.BroadcastsOnly {
		b.WriteString(` AND (to_agent = 'all' OR to_agent = '' OR to_agent IS NULL)`)
	}
	if filters.Concerns != "" {
		// to == concerns OR broadcast OR mentions contains concerns.
		// mentions_json is a JSON string array; match on exact "call-sign" token.
		b.WriteString(` AND (to_agent = ? OR to_agent = 'all' OR to_agent = '' OR to_agent IS NULL OR instr(mentions_json, ?) > 0)`)
		args = append(args, filters.Concerns, `"`+filters.Concerns+`"`)
	}
	if filters.Topic != "" {
		b.WriteString(` AND topic = ?`)
		args = append(args, filters.Topic)
	}
	if filters.Status != "" {
		b.WriteString(` AND status = ?`)
		args = append(args, filters.Status)
	}
	if filters.ThreadRootID > 0 {
		b.WriteString(` AND (id = ? OR thread_root_id = ?)`)
		args = append(args, filters.ThreadRootID, filters.ThreadRootID)
	}
	if filters.AfterID > 0 {
		b.WriteString(` AND id > ?`)
		args = append(args, filters.AfterID)
	}
	if filters.Before != "" {
		b.WriteString(` AND created_at < ?`)
		args = append(args, filters.Before)
	}
	if filters.After != "" {
		b.WriteString(` AND created_at > ?`)
		args = append(args, filters.After)
	}
	if filters.HasRefs != nil {
		if *filters.HasRefs {
			b.WriteString(` AND refs_json IS NOT NULL AND refs_json != '[]' AND refs_json != ''`)
		} else {
			b.WriteString(` AND (refs_json IS NULL OR refs_json = '[]' OR refs_json = '')`)
		}
	}
	if len(filters.MentionsAny) > 0 {
		b.WriteString(` AND (`)
		for i, m := range filters.MentionsAny {
			if i > 0 {
				b.WriteString(` OR `)
			}
			b.WriteString(`instr(mentions_json, ?) > 0`)
			args = append(args, `"`+m+`"`)
		}
		b.WriteString(`)`)
	}
	b.WriteString(` ORDER BY created_at DESC, id DESC LIMIT ?`)
	args = append(args, filters.Limit)

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []message
	for rows.Next() {
		var msg message
		var refsJSON string
		var mentionsJSON string
		var threadRootID sql.NullInt64
		var replyToID sql.NullInt64
		var ackedAt sql.NullString
		var ackedBy sql.NullString
		if err := rows.Scan(&msg.ID, &msg.CreatedAt, &msg.Room, &msg.From, &msg.To, &msg.Topic, &msg.Status, &threadRootID, &replyToID, &refsJSON, &mentionsJSON, &msg.Body, &ackedAt, &ackedBy); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(refsJSON), &msg.Refs); err != nil {
			return nil, err
		}
		if mentionsJSON == "" {
			msg.Mentions = []string{}
		} else if err := json.Unmarshal([]byte(mentionsJSON), &msg.Mentions); err != nil {
			return nil, err
		}
		if msg.Mentions == nil {
			msg.Mentions = []string{}
		}
		if threadRootID.Valid {
			msg.ThreadRootID = &threadRootID.Int64
		}
		if replyToID.Valid {
			msg.ReplyToID = &replyToID.Int64
		}
		if ackedAt.Valid {
			msg.AckedAt = &ackedAt.String
		}
		if ackedBy.Valid {
			msg.AckedBy = &ackedBy.String
		}
		out = append(out, msg)
	}

	return out, rows.Err()
}

func (s *server) renderRecentMessages(ctx context.Context, limit int) (string, error) {
	msgs, err := s.listMessages(ctx, listFilters{
		Limit:           limit,
		IncludeAcked:    true,
		IncludeArchived: false,
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("# Recent COMMSYNC Messages\n\n")
	b.WriteString(renderMessages(msgs))
	return b.String(), nil
}

func renderMessages(msgs []message) string {
	if len(msgs) == 0 {
		return "No matching messages."
	}

	var b strings.Builder
	for i, msg := range msgs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("[" + strconv.FormatInt(msg.ID, 10) + "] ")
		b.WriteString(msg.CreatedAt)
		b.WriteString(" | room:")
		b.WriteString(msg.Room)
		b.WriteString(" | from:")
		b.WriteString(msg.From)
		b.WriteString(" | to:")
		b.WriteString(msg.To)
		b.WriteString(" | topic:")
		b.WriteString(msg.Topic)
		b.WriteString(" | status:")
		b.WriteString(msg.Status)
		if msg.ThreadRootID != nil {
			b.WriteString(" | thread:")
			b.WriteString(strconv.FormatInt(*msg.ThreadRootID, 10))
		}
		if msg.ReplyToID != nil {
			b.WriteString(" | reply_to:")
			b.WriteString(strconv.FormatInt(*msg.ReplyToID, 10))
		}
		if msg.AckedBy != nil {
			b.WriteString(" | acked_by:")
			b.WriteString(*msg.AckedBy)
		}
		b.WriteString("\n")
		if len(msg.Mentions) > 0 {
			b.WriteString("mentions: ")
			b.WriteString(strings.Join(msg.Mentions, ", "))
			b.WriteString("\n")
		}
		if len(msg.Refs) > 0 {
			b.WriteString("refs: ")
			b.WriteString(strings.Join(msg.Refs, ", "))
			b.WriteString("\n")
		}
		b.WriteString(msg.Body)
	}
	return b.String()
}

func validateMessageArgs(from, to, topic, status, body string) error {
	if from == "" {
		return errors.New("from is required")
	}
	if to == "" {
		return errors.New("to is required")
	}
	if topic == "" {
		return errors.New("topic is required")
	}
	switch status {
	case "info", "ask", "warn", "ack", "decision":
	default:
		return fmt.Errorf("invalid status %q", status)
	}
	if body == "" {
		return errors.New("body is required")
	}
	return nil
}

func normalizeMentions(mentions []string) []string {
	if len(mentions) == 0 {
		return []string{}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(mentions))
	for _, m := range mentions {
		m = strings.TrimSpace(m)
		m = strings.TrimPrefix(m, "@")
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

func normalizeRefs(refs []string) []string {
	if len(refs) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref != "" {
			out = append(out, ref)
		}
	}
	return out
}

func normalizeRoomName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}
	replacer := strings.NewReplacer(" ", "-", "/", "-", "\\", "-", "\t", "-", "\n", "-")
	name = replacer.Replace(name)
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	return strings.Trim(name, "-")
}

func toolResult(text string, structured map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"content": []map[string]interface{}{{
			"type": "text",
			"text": text,
		}},
		"structuredContent": structured,
	}
}
