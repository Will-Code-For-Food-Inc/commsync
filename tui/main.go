// commsync-tui: read-only operator TUI for the commsync corridor.
//
// Separate binary, separate module. Reads the commsync SQLite DB
// directly in read-only mode. Never writes. Never networks.
//
// See commsync/README.md for the server side. Doctrine: human UX
// stays out of the core commsync binary.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	_ "github.com/mattn/go-sqlite3"
)

// ---------- data ----------

type message struct {
	ID        int64
	CreatedAt time.Time
	Room      string
	From      string
	To        string
	Topic     string
	Status    string
	Body      string
	Acked     bool
	ReplyTo   sql.NullInt64
	PinID     sql.NullInt64
	PinKind   sql.NullString
}

type pinEntry struct {
	PinID          int64
	Kind           string
	PinnedAt       time.Time
	PinnedBy       string
	TargetInstance string
	Note           string
	MsgID          int64
	MsgFrom        string
	MsgTo          string
	MsgRoom        string
	MsgTopic       string
	MsgStatus      string
	MsgBody        string
	AckedByMe      bool
}

type store struct {
	db *sql.DB
}

func openStore(path string) (*store, error) {
	// read-only, immutable=false so WAL writers aren't blocked
	dsn := fmt.Sprintf("file:%s?mode=ro&_busy_timeout=2000&_query_only=1", url.PathEscape(path))
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, err
	}
	return &store{db: db}, nil
}

func (s *store) close() { _ = s.db.Close() }

