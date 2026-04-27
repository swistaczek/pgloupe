# pgloupe Implementation Plan

> **For agentic workers:** Use [`superpowers:subagent-driven-development`](../../README.md) (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `pgloupe` v0.1.0 — a TCP proxy that observes Postgres wire-protocol traffic and renders queries (SQL, duration, row count, errors) in a Bubble Tea TUI.

**Architecture:** Three responsibilities, three files. `proxy.go` does pgproto3-based observation+forwarding. `tui.go` renders events asynchronously via `Program.Send`. `events.go` defines the shared `Event` type and ring buffer. `main.go` wires it all up. See `docs/DESIGN.md` for the full architecture rationale.

**Tech Stack:**
- Go 1.26
- `github.com/jackc/pgx/v5/pgproto3` v5.7.2 (wire parser)
- `github.com/charmbracelet/bubbletea/v2` v2.0.6 (TUI)
- `github.com/charmbracelet/bubbles/v2/key` (keybindings)
- `github.com/charmbracelet/lipgloss/v2` (styling)
- Stdlib only otherwise

---

## File structure

```
pgloupe/
├── docs/
│   ├── DESIGN.md
│   ├── IMPLEMENTATION_PLAN.md  ← you are here
│   └── USAGE.md
├── .github/workflows/
│   ├── test.yml
│   └── release.yml
├── .goreleaser.yaml
├── .gitignore
├── LICENSE                     ← MIT
├── README.md
├── go.mod
├── go.sum
├── main.go                     ← flags, signal handling, wiring
├── events.go                   ← Event type, ring buffer
├── events_test.go
├── proxy.go                    ← TCP listen, per-conn forwarding, observation
├── proxy_test.go               ← net.Pipe-based wire tests
├── tagparse.go                 ← CommandTag → row count helper
├── tagparse_test.go
├── tui.go                      ← Bubble Tea model, View, keybindings
└── tui_test.go                 ← teatest-based UI tests
```

---

## Task 1: Initialize Go module + .gitignore + LICENSE + base README

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `LICENSE`
- Create: `README.md`

- [ ] **Step 1: Init module**

```bash
cd /Users/ernest/projects/pgloupe
go mod init github.com/swistaczek/pgloupe
```

- [ ] **Step 2: .gitignore**

```
/dist/
/pgloupe
*.test
.DS_Store
.idea/
.vscode/
coverage.out
```

- [ ] **Step 3: LICENSE — MIT, year 2026, holder "swistaczek"**

(Standard MIT text, omitted for brevity — copy from https://opensource.org/licenses/MIT.)

- [ ] **Step 4: Stub README.md**

```markdown
# pgloupe

Live TUI for inspecting Postgres wire-protocol traffic through a tunnel — like `tail -f` for queries.

## Install

```
brew install swistaczek/tap/pgloupe
```

## Usage

```
make dbtunnel        # in another terminal: SSH-forwards prod PG to :15432
pgloupe              # starts the proxy on :25432 + TUI
psql -h localhost -p 25432 -U you -d yourdb
```

See [`docs/DESIGN.md`](docs/DESIGN.md) for architecture, [`docs/USAGE.md`](docs/USAGE.md) for full flags.

## License

MIT
```

- [ ] **Step 5: Commit**

```bash
git add go.mod .gitignore LICENSE README.md docs/
git commit -m "chore: scaffold pgloupe — module, license, docs"
```

---

## Task 2: Event type + ring buffer (events.go)

**Files:**
- Create: `events.go`
- Test: `events_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// events_test.go
package main

import (
	"testing"
	"time"
)

func TestEventStatusOK(t *testing.T) {
	e := Event{Started: time.Now(), Finished: time.Now().Add(time.Millisecond), Tag: "SELECT 5"}
	if e.Status() != StatusOK {
		t.Fatalf("expected StatusOK, got %v", e.Status())
	}
}

func TestEventStatusErr(t *testing.T) {
	e := Event{Started: time.Now(), Err: "42P01: relation does not exist"}
	if e.Status() != StatusErr {
		t.Fatalf("expected StatusErr, got %v", e.Status())
	}
}

func TestEventStatusInflight(t *testing.T) {
	e := Event{Started: time.Now()}
	if e.Status() != StatusInflight {
		t.Fatalf("expected StatusInflight, got %v", e.Status())
	}
}

func TestRingBufferPushAndLen(t *testing.T) {
	rb := newRingBuffer(3)
	rb.push(Event{SQL: "a"})
	rb.push(Event{SQL: "b"})
	if got := rb.len(); got != 2 {
		t.Fatalf("len=%d, want 2", got)
	}
}

func TestRingBufferEvictsOldest(t *testing.T) {
	rb := newRingBuffer(2)
	rb.push(Event{SQL: "a"})
	rb.push(Event{SQL: "b"})
	rb.push(Event{SQL: "c"})
	if rb.len() != 2 {
		t.Fatalf("len=%d, want 2", rb.len())
	}
	got := rb.snapshot()
	if got[0].SQL != "c" || got[1].SQL != "b" {
		t.Fatalf("snapshot=%v, want [c, b]", []string{got[0].SQL, got[1].SQL})
	}
}
```

- [ ] **Step 2: Run tests — expect FAIL ("undefined: Event")**

```bash
go test ./...
```

- [ ] **Step 3: Implement events.go**

```go
// events.go
package main

import "time"

type Status int

const (
	StatusInflight Status = iota
	StatusOK
	StatusErr
)

// Event is a single observed query exchange. Time-ordered; one per Execute (extended)
// or CommandComplete (simple). See docs/DESIGN.md §"Query observation model".
type Event struct {
	ConnID   uint64        // monotonic ID assigned per accepted client conn
	Started  time.Time
	Finished time.Time     // zero until terminator arrives
	SQL      string        // recovered via Query.String or Parse.Query (extended)
	Tag      string        // CommandComplete.CommandTag, e.g. "SELECT 5"
	Rows     int64         // parsed from Tag; 0 if unparseable
	Err      string        // formatted "Severity: Code: Message" if errored
	TxStatus byte          // 'I' / 'T' / 'E' from latest ReadyForQuery
}

func (e Event) Status() Status {
	switch {
	case e.Err != "":
		return StatusErr
	case e.Finished.IsZero():
		return StatusInflight
	default:
		return StatusOK
	}
}

func (e Event) Duration() time.Duration {
	if e.Finished.IsZero() {
		return 0
	}
	return e.Finished.Sub(e.Started)
}

// ringBuffer is a fixed-capacity newest-first event store.
// Push prepends; on overflow, the oldest (last) entry is dropped.
// Not goroutine-safe — caller (the TUI model) holds the only reference.
type ringBuffer struct {
	cap  int
	data []Event
}

func newRingBuffer(cap int) *ringBuffer { return &ringBuffer{cap: cap} }

func (r *ringBuffer) push(e Event) {
	r.data = append([]Event{e}, r.data...)
	if len(r.data) > r.cap {
		r.data = r.data[:r.cap]
	}
}

func (r *ringBuffer) len() int { return len(r.data) }

func (r *ringBuffer) snapshot() []Event {
	out := make([]Event, len(r.data))
	copy(out, r.data)
	return out
}
```

- [ ] **Step 4: Run tests — expect PASS**

```bash
go test ./...
```

- [ ] **Step 5: Commit**

```bash
git add events.go events_test.go
git commit -m "feat: Event type + ring buffer"
```

---

## Task 3: CommandTag parser (tagparse.go)

**Files:**
- Create: `tagparse.go`
- Test: `tagparse_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// tagparse_test.go
package main

import "testing"

func TestParseCommandTagSelect(t *testing.T) {
	rows, ok := parseCommandTag([]byte("SELECT 5"))
	if !ok || rows != 5 {
		t.Fatalf("got rows=%d ok=%v, want 5 true", rows, ok)
	}
}

func TestParseCommandTagInsert(t *testing.T) {
	rows, ok := parseCommandTag([]byte("INSERT 0 1"))
	if !ok || rows != 1 {
		t.Fatalf("got rows=%d ok=%v, want 1 true", rows, ok)
	}
}

func TestParseCommandTagUpdate(t *testing.T) {
	rows, ok := parseCommandTag([]byte("UPDATE 3"))
	if !ok || rows != 3 {
		t.Fatalf("got rows=%d ok=%v, want 3 true", rows, ok)
	}
}

func TestParseCommandTagBegin(t *testing.T) {
	_, ok := parseCommandTag([]byte("BEGIN"))
	if ok {
		t.Fatal("BEGIN should report ok=false (no row count)")
	}
}

func TestParseCommandTagEmpty(t *testing.T) {
	_, ok := parseCommandTag(nil)
	if ok {
		t.Fatal("empty tag should report ok=false")
	}
}
```

- [ ] **Step 2: Run tests — expect FAIL**

- [ ] **Step 3: Implement tagparse.go**

```go
// tagparse.go
package main

import (
	"bytes"
	"strconv"
)

// parseCommandTag extracts the trailing row count from a CommandComplete tag.
// Tags shaped like "SELECT N", "UPDATE N", "DELETE N", "INSERT 0 N", "MOVE N",
// "FETCH N", "COPY N" all match. Tags without a trailing number ("BEGIN",
// "COMMIT", "SET", "CREATE TABLE") return ok=false.
//
// Per PG 17 §53.7 "CommandComplete", the row count is always the last numeric
// field — including the INSERT 3-field "INSERT oid rows" form (oid is always 0
// in modern PG; we don't surface it).
func parseCommandTag(tag []byte) (rows int64, ok bool) {
	parts := bytes.Fields(tag)
	if len(parts) < 2 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(parts[len(parts)-1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
```

- [ ] **Step 4: Run tests — expect PASS**

- [ ] **Step 5: Commit**

```bash
git add tagparse.go tagparse_test.go
git commit -m "feat: CommandTag → row count parser"
```

---

## Task 4: Proxy startup + SSL refusal (proxy.go skeleton)

**Files:**
- Create: `proxy.go`
- Test: `proxy_test.go`

- [ ] **Step 1: Add pgx dependency**

```bash
go get github.com/jackc/pgx/v5/pgproto3@v5.7.2
```

- [ ] **Step 2: Write the failing test (SSL refusal)**

```go
// proxy_test.go
package main

import (
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// TestRefuseSSLOnStartup spins up a client conn talking to handleStartup,
// sends an SSLRequest, expects to read back a single 'N' byte, then is
// allowed to send a real StartupMessage which the handler returns.
func TestRefuseSSLOnStartup(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	done := make(chan struct{})
	var got *pgproto3.StartupMessage
	go func() {
		defer close(done)
		backend := pgproto3.NewBackend(serverConn, serverConn)
		startup, err := readStartup(backend, serverConn)
		if err != nil {
			t.Errorf("readStartup: %v", err)
			return
		}
		got = startup
	}()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)
	frontend.Send(&pgproto3.SSLRequest{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush ssl: %v", err)
	}

	buf := make([]byte, 1)
	clientConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := clientConn.Read(buf); err != nil {
		t.Fatalf("read N byte: %v", err)
	}
	if buf[0] != 'N' {
		t.Fatalf("got %q, want 'N'", buf[0])
	}

	frontend.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice"},
	})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush startup: %v", err)
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readStartup timed out")
	}
	if got == nil || got.Parameters["user"] != "alice" {
		t.Fatalf("got startup=%+v, want user=alice", got)
	}
}
```

- [ ] **Step 3: Run test — expect FAIL ("undefined: readStartup")**

- [ ] **Step 4: Implement proxy.go skeleton**

```go
// proxy.go
package main

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/jackc/pgx/v5/pgproto3"
)

// readStartup loops on ReceiveStartupMessage, refusing SSL/GSS encryption
// requests with a single 'N' byte (raw, not a BackendMessage), until the
// real StartupMessage arrives. Returns the StartupMessage to be forwarded.
func readStartup(backend *pgproto3.Backend, raw net.Conn) (*pgproto3.StartupMessage, error) {
	for {
		msg, err := backend.ReceiveStartupMessage()
		if err != nil {
			return nil, fmt.Errorf("receive startup: %w", err)
		}
		switch m := msg.(type) {
		case *pgproto3.StartupMessage:
			return m, nil
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, werr := raw.Write([]byte{'N'}); werr != nil {
				return nil, fmt.Errorf("refuse ssl: %w", werr)
			}
		case *pgproto3.CancelRequest:
			return nil, errors.New("cancel request not supported in v1")
		default:
			return nil, fmt.Errorf("unexpected startup message %T", m)
		}
	}
}

// Sentinel for graceful shutdown.
var errProxyClosed = errors.New("proxy closed")

// Stub functions filled in by later tasks; declared here so the package builds.
func _unusedTypeAnchor() error { return io.EOF }
```

- [ ] **Step 5: Run test — expect PASS**

- [ ] **Step 6: Commit**

```bash
git add proxy.go proxy_test.go go.mod go.sum
git commit -m "feat: proxy startup handshake — refuse SSL, accept StartupMessage"
```

---

## Task 5: Bidirectional forwarding loop (proxy.go expansion)

**Files:**
- Modify: `proxy.go`
- Modify: `proxy_test.go`

- [ ] **Step 1: Write the failing test (round-trip forwarding)**

```go
// proxy_test.go (append)
func TestForwardSimpleQueryRoundTrip(t *testing.T) {
	// Three pipes: clientConn ↔ proxyClient (proxy's downstream); proxyServer ↔ upstreamConn.
	clientConn, proxyClient := net.Pipe()
	proxyServer, upstreamConn := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	events := make(chan Event, 16)
	go forward(proxyClient, proxyServer, 1, events)

	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := clientFE.Flush(); err != nil {
		t.Fatalf("send Q: %v", err)
	}

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	got, err := upstreamBE.Receive()
	if err != nil {
		t.Fatalf("upstream recv: %v", err)
	}
	q, ok := got.(*pgproto3.Query)
	if !ok || q.String != "SELECT 1" {
		t.Fatalf("upstream saw %T %+v, want Query{SELECT 1}", got, got)
	}

	// Server replies CommandComplete.
	upstreamBE.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := upstreamBE.Flush(); err != nil {
		t.Fatalf("upstream flush: %v", err)
	}

	clientBE := pgproto3.NewFrontend(clientConn, clientConn)
	if _, err := clientBE.Receive(); err != nil {
		t.Fatalf("client recv CC: %v", err)
	}
	if _, err := clientBE.Receive(); err != nil {
		t.Fatalf("client recv RFQ: %v", err)
	}

	// Two events: started + finished, OR one finished event with both timestamps.
	// Check at least one event arrived with SQL "SELECT 1" and Tag "SELECT 1".
	var started, finished Event
	for i := 0; i < 2; i++ {
		select {
		case e := <-events:
			if e.SQL != "" {
				started = e
			}
			if e.Tag != "" {
				finished = e
			}
		case <-time.After(time.Second):
			t.Fatal("no event")
		}
	}
	if started.SQL != "SELECT 1" {
		t.Fatalf("started SQL=%q, want SELECT 1", started.SQL)
	}
	if finished.Tag != "SELECT 1" {
		t.Fatalf("finished Tag=%q, want SELECT 1", finished.Tag)
	}
}
```

- [ ] **Step 2: Run test — expect FAIL ("undefined: forward")**

- [ ] **Step 3: Implement forward()**

```go
// proxy.go (add)

// forward runs two goroutines: client→server forwards FrontendMessages and
// observes Query/Parse/Bind/Execute; server→client forwards BackendMessages
// and observes CommandComplete/ErrorResponse/ReadyForQuery. Both close
// when either direction errors. forward consumes the post-startup phase only;
// the StartupMessage must already have been forwarded by the caller.
func forward(client, server net.Conn, connID uint64, events chan<- Event) {
	defer client.Close()
	defer server.Close()

	backend := pgproto3.NewBackend(client, client)   // downstream (client-facing)
	frontend := pgproto3.NewFrontend(server, server) // upstream (server-facing)

	state := newConnState(connID)
	done := make(chan struct{}, 2)

	// client → server
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := backend.Receive()
			if err != nil {
				return
			}
			state.observeFrontend(msg, events)
			frontend.Send(msg)
			if err := frontend.Flush(); err != nil {
				return
			}
		}
	}()

	// server → client
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := frontend.Receive()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
					// log non-fatal — we'll exit anyway
				}
				return
			}
			state.observeBackend(msg, events)
			backend.Send(msg)
			if err := backend.Flush(); err != nil {
				return
			}
		}
	}()

	<-done
}
```

(observeFrontend / observeBackend / connState filled in next task — for now stub them out:)

```go
type connState struct{ id uint64 }

func newConnState(id uint64) *connState { return &connState{id: id} }

func (s *connState) observeFrontend(msg pgproto3.FrontendMessage, events chan<- Event) {
	now := time.Now()
	switch m := msg.(type) {
	case *pgproto3.Query:
		select {
		case events <- Event{ConnID: s.id, Started: now, SQL: m.String}:
		default:
		}
	}
}

func (s *connState) observeBackend(msg pgproto3.BackendMessage, events chan<- Event) {
	now := time.Now()
	switch m := msg.(type) {
	case *pgproto3.CommandComplete:
		tag := string(m.CommandTag)
		rows, _ := parseCommandTag(m.CommandTag)
		select {
		case events <- Event{ConnID: s.id, Finished: now, Tag: tag, Rows: rows}:
		default:
		}
	case *pgproto3.ErrorResponse:
		select {
		case events <- Event{ConnID: s.id, Finished: now, Err: fmt.Sprintf("%s: %s: %s", m.Severity, m.Code, m.Message)}:
		default:
		}
	}
}
```

(Add `time` and `fmt` imports.)

- [ ] **Step 4: Run test — expect PASS**

- [ ] **Step 5: Commit**

```bash
git add proxy.go proxy_test.go
git commit -m "feat: bidirectional forwarding with simple-protocol observation"
```

---

## Task 6: Per-connection state — pair started+finished events into one

**Files:**
- Modify: `proxy.go`
- Modify: `proxy_test.go`

The Task 5 implementation emits *two* events per simple query (one started, one finished). The TUI wants *one* event per query — the started event mutated to include the terminator. Refactor to track an inflight Event and only emit on terminator.

- [ ] **Step 1: Update test to assert one paired event**

```go
// proxy_test.go — replace TestForwardSimpleQueryRoundTrip's tail:
// (instead of looping for 2 events, expect 1 paired Event)
	select {
	case e := <-events:
		if e.SQL != "SELECT 1" {
			t.Fatalf("e.SQL=%q, want SELECT 1", e.SQL)
		}
		if e.Tag != "SELECT 1" {
			t.Fatalf("e.Tag=%q, want SELECT 1", e.Tag)
		}
		if e.Rows != 1 {
			t.Fatalf("e.Rows=%d, want 1", e.Rows)
		}
		if e.Status() != StatusOK {
			t.Fatalf("e.Status=%v, want OK", e.Status())
		}
		if e.Duration() <= 0 {
			t.Fatalf("e.Duration=%v, want >0", e.Duration())
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
```

Add a test for ErrorResponse pairing:

```go
func TestForwardSimpleQueryErrorPaired(t *testing.T) {
	clientConn, proxyClient := net.Pipe()
	proxyServer, upstreamConn := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	events := make(chan Event, 16)
	go forward(proxyClient, proxyServer, 1, events)

	pgproto3.NewFrontend(clientConn, clientConn).Send(&pgproto3.Query{String: "SELECT * FROM nope"})
	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Query{String: "SELECT * FROM nope"})
	clientFE.Flush()

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	upstreamBE.Receive() // drain

	upstreamBE.Send(&pgproto3.ErrorResponse{
		Severity: "ERROR", Code: "42P01", Message: "relation \"nope\" does not exist",
	})
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	upstreamBE.Flush()

	pgproto3.NewFrontend(clientConn, clientConn).Receive() // drain ER
	pgproto3.NewFrontend(clientConn, clientConn).Receive() // drain RFQ

	select {
	case e := <-events:
		if e.SQL != "SELECT * FROM nope" || e.Status() != StatusErr {
			t.Fatalf("e=%+v, want SQL set + StatusErr", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}
```

- [ ] **Step 2: Run tests — expect FAIL**

- [ ] **Step 3: Refactor connState to pair events**

```go
// Replace observeFrontend / observeBackend with state-machine versions:
type connState struct {
	id          uint64
	inflight    *Event
	preparedSQL map[string]string // Parse.Name → SQL
	portalStmt  map[string]string // Bind.DestinationPortal → Bind.PreparedStatement
	txStatus    byte
}

func newConnState(id uint64) *connState {
	return &connState{
		id:          id,
		preparedSQL: map[string]string{},
		portalStmt:  map[string]string{},
		txStatus:    'I',
	}
}

func (s *connState) startInflight(sql string) {
	now := time.Now()
	s.inflight = &Event{ConnID: s.id, Started: now, SQL: sql, TxStatus: s.txStatus}
}

func (s *connState) finishInflight(tag string, rows int64, errStr string, events chan<- Event) {
	if s.inflight == nil {
		return
	}
	s.inflight.Finished = time.Now()
	s.inflight.Tag = tag
	s.inflight.Rows = rows
	s.inflight.Err = errStr
	select {
	case events <- *s.inflight:
	default:
	}
	s.inflight = nil
}

func (s *connState) observeFrontend(msg pgproto3.FrontendMessage, events chan<- Event) {
	switch m := msg.(type) {
	case *pgproto3.Query:
		s.startInflight(m.String)
	case *pgproto3.Parse:
		s.preparedSQL[m.Name] = m.Query
	case *pgproto3.Bind:
		s.portalStmt[m.DestinationPortal] = m.PreparedStatement
	case *pgproto3.Execute:
		stmt := s.portalStmt[m.Portal]
		sql := s.preparedSQL[stmt]
		s.startInflight(sql)
	}
}

func (s *connState) observeBackend(msg pgproto3.BackendMessage, events chan<- Event) {
	switch m := msg.(type) {
	case *pgproto3.CommandComplete:
		rows, _ := parseCommandTag(m.CommandTag)
		s.finishInflight(string(m.CommandTag), rows, "", events)
	case *pgproto3.EmptyQueryResponse:
		s.finishInflight("EMPTY", 0, "", events)
	case *pgproto3.PortalSuspended:
		s.finishInflight("SUSPENDED", 0, "", events)
	case *pgproto3.ErrorResponse:
		errStr := fmt.Sprintf("%s: %s: %s", m.Severity, m.Code, m.Message)
		s.finishInflight("", 0, errStr, events)
	case *pgproto3.ReadyForQuery:
		s.txStatus = m.TxStatus
	}
}
```

- [ ] **Step 4: Run tests — expect PASS**

- [ ] **Step 5: Commit**

```bash
git add proxy.go proxy_test.go
git commit -m "feat: per-conn state — pair Query/Execute → CommandComplete into one Event"
```

---

## Task 7: Extended-protocol prepared-statement test (Parse → Bind → Execute → CommandComplete)

**Files:**
- Modify: `proxy_test.go`

This task validates the extended-protocol path against the Task 6 implementation. If the test passes immediately, great — it means Task 6 was correct. If it fails, fix the state machine.

- [ ] **Step 1: Add the failing test**

```go
// proxy_test.go (append)
func TestForwardExtendedProtocolPaired(t *testing.T) {
	clientConn, proxyClient := net.Pipe()
	proxyServer, upstreamConn := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	events := make(chan Event, 16)
	go forward(proxyClient, proxyServer, 1, events)

	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Parse{Name: "stmt1", Query: "SELECT $1::int"})
	clientFE.Send(&pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "stmt1"})
	clientFE.Send(&pgproto3.Execute{Portal: "p1"})
	clientFE.Send(&pgproto3.Sync{})
	clientFE.Flush()

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	for i := 0; i < 4; i++ { upstreamBE.Receive() } // P, B, E, S

	upstreamBE.Send(&pgproto3.ParseComplete{})
	upstreamBE.Send(&pgproto3.BindComplete{})
	upstreamBE.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	upstreamBE.Flush()

	clientReader := pgproto3.NewFrontend(clientConn, clientConn)
	for i := 0; i < 4; i++ { clientReader.Receive() }

	select {
	case e := <-events:
		if e.SQL != "SELECT $1::int" {
			t.Fatalf("e.SQL=%q, want SELECT $1::int", e.SQL)
		}
		if e.Tag != "SELECT 1" || e.Rows != 1 {
			t.Fatalf("e.Tag=%q rows=%d, want SELECT 1 / 1", e.Tag, e.Rows)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
}
```

- [ ] **Step 2: Run — expect PASS (or fix until pass)**

- [ ] **Step 3: Commit**

```bash
git add proxy_test.go
git commit -m "test: extended-protocol prepared-statement event pairing"
```

---

## Task 8: Listener + Serve loop

**Files:**
- Modify: `proxy.go`

- [ ] **Step 1: Add Serve function**

```go
// proxy.go (add)

// Serve listens on listenAddr and forwards each accepted client connection
// to upstreamAddr. Returns when the listener errors (e.g. closed).
func Serve(listenAddr, upstreamAddr string, events chan<- Event) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	defer ln.Close()

	var nextConnID uint64
	for {
		client, err := ln.Accept()
		if err != nil {
			return err
		}
		nextConnID++
		go handleConn(client, upstreamAddr, nextConnID, events)
	}
}

func handleConn(client net.Conn, upstreamAddr string, connID uint64, events chan<- Event) {
	defer client.Close()
	backend := pgproto3.NewBackend(client, client)
	startup, err := readStartup(backend, client)
	if err != nil {
		return
	}

	server, err := net.Dial("tcp", upstreamAddr)
	if err != nil {
		return
	}
	frontend := pgproto3.NewFrontend(server, server)
	frontend.Send(startup)
	if err := frontend.Flush(); err != nil {
		server.Close()
		return
	}

	forward(client, server, connID, events)
}
```

- [ ] **Step 2: Run all tests — expect PASS**

- [ ] **Step 3: Commit**

```bash
git add proxy.go
git commit -m "feat: TCP listener + per-conn handler"
```

---

## Task 9: TUI model + View (tui.go)

**Files:**
- Create: `tui.go`
- Test: `tui_test.go`

- [ ] **Step 1: Add bubbletea v2 deps**

```bash
go get github.com/charmbracelet/bubbletea/v2@v2.0.6
go get github.com/charmbracelet/bubbles/v2/key
go get github.com/charmbracelet/lipgloss/v2
```

- [ ] **Step 2: Write the failing test**

```go
// tui_test.go
package main

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea/v2"
)

func TestUpdateAppendsEventAtTop(t *testing.T) {
	m := newModel(3)
	updated, _ := m.Update(eventMsg{SQL: "first"})
	updated, _ = updated.Update(eventMsg{SQL: "second"})
	got := updated.(model).events.snapshot()
	if len(got) != 2 || got[0].SQL != "second" || got[1].SQL != "first" {
		t.Fatalf("got=%v, want [second, first]", got)
	}
}

func TestQuitKeyReturnsTeaQuit(t *testing.T) {
	m := newModel(3)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q'})
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd")
	}
}

func TestViewIncludesPausedBadgeWhenPaused(t *testing.T) {
	m := newModel(3)
	m.windowH = 10
	m.windowW = 80
	m.paused = true
	out := m.View()
	if !strings.Contains(out, "PAUSED") {
		t.Fatalf("View missing PAUSED badge:\n%s", out)
	}
}

func TestEventInflightRendersDots(t *testing.T) {
	m := newModel(3)
	m.windowH = 10
	m.windowW = 80
	m.events.push(Event{SQL: "SELECT pg_sleep(5)", Started: time.Now()})
	out := m.View()
	if !strings.Contains(out, "SELECT pg_sleep(5)") {
		t.Fatalf("View missing inflight SQL:\n%s", out)
	}
}
```

- [ ] **Step 3: Run — expect FAIL**

- [ ] **Step 4: Implement tui.go**

```go
// tui.go
package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/v2/key"
	tea "github.com/charmbracelet/bubbletea/v2"
	"github.com/charmbracelet/lipgloss/v2"
)

var (
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	inflightSty = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	chromeSty   = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true)
)

type keyMap struct{ Quit, Pause, Up, Down, Jump key.Binding }

var keys = keyMap{
	Quit:  key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Pause: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
	Up:    key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "scroll up")),
	Down:  key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll down")),
	Jump:  key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "jump to newest")),
}

type eventMsg Event

type model struct {
	events       *ringBuffer
	windowW      int
	windowH      int
	paused       bool
	scrollOffset int
}

func newModel(maxEvents int) model {
	return model{events: newRingBuffer(maxEvents)}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.windowW, m.windowH = msg.Width, msg.Height
	case eventMsg:
		m.events.push(Event(msg))
		if !m.paused {
			m.scrollOffset = 0
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Pause):
			m.paused = !m.paused
		case key.Matches(msg, keys.Up):
			if m.scrollOffset < m.events.len()-1 {
				m.scrollOffset++
				m.paused = true
			}
		case key.Matches(msg, keys.Down):
			if m.scrollOffset > 0 {
				m.scrollOffset--
			}
		case key.Matches(msg, keys.Jump):
			m.scrollOffset, m.paused = 0, false
		}
	}
	return m, nil
}

func (m model) View() string {
	var b strings.Builder
	header := "pgloupe"
	if m.paused {
		header += " [PAUSED]"
	}
	b.WriteString(chromeSty.Render(header))
	b.WriteString("\n")
	visible := m.windowH - 2
	if visible < 1 {
		visible = 1
	}
	snap := m.events.snapshot()
	end := m.scrollOffset + visible
	if end > len(snap) {
		end = len(snap)
	}
	for _, e := range snap[m.scrollOffset:end] {
		b.WriteString(renderRow(e))
		b.WriteString("\n")
	}
	b.WriteString(chromeSty.Render("q quit · p pause · ↑↓ scroll · g newest"))
	return b.String()
}

func renderRow(e Event) string {
	ts := e.Started.Format("15:04:05.000")
	switch e.Status() {
	case StatusErr:
		return errStyle.Render(fmt.Sprintf("%s  ERR     —          %s  →  %s", ts, e.SQL, e.Err))
	case StatusInflight:
		return inflightSty.Render(fmt.Sprintf("%s  …       —          %s", ts, e.SQL))
	default:
		return okStyle.Render(fmt.Sprintf("%s  %-7s %-10s %s", ts, e.Duration().Round(100_000), e.Tag, e.SQL))
	}
}

// RunTUI starts the Bubble Tea program and forwards events from the channel
// into model updates. Blocks until the user quits.
func RunTUI(events <-chan Event, maxEvents int) error {
	p := tea.NewProgram(newModel(maxEvents), tea.WithAltScreen())
	go func() {
		for ev := range events {
			p.Send(eventMsg(ev))
		}
	}()
	_, err := p.Run()
	return err
}
```

- [ ] **Step 5: Run — expect PASS**

- [ ] **Step 6: Commit**

```bash
git add tui.go tui_test.go go.mod go.sum
git commit -m "feat: TUI model with autoscroll, pause, scroll, color-coded rows"
```

---

## Task 10: main.go — flag parsing, signal handling, wiring

**Files:**
- Create: `main.go`

- [ ] **Step 1: Implement main.go**

```go
// main.go
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
)

// Injected by goreleaser ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		listenAddr   = flag.String("listen", "127.0.0.1:25432", "TCP address to listen on")
		upstreamAddr = flag.String("upstream", "127.0.0.1:15432", "Upstream Postgres address (typically the SSH-tunnel local end)")
		maxEvents    = flag.Int("max-events", 1000, "Ring buffer size for in-memory events")
		showVersion  = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("pgloupe %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	events := make(chan Event, 256)

	// Proxy goroutine.
	go func() {
		if err := Serve(*listenAddr, *upstreamAddr, events); err != nil {
			log.Printf("proxy: %v", err)
		}
	}()

	// Signal handling — translate Ctrl-C / SIGTERM into a clean exit.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		os.Exit(0)
	}()

	if err := RunTUI(events, *maxEvents); err != nil {
		log.Fatalf("tui: %v", err)
	}
}
```

- [ ] **Step 2: Build + smoke test**

```bash
go build ./...
./pgloupe --version
```

Expected: `pgloupe dev (commit none, built unknown)`

- [ ] **Step 3: Commit**

```bash
git add main.go
git commit -m "feat: main — flags, signal handling, wire proxy + TUI"
```

---

## Task 11: GitHub Actions — test.yml

**Files:**
- Create: `.github/workflows/test.yml`

- [ ] **Step 1: Write workflow**

(Content from research agent C — paste verbatim.)

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/test.yml
git commit -m "ci: test workflow on push/PR — vet, build, race tests"
```

---

## Task 12: GitHub Actions — release.yml + .goreleaser.yaml

**Files:**
- Create: `.github/workflows/release.yml`
- Create: `.goreleaser.yaml`

- [ ] **Step 1: Write both files** (content from research agent C, verbatim)

- [ ] **Step 2: Commit**

```bash
git add .github/workflows/release.yml .goreleaser.yaml
git commit -m "ci: goreleaser config + release workflow + homebrew tap publish"
```

---

## Task 13: Homebrew tap repo bootstrap

**Files (in `/Users/ernest/projects/homebrew-tap`):**
- Create: `README.md`

- [ ] **Step 1: One-line README**

```markdown
# homebrew-tap

Homebrew tap for [swistaczek](https://github.com/swistaczek)'s CLI tools.

## Available formulae

- [`pgloupe`](https://github.com/swistaczek/pgloupe) — `brew install swistaczek/tap/pgloupe`
```

- [ ] **Step 2: Commit + push**

```bash
cd /Users/ernest/projects/homebrew-tap
git add README.md
git commit -m "chore: bootstrap tap"
git push -u origin main
```

---

## Task 14: First push of pgloupe

**Files:** none new

- [ ] **Step 1: Push**

```bash
cd /Users/ernest/projects/pgloupe
git push -u origin main
```

- [ ] **Step 2: Verify CI runs**

```bash
gh run list --repo swistaczek/pgloupe --limit 3
```

Expected: `test` workflow run, status `in_progress` or `completed`.

---

## Task 15: README polish + USAGE.md

**Files:**
- Modify: `README.md`
- Create: `docs/USAGE.md`

Final user-facing docs once everything works. Include screenshot/asciinema if available. Defer until after first successful release.

- [ ] **Step 1: Expand README** (full install, demo gif placeholder, link to design + usage)
- [ ] **Step 2: Write USAGE.md** (every flag, examples, troubleshooting, integration with `make dbtunnel`)
- [ ] **Step 3: Commit**

```bash
git add README.md docs/USAGE.md
git commit -m "docs: full README + USAGE"
```

---

## Manual one-time setup (user action — swistaczek)

After Task 14 succeeds, before tagging v0.1.0:

1. Generate fine-grained PAT at https://github.com/settings/personal-access-tokens/new:
   - Resource owner: swistaczek
   - Repository access: only `swistaczek/homebrew-tap`
   - Permissions: `Contents: Read and write` + `Metadata: Read-only`
2. Add as repo secret on `swistaczek/pgloupe`:
   - https://github.com/swistaczek/pgloupe/settings/secrets/actions
   - Name: `HOMEBREW_TAP_GITHUB_TOKEN`
   - Value: paste the `github_pat_…` token

Then tag the first release:

```bash
git tag -a v0.1.0 -m "Initial release"
git push origin v0.1.0
```

GoReleaser will:
- Build 4 binaries (darwin/linux × amd64/arm64)
- Create a GitHub Release with tarballs + `checksums.txt`
- Commit `Formula/pgloupe.rb` to `swistaczek/homebrew-tap`

After ~3 minutes, verify:

```bash
brew install swistaczek/tap/pgloupe
pgloupe --version  # → pgloupe v0.1.0 (commit ..., built ...)
```
