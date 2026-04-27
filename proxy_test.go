package main

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// TestRefuseSSLOnStartup feeds an SSLRequest then a real StartupMessage and
// asserts the proxy writes a single 'N' byte and then accepts the StartupMessage.
func TestRefuseSSLOnStartup(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	resultCh := make(chan *pgproto3.StartupMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		backend := pgproto3.NewBackend(serverConn, serverConn)
		startup, err := readStartup(backend, serverConn)
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- startup
	}()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)
	frontend.Send(&pgproto3.SSLRequest{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush ssl: %v", err)
	}

	buf := make([]byte, 1)
	if err := clientConn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := clientConn.Read(buf); err != nil {
		t.Fatalf("read N byte: %v", err)
	}
	if buf[0] != 'N' {
		t.Fatalf("got %q, want 'N'", buf[0])
	}

	frontend.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "appdb"},
	})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush startup: %v", err)
	}

	select {
	case startup := <-resultCh:
		if startup.Parameters["user"] != "alice" {
			t.Fatalf("user=%q, want alice", startup.Parameters["user"])
		}
		if startup.Parameters["database"] != "appdb" {
			t.Fatalf("database=%q, want appdb", startup.Parameters["database"])
		}
	case err := <-errCh:
		t.Fatalf("readStartup: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("readStartup timed out")
	}
}

func TestRefuseGSSOnStartup(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	resultCh := make(chan *pgproto3.StartupMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		backend := pgproto3.NewBackend(serverConn, serverConn)
		startup, err := readStartup(backend, serverConn)
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- startup
	}()

	frontend := pgproto3.NewFrontend(clientConn, clientConn)
	frontend.Send(&pgproto3.GSSEncRequest{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush gss: %v", err)
	}

	buf := make([]byte, 1)
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := clientConn.Read(buf); err != nil {
		t.Fatalf("read N byte: %v", err)
	}
	if buf[0] != 'N' {
		t.Fatalf("got %q, want 'N'", buf[0])
	}

	frontend.Send(&pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "bob"},
	})
	frontend.Flush()

	select {
	case startup := <-resultCh:
		if startup.Parameters["user"] != "bob" {
			t.Fatalf("user=%q, want bob", startup.Parameters["user"])
		}
	case err := <-errCh:
		t.Fatalf("readStartup: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("readStartup timed out")
	}
}

// runForward starts a forward goroutine in the background and returns the
// pipes the test can use as client + upstream surrogate.
func runForward(t *testing.T) (clientConn net.Conn, upstreamConn net.Conn, events chan Event, cancel func()) {
	t.Helper()
	clientConn, proxyClient := net.Pipe()
	proxyServer, upstreamConn := net.Pipe()
	events = make(chan Event, 64)
	dropped := &atomic.Uint64{}
	ctx, cancelFn := context.WithCancel(context.Background())
	go forward(ctx, proxyClient, proxyServer, 1, events, dropped)
	cancel = func() {
		cancelFn()
		clientConn.Close()
		upstreamConn.Close()
	}
	return
}