func (s *store) listRooms(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM rooms ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

type filterSpec struct {
	room         string // "" = all rooms
	topic        string // "" = all topics
	agent        string // "" = all agents (matches from_agent or to_agent)
	includeAcked bool
	limit        int
}

func (s *store) listMessages(ctx context.Context, f filterSpec) ([]message, error) {
	var (
		clauses []string
		args    []interface{}
	)
	clauses = append(clauses, `archived_at IS NULL`)
	if f.room != "" {
		clauses = append(clauses, `room_name = ?`)
		args = append(args, f.room)
	}
	if f.topic != "" {
		clauses = append(clauses, `topic = ?`)
		args = append(args, f.topic)
	}
	if f.agent != "" {
		clauses = append(clauses, `(from_agent = ? OR to_agent = ?)`)
		args = append(args, f.agent, f.agent)
	}
	if !f.includeAcked {
		clauses = append(clauses, `acked_at IS NULL`)
	}
	limit := f.limit
	if limit <= 0 {
		limit = 500
	}
	whereClause := strings.Join(clauses, " AND ")
	qWithPins := fmt.Sprintf(`
SELECT id, created_at, room_name, from_agent, to_agent, topic, status, body,
       acked_at IS NOT NULL AS acked, reply_to_id,
       (SELECT id FROM pinned_messages WHERE message_id = messages.id AND unpinned_at IS NULL ORDER BY id ASC LIMIT 1) AS pin_id,
       (SELECT kind FROM pinned_messages WHERE message_id = messages.id AND unpinned_at IS NULL ORDER BY id ASC LIMIT 1) AS pin_kind
FROM messages
WHERE %s
ORDER BY created_at DESC, id DESC
LIMIT %d`, whereClause, limit)

	rows, err := s.db.QueryContext(ctx, qWithPins, args...)
	if err != nil {
		// Fallback: pin tables may not exist yet (server not yet run)
		qFallback := fmt.Sprintf(`
SELECT id, created_at, room_name, from_agent, to_agent, topic, status, body,
       acked_at IS NOT NULL AS acked, reply_to_id
FROM messages
WHERE %s
ORDER BY created_at DESC, id DESC
LIMIT %d`, whereClause, limit)
		rows, err = s.db.QueryContext(ctx, qFallback, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []message
		for rows.Next() {
			var m message
			var createdAt string
			if err := rows.Scan(&m.ID, &createdAt, &m.Room, &m.From, &m.To, &m.Topic,
				&m.Status, &m.Body, &m.Acked, &m.ReplyTo); err != nil {
				return nil, err
			}
			m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
			out = append(out, m)
		}
		return out, rows.Err()
	}
	defer rows.Close()
	var out []message
	for rows.Next() {
		var m message
		var createdAt string
		if err := rows.Scan(&m.ID, &createdAt, &m.Room, &m.From, &m.To, &m.Topic,
			&m.Status, &m.Body, &m.Acked, &m.ReplyTo, &m.PinID, &m.PinKind); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *store) distinctTopics(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT topic FROM messages WHERE archived_at IS NULL ORDER BY topic`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		if t != "" {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func (s *store) distinctAgents(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT agent FROM (
  SELECT from_agent AS agent FROM messages WHERE archived_at IS NULL
  UNION
  SELECT to_agent   AS agent FROM messages WHERE archived_at IS NULL
) WHERE agent IS NOT NULL AND agent <> '' ORDER BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *store) searchMessages(ctx context.Context, query string) ([]message, error) {
	q := `
SELECT m.id, m.created_at, m.room_name, m.from_agent, m.to_agent, m.topic, m.status, m.body,
       m.acked_at IS NOT NULL AS acked, m.reply_to_id,
       (SELECT id FROM pinned_messages WHERE message_id = m.id AND unpinned_at IS NULL ORDER BY id ASC LIMIT 1) AS pin_id,
       (SELECT kind FROM pinned_messages WHERE message_id = m.id AND unpinned_at IS NULL ORDER BY id ASC LIMIT 1) AS pin_kind
FROM messages_fts f
JOIN messages m ON m.id = f.rowid
WHERE messages_fts MATCH ? AND m.archived_at IS NULL
ORDER BY bm25(messages_fts), m.created_at DESC, m.id DESC
LIMIT 200`
	rows, err := s.db.QueryContext(ctx, q, query)
	if err != nil {
		// FTS5 not available or query error — fallback to LIKE
		q2 := `
SELECT id, created_at, room_name, from_agent, to_agent, topic, status, body,
       acked_at IS NOT NULL AS acked, reply_to_id
FROM messages
WHERE archived_at IS NULL AND body LIKE ?
ORDER BY created_at DESC, id DESC
LIMIT 200`
		rows, err = s.db.QueryContext(ctx, q2, "%"+query+"%")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []message
		for rows.Next() {
			var m message
			var createdAt string
			if err := rows.Scan(&m.ID, &createdAt, &m.Room, &m.From, &m.To, &m.Topic,
				&m.Status, &m.Body, &m.Acked, &m.ReplyTo); err != nil {
				return nil, err
			}
			m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
			out = append(out, m)
		}
		return out, rows.Err()
	}
	defer rows.Close()
	var out []message
	for rows.Next() {
		var m message
		var createdAt string
		if err := rows.Scan(&m.ID, &createdAt, &m.Room, &m.From, &m.To, &m.Topic,
			&m.Status, &m.Body, &m.Acked, &m.ReplyTo, &m.PinID, &m.PinKind); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *store) listPins(ctx context.Context, identity string) ([]pinEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT pm.id, pm.kind, pm.pinned_at, pm.pinned_by,
       COALESCE(pm.target_instance, '') AS target_instance,
       pm.note,
       m.id, m.from_agent, m.to_agent, m.room_name, m.topic, m.status,
       substr(m.body, 1, 200) AS body_preview,
       EXISTS(SELECT 1 FROM pin_acks WHERE pin_id = pm.id AND instance_id = ?) AS acked_by_me
FROM pinned_messages pm
JOIN messages m ON m.id = pm.message_id
WHERE pm.unpinned_at IS NULL
  AND (pm.target_instance IS NULL OR pm.target_instance = ?)
ORDER BY pm.pinned_at DESC`, identity, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pinEntry
	for rows.Next() {
		var p pinEntry
		var pinnedAt string
		var ackedByMe int
		if err := rows.Scan(&p.PinID, &p.Kind, &pinnedAt, &p.PinnedBy,
			&p.TargetInstance, &p.Note,
			&p.MsgID, &p.MsgFrom, &p.MsgTo, &p.MsgRoom, &p.MsgTopic, &p.MsgStatus,
			&p.MsgBody, &ackedByMe); err != nil {
			return nil, err
		}
		p.PinnedAt, _ = time.Parse(time.RFC3339, pinnedAt)
		p.AckedByMe = ackedByMe == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---------- row types for date-grouped list ----------

type rowKind int

const (
	rowDateHeader rowKind = iota
	rowMessage
)

type listRow struct {
	kind   rowKind
	date   string // YYYY-MM-DD key; set for both kinds
	msgIdx int    // index into m.messages; valid only when kind==rowMessage
}

// ---------- bubbletea model ----------

type tickMsg time.Time
type pinResultMsg struct{ err error }
type searchMsg struct {
	results []message
	err     error
}

type dataMsg struct {
	msgs   []message
	pins   []pinEntry
	rooms  []string
	topics []string
	agents []string
	err    error
}

type model struct {
	st             *store
	width          int
	height         int
	filter         filterSpec
	messages       []message
	pins           []pinEntry
	rooms          []string
	topics         []string
	agents         []string
	cursor         int
	rows           []listRow
	collapsedDates map[string]bool
	showPreview    bool
	previewScroll  int
	showHelp       bool
	showAbout      bool
	showPick       bool
	pickKind       string // "room" | "topic" | "agent" | "pinkind"
	pickIdx        int
	showPins       bool
	pinCursor      int
	showSearch     bool
	searchInput    string
	searchActive   bool
	searchResults  []message
	searchCursor   int
	identity       string
	binPath        string
	dbPath         string
	pinTargetMsgID int64
	pinTargetPinID int64
	errMsg         string
	pollEvery      time.Duration
}

func initialModel(st *store, pollEvery time.Duration, identity, binPath, dbPath string) model {
	return model{
		st:             st,
		filter:         filterSpec{includeAcked: false, limit: 500},
		pollEvery:      pollEvery,
		identity:       identity,
		binPath:        binPath,
		dbPath:         dbPath,
		collapsedDates: make(map[string]bool),
	}
}

func (m *model) rebuildRows() {
	m.rows = m.rows[:0]
	today := time.Now().Local().Format("2006-01-02")
	lastKey := ""
	for i, msg := range m.messages {
		key := msg.CreatedAt.Local().Format("2006-01-02")
		if key != lastKey {
			if _, seen := m.collapsedDates[key]; !seen {
				m.collapsedDates[key] = key != today
			}
			m.rows = append(m.rows, listRow{kind: rowDateHeader, date: key})
			lastKey = key
		}
		if !m.collapsedDates[key] {
			m.rows = append(m.rows, listRow{kind: rowMessage, date: key, msgIdx: i})
		}
	}
}

func (m *model) clampCursor() {
	if len(m.rows) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *model) currentMessage() (message, bool) {
	if m.searchActive {
		if m.searchCursor < 0 || m.searchCursor >= len(m.searchResults) {
			return message{}, false
		}
		return m.searchResults[m.searchCursor], true
	}
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return message{}, false
	}
	r := m.rows[m.cursor]
	if r.kind != rowMessage {
		return message{}, false
	}
	return m.messages[r.msgIdx], true
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.reload(), tick(m.pollEvery))
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m model) reload() tea.Cmd {
	st := m.st
	f := m.filter
	identity := m.identity
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		rooms, err := st.listRooms(ctx)
		if err != nil {
			return dataMsg{err: err}
		}
		topics, _ := st.distinctTopics(ctx)
		agents, _ := st.distinctAgents(ctx)
		msgs, err := st.listMessages(ctx, f)
		if err != nil {
			return dataMsg{err: err}
		}
		pins, _ := st.listPins(ctx, identity)
		return dataMsg{msgs: msgs, pins: pins, rooms: rooms, topics: topics, agents: agents}
	}
}

func (m model) Update(raw tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := raw.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case tickMsg:
		return m, tea.Batch(m.reload(), tick(m.pollEvery))
	case dataMsg:
		if msg.err != nil {
			m.errMsg = msg.err.Error()
			return m, nil
		}
		m.errMsg = ""
		// If preview is open, track the pinned message by ID so new arrivals
		// don't silently swap what's being displayed.
		var pinnedID int64
		if m.showPreview {
			if cur, ok := m.currentMessage(); ok {
				pinnedID = cur.ID
			}
		}
		m.messages = msg.msgs
		m.pins = msg.pins
		m.rooms = msg.rooms
		m.topics = msg.topics
		m.agents = msg.agents
		m.rebuildRows()
		if pinnedID != 0 {
			for i, row := range m.rows {
				if row.kind == rowMessage && m.messages[row.msgIdx].ID == pinnedID {
					m.cursor = i
					break
				}
			}
		}
		m.clampCursor()
		return m, nil
	case pinResultMsg:
		if msg.err != nil {
			m.errMsg = msg.err.Error()
		}
		return m, m.reload()
	case searchMsg:
		if msg.err != nil {
			m.errMsg = msg.err.Error()
		} else {
			m.searchResults = msg.results
			m.searchCursor = 0
		}
		return m, nil
	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m model) execSearch() tea.Cmd {
	st := m.st
	q := m.searchInput
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		results, err := st.searchMessages(ctx, q)
		return searchMsg{results: results, err: err}
	}
}

func (m model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.showSearch {
		return m.onSearchKey(k)
	}
	if m.searchActive && !m.showPreview {
		return m.onSearchListKey(k)
	}
	if m.showPins {
		return m.onPinPanelKey(k)
	}
	if m.showPreview {
		return m.onPreviewKey(k)
	}
	if m.showPick {
		return m.onPickKey(k)
	}
	if m.showHelp {
		switch k.String() {
		case "?", "esc", "q":
			m.showHelp = false
		}
		return m, nil
	}
	if m.showAbout {
		switch k.String() {
		case "v", "esc", "q":
			m.showAbout = false
		}
		return m, nil
	}
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "/":
		m.showSearch = true
		m.searchInput = ""
		return m, nil
	case "esc":
		if m.searchActive {
			m.searchActive = false
			m.searchResults = nil
			m.searchInput = ""
			return m, nil
		}
	case "?":
		m.showHelp = true
	case "v":
		m.showAbout = true
	case "j", "down":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g":
		m.cursor = 0
	case "G":
		m.cursor = max(0, len(m.rows)-1)
	case "pgdown", "ctrl+f":
		m.cursor = min(len(m.rows)-1, m.cursor+10)
	case "pgup", "ctrl+b":
		m.cursor = max(0, m.cursor-10)
	case "tab":
		if m.cursor >= 0 && m.cursor < len(m.rows) {
			key := m.rows[m.cursor].date
			wasCollapsed := m.collapsedDates[key]
			m.collapsedDates[key] = !wasCollapsed
			m.rebuildRows()
			// move cursor to the date header for this key
			for i, r := range m.rows {
				if r.date == key && r.kind == rowDateHeader {
					m.cursor = i
					break
				}
			}
		}
	case "enter", " ":
		if _, ok := m.currentMessage(); ok {
			m.showPreview = true
			m.previewScroll = 0
		} else if m.cursor < len(m.rows) {
			// on a header — toggle collapse
			key := m.rows[m.cursor].date
			m.collapsedDates[key] = !m.collapsedDates[key]
			m.rebuildRows()
			m.clampCursor()
		}
	case "r":
		m.showPick = true
		m.pickKind = "room"
		m.pickIdx = indexOf(m.rooms, m.filter.room)
	case "t":
		m.showPick = true
		m.pickKind = "topic"
		m.pickIdx = indexOf(m.topics, m.filter.topic)
	case "a":
		m.showPick = true
		m.pickKind = "agent"
		m.pickIdx = indexOf(m.agents, m.filter.agent)
	case "x":
		m.filter.room = ""
		m.filter.topic = ""
		m.filter.agent = ""
		return m, m.reload()
	case "A":
		m.filter.includeAcked = !m.filter.includeAcked
		return m, m.reload()
	case "R":
		return m, m.reload()
	case "p":
		if cur, ok := m.currentMessage(); ok {
			m.pinTargetMsgID = cur.ID
			m.showPick = true
			m.pickKind = "pinkind"
			m.pickIdx = 0
		}
	case "u":
		if cur, ok := m.currentMessage(); ok {
			if cur.PinID.Valid {
				return m, callCommsync(m.binPath, "unpin_message", map[string]interface{}{
					"pin_id":      cur.PinID.Int64,
					"unpinned_by": m.identity,
				})
			}
		}
	case "P":
		m.showPins = !m.showPins
		m.pinCursor = 0
	}
	return m, nil
}

func (m model) onPinPanelKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "P", "esc":
		m.showPins = false
	case "j", "down":
		if m.pinCursor < len(m.pins)-1 {
			m.pinCursor++
		}
	case "k", "up":
		if m.pinCursor > 0 {
			m.pinCursor--
		}
	case "d":
		if m.pinCursor < len(m.pins) {
			p := m.pins[m.pinCursor]
			if p.Kind == "broadcast" && !p.AckedByMe {
				return m, callCommsync(m.binPath, "ack_pin", map[string]interface{}{
					"pin_id":      p.PinID,
					"instance_id": m.identity,
					"acked_by":    m.identity,
				})
			}
		}
	case "u":
		if m.pinCursor < len(m.pins) {
			p := m.pins[m.pinCursor]
			return m, callCommsync(m.binPath, "unpin_message", map[string]interface{}{
				"pin_id":      p.PinID,
				"unpinned_by": m.identity,
			})
		}
	case "enter":
		if m.pinCursor < len(m.pins) {
			p := m.pins[m.pinCursor]
			m.showPins = false
			// Find the row index for this message and set cursor to it
			for i, row := range m.rows {
				if row.kind == rowMessage && m.messages[row.msgIdx].ID == p.MsgID {
					m.cursor = i
					break
				}
			}
			m.showPreview = true
			m.previewScroll = 0
		}
	}
	return m, nil
}