func TestForwardSimpleQueryRoundTrip(t *testing.T) {
	clientConn, upstreamConn, events, cancel := runForward(t)
	defer cancel()

	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Query{String: "SELECT 1"})
	if err := clientFE.Flush(); err != nil {
		t.Fatalf("client send Q: %v", err)
	}

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	got, err := upstreamBE.Receive()
	if err != nil {
		t.Fatalf("upstream recv: %v", err)
	}
	if q, ok := got.(*pgproto3.Query); !ok || q.String != "SELECT 1" {
		t.Fatalf("upstream saw %T %+v, want Query{SELECT 1}", got, got)
	}

	upstreamBE.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	upstreamBE.Flush()

	clientReader := pgproto3.NewFrontend(clientConn, clientConn)
	for i := 0; i < 2; i++ {
		clientReader.Receive()
	}

	select {
	case e := <-events:
		if e.SQL != "SELECT 1" || e.Tag != "SELECT 1" || e.Rows != 1 || e.Status() != StatusOK {
			t.Fatalf("e=%+v, want SQL/Tag SELECT 1 / Rows 1 / OK", e)
		}
		if e.Duration() <= 0 {
			t.Fatal("duration must be positive")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

func TestForwardSimpleQueryError(t *testing.T) {
	clientConn, upstreamConn, events, cancel := runForward(t)
	defer cancel()

	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Query{String: "SELECT * FROM nope"})
	clientFE.Flush()

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	upstreamBE.Receive()

	upstreamBE.Send(&pgproto3.ErrorResponse{
		Severity: "ERROR", Code: "42P01", Message: `relation "nope" does not exist`,
	})
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	upstreamBE.Flush()

	clientReader := pgproto3.NewFrontend(clientConn, clientConn)
	clientReader.Receive()
	clientReader.Receive()

	select {
	case e := <-events:
		if e.SQL != "SELECT * FROM nope" || e.Status() != StatusErr {
			t.Fatalf("e=%+v, want SQL set + StatusErr", e)
		}
		if !strings.Contains(e.Err, "42P01") {
			t.Fatalf("Err missing SQLSTATE: %q", e.Err)
		}
		// I1 fix: should NOT contain duplicate "ERROR:" prefix (Severity is implicit via badge).
		if strings.Contains(e.Err, "ERROR:") {
			t.Fatalf("Err includes redundant ERROR: prefix: %q", e.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

func TestForwardExtendedProtocolPaired(t *testing.T) {
	clientConn, upstreamConn, events, cancel := runForward(t)
	defer cancel()

	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Parse{Name: "stmt1", Query: "SELECT $1::int"})
	clientFE.Send(&pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "stmt1"})
	clientFE.Send(&pgproto3.Execute{Portal: "p1"})
	clientFE.Send(&pgproto3.Sync{})
	clientFE.Flush()

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	for i := 0; i < 4; i++ {
		if _, err := upstreamBE.Receive(); err != nil {
			t.Fatalf("upstream recv %d: %v", i, err)
		}
	}

	upstreamBE.Send(&pgproto3.ParseComplete{})
	upstreamBE.Send(&pgproto3.BindComplete{})
	upstreamBE.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	upstreamBE.Flush()

	clientReader := pgproto3.NewFrontend(clientConn, clientConn)
	for i := 0; i < 4; i++ {
		clientReader.Receive()
	}

	select {
	case e := <-events:
		if e.SQL != "SELECT $1::int" {
			t.Fatalf("e.SQL=%q, want SELECT $1::int", e.SQL)
		}
		if e.Tag != "SELECT 1" || e.Rows != 1 {
			t.Fatalf("e.Tag=%q rows=%d, want SELECT 1 / 1", e.Tag, e.Rows)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

// TestForwardMultiStatementSimple — C2 fix. Single Query with two statements
// MUST yield two events (PG sends one CC per statement).
func TestForwardMultiStatementSimple(t *testing.T) {
	clientConn, upstreamConn, events, cancel := runForward(t)
	defer cancel()

	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Query{String: "SELECT 1; SELECT 2"})
	clientFE.Flush()

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	upstreamBE.Receive()

	upstreamBE.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	upstreamBE.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}) // 1 row each
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	upstreamBE.Flush()

	clientReader := pgproto3.NewFrontend(clientConn, clientConn)
	for i := 0; i < 3; i++ {
		clientReader.Receive()
	}

	got := drainEvents(events, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	for i, e := range got {
		if e.SQL != "SELECT 1; SELECT 2" {
			t.Errorf("got[%d].SQL=%q, want full multi-stmt", i, e.SQL)
		}
		if e.Tag != "SELECT 1" {
			t.Errorf("got[%d].Tag=%q, want SELECT 1", i, e.Tag)
		}
	}
}

// TestForwardPipelinedExtended — C2 fix. Two Parse-Bind-Execute sequences
// before one Sync MUST yield two events (one per Execute).
func TestForwardPipelinedExtended(t *testing.T) {
	clientConn, upstreamConn, events, cancel := runForward(t)
	defer cancel()

	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Parse{Name: "s1", Query: "SELECT 1"})
	clientFE.Send(&pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "s1"})
	clientFE.Send(&pgproto3.Execute{Portal: "p1"})
	clientFE.Send(&pgproto3.Parse{Name: "s2", Query: "SELECT 2"})
	clientFE.Send(&pgproto3.Bind{DestinationPortal: "p2", PreparedStatement: "s2"})
	clientFE.Send(&pgproto3.Execute{Portal: "p2"})
	clientFE.Send(&pgproto3.Sync{})
	clientFE.Flush()

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	for i := 0; i < 7; i++ {
		upstreamBE.Receive()
	}

	upstreamBE.Send(&pgproto3.ParseComplete{})
	upstreamBE.Send(&pgproto3.BindComplete{})
	upstreamBE.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	upstreamBE.Send(&pgproto3.ParseComplete{})
	upstreamBE.Send(&pgproto3.BindComplete{})
	upstreamBE.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	upstreamBE.Flush()

	clientReader := pgproto3.NewFrontend(clientConn, clientConn)
	for i := 0; i < 7; i++ {
		clientReader.Receive()
	}

	got := drainEvents(events, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].SQL != "SELECT 1" {
		t.Errorf("got[0].SQL=%q, want SELECT 1", got[0].SQL)
	}
	if got[1].SQL != "SELECT 2" {
		t.Errorf("got[1].SQL=%q, want SELECT 2", got[1].SQL)
	}
}

// TestForwardPipelineErrorDrains — mid-pipeline ErrorResponse: subsequent
// Bind/Execute messages are skipped by PG until Sync, so their pending
// entries must be drained as "skipped" events on ReadyForQuery.
func TestForwardPipelineErrorDrains(t *testing.T) {
	clientConn, upstreamConn, events, cancel := runForward(t)
	defer cancel()

	clientFE := pgproto3.NewFrontend(clientConn, clientConn)
	clientFE.Send(&pgproto3.Parse{Name: "s1", Query: "SELECT badcol"})
	clientFE.Send(&pgproto3.Bind{DestinationPortal: "p1", PreparedStatement: "s1"})
	clientFE.Send(&pgproto3.Execute{Portal: "p1"})
	clientFE.Send(&pgproto3.Parse{Name: "s2", Query: "SELECT 2"})
	clientFE.Send(&pgproto3.Bind{DestinationPortal: "p2", PreparedStatement: "s2"})
	clientFE.Send(&pgproto3.Execute{Portal: "p2"})
	clientFE.Send(&pgproto3.Sync{})
	clientFE.Flush()

	upstreamBE := pgproto3.NewBackend(upstreamConn, upstreamConn)
	for i := 0; i < 7; i++ {
		upstreamBE.Receive()
	}

	upstreamBE.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42703", Message: "column \"badcol\" does not exist"})
	upstreamBE.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	upstreamBE.Flush()

	clientReader := pgproto3.NewFrontend(clientConn, clientConn)
	clientReader.Receive()
	clientReader.Receive()

	got := drainEvents(events, 2, 2*time.Second)
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Status() != StatusErr {
		t.Errorf("got[0] status=%v, want Err (the failing query)", got[0].Status())
	}
	if got[1].Status() != StatusErr || !strings.Contains(got[1].Err, "skipped") {
		t.Errorf("got[1] status=%v err=%q, want Err with 'skipped' marker", got[1].Status(), got[1].Err)
	}
}

func drainEvents(ch <-chan Event, want int, deadline time.Duration) []Event {
	out := make([]Event, 0, want)
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for len(out) < want {
		select {
		case e := <-ch:
			out = append(out, e)
		case <-timer.C:
			return out
		}
	}
	return out
}