func (m model) onPreviewKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "q", "enter", " ":
		m.showPreview = false
		m.previewScroll = 0
	case "j", "down":
		m.previewScroll++
	case "k", "up":
		if m.previewScroll > 0 {
			m.previewScroll--
		}
	case "g":
		m.previewScroll = 0
	case "G":
		m.previewScroll = 1<<31 - 1 // clamped in renderPreview
	case "pgdown", "ctrl+f":
		m.previewScroll += 10
	case "pgup", "ctrl+b":
		if m.previewScroll > 10 {
			m.previewScroll -= 10
		} else {
			m.previewScroll = 0
		}
	}
	return m, nil
}

func (m model) onPickKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	opts := m.pickOptions()
	switch k.String() {
	case "esc", "q":
		m.showPick = false
	case "j", "down":
		if m.pickIdx < len(opts)-1 {
			m.pickIdx++
		}
	case "k", "up":
		if m.pickIdx > 0 {
			m.pickIdx--
		}
	case "enter":
		if m.pickKind == "pinkind" {
			kind := "broadcast"
			if m.pickIdx == 1 {
				kind = "snippet"
			}
			m.showPick = false
			msgID := m.pinTargetMsgID
			return m, callCommsync(m.binPath, "pin_message", map[string]interface{}{
				"message_id": msgID,
				"kind":       kind,
				"pinned_by":  m.identity,
			})
		}
		val := opts[m.pickIdx]
		// opts[0] is always "(all)"
		if m.pickIdx == 0 {
			val = ""
		}
		switch m.pickKind {
		case "room":
			m.filter.room = val
		case "topic":
			m.filter.topic = val
		case "agent":
			m.filter.agent = val
		}
		m.showPick = false
		return m, m.reload()
	}
	return m, nil
}

func (m model) onSearchKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc":
		m.showSearch = false
		m.searchInput = ""
		m.searchActive = false
		m.searchResults = nil
	case "enter":
		if m.searchInput != "" {
			m.showSearch = false
			m.searchActive = true
			m.searchCursor = 0
			return m, m.execSearch()
		}
		m.showSearch = false
	case "backspace", "ctrl+h":
		if len(m.searchInput) > 0 {
			runes := []rune(m.searchInput)
			m.searchInput = string(runes[:len(runes)-1])
		}
	default:
		if len(k.String()) == 1 || k.String() == " " {
			m.searchInput += k.String()
		}
	}
	return m, nil
}

func (m model) onSearchListKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.String() {
	case "esc", "x":
		m.searchActive = false
		m.searchResults = nil
		m.searchInput = ""
	case "j", "down":
		if m.searchCursor < len(m.searchResults)-1 {
			m.searchCursor++
		}
	case "k", "up":
		if m.searchCursor > 0 {
			m.searchCursor--
		}
	case "enter", " ":
		m.showPreview = true
		m.previewScroll = 0
	}
	return m, nil
}

func (m model) pickOptions() []string {
	switch m.pickKind {
	case "pinkind":
		return []string{
			"broadcast  (at-least-once: every instance must ack)",
			"snippet    (always-visible ambient context)",
		}
	}
	var base []string
	switch m.pickKind {
	case "room":
		base = m.rooms
	case "topic":
		base = m.topics
	case "agent":
		base = m.agents
	}
	out := []string{"(all)"}
	out = append(out, base...)
	return out
}

// ---------- rendering ----------

var (
	styleBar    = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("252")).Padding(0, 1)
	styleHelp   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleCursor = lipgloss.NewStyle().Background(lipgloss.Color("238")).Foreground(lipgloss.Color("230"))
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleMuted  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231"))

	styleInfo     = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	styleAsk      = lipgloss.NewStyle().Foreground(lipgloss.Color("220")) // yellow
	styleWarn     = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
	styleAck      = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // dim
	styleDecision = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))  // blue
)

func statusStyle(s string) lipgloss.Style {
	switch s {
	case "ask":
		return styleAsk
	case "warn":
		return styleWarn
	case "ack":
		return styleAck
	case "decision":
		return styleDecision
	default:
		return styleInfo
	}
}

func dateLabel(key string) string {
	today := time.Now().Local().Format("2006-01-02")
	yesterday := time.Now().Local().AddDate(0, 0, -1).Format("2006-01-02")
	switch key {
	case today:
		return "Today"
	case yesterday:
		return "Yesterday"
	default:
		return key
	}
}

func (m model) View() string {
	if m.width == 0 {
		return "loading..."
	}
	header := m.renderHeader()
	footer := m.renderFooter()
	bodyH := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if bodyH < 3 {
		bodyH = 3
	}
	body := m.renderList(bodyH)
	screen := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)

	if m.showPins {
		return overlay(screen, m.renderPinPanel(), m.width, m.height)
	}
	if m.showPreview {
		return overlay(screen, m.renderPreview(), m.width, m.height)
	}
	if m.showHelp {
		return overlay(screen, m.renderHelp(), m.width, m.height)
	}
	if m.showPick {
		return overlay(screen, m.renderPicker(), m.width, m.height)
	}
	if m.showAbout {
		return overlay(screen, m.renderAbout(), m.width, m.height)
	}
	return screen
}

func (m model) renderHeader() string {
	f := []string{}
	if m.filter.room != "" {
		f = append(f, "room="+m.filter.room)
	} else {
		f = append(f, "room=*")
	}
	if m.filter.topic != "" {
		f = append(f, "topic="+m.filter.topic)
	} else {
		f = append(f, "topic=*")
	}
	if m.filter.agent != "" {
		f = append(f, "agent="+m.filter.agent)
	} else {
		f = append(f, "agent=*")
	}
	if m.filter.includeAcked {
		f = append(f, "acked=on")
	} else {
		f = append(f, "acked=off")
	}
	title := styleHeader.Render("commsync-tui")
	filt := styleMuted.Render(strings.Join(f, "  "))
	line := fmt.Sprintf("%s  %s  (%d msgs)", title, filt, len(m.messages))
	activePins := 0
	for _, p := range m.pins {
		if p.Kind == "snippet" || !p.AckedByMe {
			activePins++
		}
	}
	if activePins > 0 {
		line += "  " + styleWarn.Render(fmt.Sprintf("[%d pin(s) · P]", activePins))
	}
	if m.errMsg != "" {
		line += "  " + styleErr.Render("ERR: "+m.errMsg)
	}
	return styleBar.Width(m.width).Render(line)
}

func (m model) renderFooter() string {
	if m.showSearch {
		bar := styleHeader.Render("/") + " " + m.searchInput + "█"
		return styleBar.Width(m.width).Render(bar)
	}
	if m.searchActive {
		indicator := styleWarn.Render(fmt.Sprintf("search: %q (%d results) — esc/x to clear", m.searchInput, len(m.searchResults)))
		return styleBar.Width(m.width).Render(indicator)
	}
	hint := "q quit | / search | ? help | v about | j/k move | tab/enter collapse | enter preview | r room | t topic | a agent | A acked | x clear | R reload | p pin | u unpin | P pins"
	return styleBar.Width(m.width).Render(styleHelp.Render(hint))
}

func (m model) renderSearchResults(h int) string {
	if len(m.searchResults) == 0 {
		return styleDim.Render("no results for " + m.searchInput)
	}
	const badgeW = 2
	tsW := 5
	roomW := 10
	fromW := 12
	toW := 10
	topicW := 14
	statusW := 9
	remainder := m.width - badgeW - tsW - roomW - fromW - toW - topicW - statusW - 6
	if remainder < 20 {
		remainder = 20
	}
	bodyW := remainder

	lines := []string{}
	colHeader := fmt.Sprintf("  %-*s  %-*s %-*s %-*s %-*s %-*s %s",
		tsW, "TIME", roomW, "ROOM", fromW, "FROM", toW, "→ TO", topicW, "TOPIC", statusW, "STATUS", "BODY")
	lines = append(lines, styleDim.Render(colHeader))

	rowsAvail := h - 1
	if rowsAvail < 3 {
		rowsAvail = 3
	}
	start := 0
	if m.searchCursor >= rowsAvail {
		start = m.searchCursor - rowsAvail + 1
	}
	end := start + rowsAvail
	if end > len(m.searchResults) {
		end = len(m.searchResults)
	}

	for i := start; i < end; i++ {
		msg := m.searchResults[i]
		ts := msg.CreatedAt.Local().Format("15:04")
		room := truncate(msg.Room, roomW)
		from := truncate(msg.From, fromW)
		to := truncate(msg.To, toW)
		topic := truncate(msg.Topic, topicW)
		status := truncate(msg.Status, statusW)
		preview := truncate(oneLine(msg.Body), bodyW)
		badge := "  "
		if msg.PinID.Valid {
			if msg.PinKind.String == "snippet" {
				badge = "* "
			} else {
				badge = "! "
			}
		}
		statusStr := statusStyle(msg.Status).Render(fmt.Sprintf("%-*s", statusW, status))
		msgRow := fmt.Sprintf("%s%-*s  %-*s %-*s %-*s %-*s %s %s",
			badge, tsW, ts, roomW, room, fromW, from, toW, to, topicW, topic, statusStr, preview)
		if msg.Acked {
			msgRow = styleAck.Render(msgRow)
		}
		if i == m.searchCursor {
			msgRow = styleCursor.Render(msgRow)
		}
		lines = append(lines, msgRow)
	}
	return strings.Join(lines, "\n")
}

func (m model) renderList(h int) string {
	if m.searchActive {
		return m.renderSearchResults(h)
	}
	if len(m.messages) == 0 {
		return styleDim.Render("no messages match current filter")
	}
	// messages slice is already newest-first. Render newest at top.
	// Column widths (fit to width).
	const badgeW = 2 // pin badge: "* " or "! " or "  "
	tsW := 5         // "15:04"
	roomW := 10
	fromW := 12
	toW := 10
	topicW := 14
	statusW := 9
	remainder := m.width - badgeW - tsW - roomW - fromW - toW - topicW - statusW - 6
	if remainder < 20 {
		remainder = 20
	}
	bodyW := remainder

	lines := []string{}
	colHeader := fmt.Sprintf("  %-*s  %-*s %-*s %-*s %-*s %-*s %s",
		tsW, "TIME", roomW, "ROOM", fromW, "FROM", toW, "→ TO", topicW, "TOPIC", statusW, "STATUS", "BODY")
	lines = append(lines, styleDim.Render(colHeader))

	// determine visible window around cursor
	rowsAvail := h - 1 // minus header
	if rowsAvail < 3 {
		rowsAvail = 3
	}
	start := 0
	if m.cursor >= rowsAvail {
		start = m.cursor - rowsAvail + 1
	}
	end := start + rowsAvail
	if end > len(m.rows) {
		end = len(m.rows)
	}

	for i := start; i < end; i++ {
		row := m.rows[i]
		if row.kind == rowDateHeader {
			arrow := "▾"
			if m.collapsedDates[row.date] {
				arrow = "▸"
			}
			label := fmt.Sprintf("%s %s", arrow, dateLabel(row.date))
			dashW := m.width - len([]rune(label)) - 8
			if dashW < 0 {
				dashW = 0
			}
			line := styleDim.Render(strings.Repeat("─", 4)) + " " + styleHeader.Render(label) + " " + styleDim.Render(strings.Repeat("─", dashW))
			if i == m.cursor {
				line = styleCursor.Render(line)
			}
			lines = append(lines, line)
		} else {
			msg := m.messages[row.msgIdx]
			ts := msg.CreatedAt.Local().Format("15:04")
			room := truncate(msg.Room, roomW)
			from := truncate(msg.From, fromW)
			to := truncate(msg.To, toW)
			topic := truncate(msg.Topic, topicW)
			status := truncate(msg.Status, statusW)
			preview := truncate(oneLine(msg.Body), bodyW)

			badge := "  "
			if msg.PinID.Valid {
				if msg.PinKind.String == "snippet" {
					badge = "* "
				} else {
					badge = "! "
				}
			}

			statusStr := statusStyle(msg.Status).Render(fmt.Sprintf("%-*s", statusW, status))
			msgRow := fmt.Sprintf("%s%-*s  %-*s %-*s %-*s %-*s %s %s",
				badge, tsW, ts, roomW, room, fromW, from, toW, to, topicW, topic, statusStr, preview)

			if msg.Acked {
				msgRow = styleAck.Render(msgRow)
			}
			if i == m.cursor {
				msgRow = styleCursor.Render(msgRow)
			}
			lines = append(lines, msgRow)
		}
	}
	return strings.Join(lines, "\n")
}

func (m model) renderPreview() string {
	msg, ok := m.currentMessage()
	if !ok {
		return ""
	}

	boxW := m.width - 6
	if boxW < 40 {
		boxW = 40
	}
	contentW := boxW - 4 // border (1 each) + padding (1 each)

	// Build all content lines
	var lines []string
	lines = append(lines, styleHeader.Render(fmt.Sprintf("[%d]  %s → %s", msg.ID, msg.From, msg.To)))
	lines = append(lines, styleDim.Render(fmt.Sprintf("room=%-12s topic=%-14s status=%s", msg.Room, msg.Topic, msg.Status)))
	lines = append(lines, styleDim.Render(fmt.Sprintf("created: %s", msg.CreatedAt.Local().Format(time.RFC3339))))
	lines = append(lines, styleDim.Render(strings.Repeat("─", contentW)))
	lines = append(lines, "")
	lines = append(lines, wrap(msg.Body, contentW)...)

	// Viewport height: terminal minus borders/chrome
	boxH := m.height - 4
	if boxH < 12 {
		boxH = 12
	}
	// reserve: border(2) + header(1) + scroll-hint-top(1) + scroll-hint-bot(1) + close-hint(1) + padding(2) = 8
	contentH := boxH - 8
	if contentH < 3 {
		contentH = 3
	}

	maxScroll := len(lines) - contentH
	if maxScroll < 0 {
		maxScroll = 0
	}
	scroll := m.previewScroll
	if scroll > maxScroll {
		scroll = maxScroll
	}

	end := scroll + contentH
	if end > len(lines) {
		end = len(lines)
	}
	visible := lines[scroll:end]
	for len(visible) < contentH {
		visible = append(visible, "")
	}

	var b strings.Builder
	if scroll > 0 {
		b.WriteString(styleDim.Render(fmt.Sprintf("↑ %d lines above", scroll)) + "\n")
	} else {
		b.WriteString(styleDim.Render(strings.Repeat("─", contentW)) + "\n")
	}
	for _, l := range visible {
		b.WriteString(l + "\n")
	}
	if scroll < maxScroll {
		b.WriteString(styleDim.Render(fmt.Sprintf("↓ %d lines below", len(lines)-end)) + "\n")
	} else {
		b.WriteString(styleDim.Render(strings.Repeat("─", contentW)) + "\n")
	}
	b.WriteString(styleHelp.Render("j/k scroll  g/G top/bottom  esc/q/enter close"))

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(boxW).
		Background(lipgloss.Color("235")).
		Foreground(lipgloss.Color("252")).
		Render(b.String())
}

func (m model) renderHelp() string {
	help := `commsync-tui — corridor viewer

  q / ctrl-c   quit
  ?            toggle this help
  /            search messages (FTS5; enter to run, esc to clear)
  j / down     move cursor down
  k / up       move cursor up
  g            jump to newest
  G            jump to oldest
  pgdn/pgup    page
  tab          toggle collapse for current date group
  enter/space  open preview (on message) or toggle collapse (on date header)
  r            pick room filter
  t            pick topic filter
  a            pick agent filter (matches from OR to)
  A            toggle include-acked messages
  x            clear all filters
  R            reload now (normally auto-polls)

  p            pin message under cursor (choose kind)
  u            unpin pinned message under cursor
  P            toggle pin panel overlay

  * = snippet pin (always visible)   ! = broadcast pin (ack required)

  Inside a picker:  j/k move, enter select, q/esc cancel
  Inside pin panel: j/k move, d ack broadcast, u unpin, enter preview, P/esc close

Ordering: newest-first, top of list.
identity: ` + m.identity + `
Polls every ~2s. Writes via commsync binary.`
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Foreground(lipgloss.Color("252")).
		Background(lipgloss.Color("235")).
		Render(help)
	return box
}

func (m model) renderAbout() string {
	exe, _ := os.Executable()

	revision := "(unknown)"
	buildTime := "(unknown)"
	goVersion := "(unknown)"
	if info, ok := debug.ReadBuildInfo(); ok {
		goVersion = info.GoVersion
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) > 12 {
					revision = s.Value[:12]
				} else {
					revision = s.Value
				}
			case "vcs.time":
				revision += " @ " + s.Value
			}
		}
		if buildTime == "(unknown)" {
			for _, s := range info.Settings {
				if s.Key == "vcs.time" {
					buildTime = s.Value
				}
			}
		}
	}

	content := fmt.Sprintf(`commsync-tui

  revision  %s
  go        %s
  binary    %s
  db        %s
  identity  %s
  poll      %s

  v / esc / q  close`,
		revision, goVersion, exe, m.dbPath, m.identity, m.pollEvery)

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Background(lipgloss.Color("235")).
		Foreground(lipgloss.Color("252")).
		Render(styleHeader.Render("about") + "\n\n" + content)
}

func (m model) renderPinPanel() string {
	var b strings.Builder
	title := fmt.Sprintf("PINS (%d)  identity:%s  — j/k · d ack · u unpin · enter preview · P/esc close",
		len(m.pins), m.identity)
	b.WriteString(styleHeader.Render(title) + "\n\n")
	if len(m.pins) == 0 {
		b.WriteString(styleDim.Render("no active pins") + "\n")
	} else {
		for i, p := range m.pins {
			badge := "!"
			if p.Kind == "snippet" {
				badge = "*"
			}
			target := "[all]"
			if p.TargetInstance != "" {
				target = fmt.Sprintf("[→ %s]", p.TargetInstance)
			}
			ackMark := ""
			if p.AckedByMe && p.Kind == "broadcast" {
				ackMark = " ✓"
			}
			headerLine := fmt.Sprintf("%s #%d  %-9s  room:%-10s  topic:%-12s  by:%s%s  %s",
				badge, p.PinID, p.Kind, p.MsgRoom, p.MsgTopic, p.PinnedBy, ackMark, target)
			bodyPreview := truncate(oneLine(p.MsgBody), m.width-12)
			var line string
			if i == m.pinCursor {
				line = styleCursor.Render(headerLine) + "\n    " + styleDim.Render(bodyPreview)
			} else {
				line = headerLine + "\n    " + styleDim.Render(bodyPreview)
			}
			b.WriteString(line + "\n")
		}
	}
	boxW := m.width - 4
	if boxW < 60 {
		boxW = 60
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Background(lipgloss.Color("235")).
		Width(boxW).
		Render(b.String())
}

func (m model) renderPicker() string {
	opts := m.pickOptions()

	boxW := m.width - 6
	if boxW < 40 {
		boxW = 40
	}
	boxH := m.height - 4
	if boxH < 10 {
		boxH = 10
	}
	// border(2) + padding(2) + title(1) + blank(1) + scroll-hint-top(1) + scroll-hint-bot(1) = 8
	contentH := boxH - 8
	if contentH < 3 {
		contentH = 3
	}

	// Derive scroll offset so the cursor stays visible.
	scroll := 0
	if m.pickIdx >= contentH {
		scroll = m.pickIdx - contentH + 1
	}
	end := scroll + contentH
	if end > len(opts) {
		end = len(opts)
	}

	var b strings.Builder
	title := fmt.Sprintf("pick %s  (j/k move, enter select, q cancel)", m.pickKind)
	b.WriteString(styleHeader.Render(title) + "\n\n")

	if scroll > 0 {
		b.WriteString(styleDim.Render(fmt.Sprintf("↑ %d above", scroll)) + "\n")
	} else {
		b.WriteString(styleDim.Render(strings.Repeat("─", boxW-6)) + "\n")
	}
	for i := scroll; i < end; i++ {
		line := opts[i]
		if i == m.pickIdx {
			line = styleCursor.Render("› " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	if end < len(opts) {
		b.WriteString(styleDim.Render(fmt.Sprintf("↓ %d below", len(opts)-end)) + "\n")
	} else {
		b.WriteString(styleDim.Render(strings.Repeat("─", boxW-6)) + "\n")
	}

	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(1, 2).
		Width(boxW).
		Background(lipgloss.Color("235")).
		Render(b.String())
}

// overlay puts the box over the screen, centered. bubbletea will just
// render the overlay string in its own frame; we approximate centering
// by padding vertically and horizontally.
func overlay(base, box string, w, h int) string {
	boxW := lipgloss.Width(box)
	boxH := lipgloss.Height(box)
	padX := max(0, (w-boxW)/2)
	padY := max(0, (h-boxH)/2)
	var b strings.Builder
	for i := 0; i < padY; i++ {
		b.WriteString("\n")
	}
	for _, line := range strings.Split(box, "\n") {
		b.WriteString(strings.Repeat(" ", padX))
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// ---------- helpers ----------

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w <= 1 {
		return "…"
	}
	// simple byte-truncate; good enough for ASCII-ish corridor traffic
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	s = strings.ReplaceAll(s, "\t", " ")
	return s
}

func wrap(s string, w int) []string {
	if w <= 0 {
		w = 80
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		if len(para) == 0 {
			out = append(out, "")
			continue
		}
		runes := []rune(para)
		for len(runes) > w {
			out = append(out, string(runes[:w]))
			runes = runes[w:]
		}
		out = append(out, string(runes))
	}
	return out
}

func indexOf(xs []string, s string) int {
	// index in picker options (offset by 1 for "(all)")
	if s == "" {
		return 0
	}
	sort.Strings(xs) // ensure deterministic if caller didn't
	for i, x := range xs {
		if x == s {
			return i + 1
		}
	}
	return 0
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- pin helpers ----------

func callCommsync(binPath, tool string, args interface{}) tea.Cmd {
	return func() tea.Msg {
		jsonArgs, err := json.Marshal(args)
		if err != nil {
			return pinResultMsg{err: err}
		}
		cmd := exec.Command(binPath, "call", tool, string(jsonArgs))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return pinResultMsg{err: fmt.Errorf("%s: %w\n%s", tool, err, strings.TrimSpace(string(out)))}
		}
		return pinResultMsg{}
	}
}

func defaultIdentity() string {
	if id := strings.TrimSpace(os.Getenv("COMMSYNC_TUI_ID")); id != "" {
		return id
	}
	stateDir := func() string {
		u, err := user.Current()
		if err != nil || u.HomeDir == "" {
			return "."
		}
		return filepath.Join(u.HomeDir, ".local", "state", "commsync")
	}()
	idFile := filepath.Join(stateDir, "tui-instance-id")
	if data, err := os.ReadFile(idFile); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	id := generateUUID()
	if err := os.MkdirAll(stateDir, 0o755); err == nil {
		_ = os.WriteFile(idFile, []byte(id), 0o600)
	}
	return id
}

func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func resolveCommSyncBin() string {
	if p := strings.TrimSpace(os.Getenv("COMMSYNC_BIN")); p != "" {
		return p
	}
	self, _ := os.Executable()
	if self != "" {
		candidate := filepath.Join(filepath.Dir(self), "commsync")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath("commsync"); err == nil {
		return p
	}
	return "commsync"
}

// ---------- main ----------

func defaultDBPath() string {
	if p := os.Getenv("COMMSYNC_DB"); p != "" {
		return p
	}
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return filepath.Join(".", "commsync.db")
	}
	return filepath.Join(u.HomeDir, ".local", "state", "commsync", "commsync.db")
}

func main() {
	var (
		dbPath = flag.String("db", defaultDBPath(), "path to commsync SQLite database")
		poll   = flag.Duration("poll", 2*time.Second, "polling interval")
	)
	flag.Parse()

	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "commsync-tui: db not found at %s: %v\n", *dbPath, err)
		fmt.Fprintf(os.Stderr, "set COMMSYNC_DB or pass -db\n")
		os.Exit(1)
	}

	st, err := openStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.close()

	identity := defaultIdentity()
	binPath := resolveCommSyncBin()

	p := tea.NewProgram(initialModel(st, *poll, identity, binPath, *dbPath), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatalf("tui: %v", err)
	}
}
