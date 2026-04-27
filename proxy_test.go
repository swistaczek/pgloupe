package main

import (
	"net"
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

	// Client sends SSLRequest first.
	frontend := pgproto3.NewFrontend(clientConn, clientConn)
	frontend.Send(&pgproto3.SSLRequest{})
	if err := frontend.Flush(); err != nil {
		t.Fatalf("flush ssl: %v", err)
	}

	// Expect a single 'N' byte response.
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

	// Now send the real StartupMessage.
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

// TestRefuseGSSOnStartup mirrors the SSL refusal flow for GSSEncRequest.
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

// TestForwardSimpleQueryRoundTrip drives a full simple-protocol exchange
// through forward() and asserts: bytes are relayed verbatim in both
// directions, AND a single paired Event arrives with SQL+Tag+Rows+Duration.
func TestForwardSimpleQueryRoundTrip(t *testing.T) {
	clientConn, proxyClient := net.Pipe()
	proxyServer, upstreamConn := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	events := make(chan Event, 16)
	go forward(proxyClient, proxyServer, 1, events)

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
	if err := upstreamBE.Flush(); err != nil {
		t.Fatalf("upstream flush: %v", err)
	}

	clientReader := pgproto3.NewFrontend(clientConn, clientConn)
	for i := 0; i < 2; i++ {
		if _, err := clientReader.Receive(); err != nil {
			t.Fatalf("client recv %d: %v", i, err)
		}
	}

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
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

// TestForwardSimpleQueryError pairs a Query with an ErrorResponse.
func TestForwardSimpleQueryError(t *testing.T) {
	clientConn, proxyClient := net.Pipe()
	proxyServer, upstreamConn := net.Pipe()
	defer clientConn.Close()
	defer upstreamConn.Close()

	events := make(chan Event, 16)
	go forward(proxyClient, proxyServer, 1, events)

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
		if e.SQL != "SELECT * FROM nope" {
			t.Fatalf("e.SQL=%q", e.SQL)
		}
		if e.Status() != StatusErr {
			t.Fatalf("e.Status=%v, want Err", e.Status())
		}
		if e.Err == "" {
			t.Fatal("expected Err string")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

// TestForwardExtendedProtocol exercises Parse → Bind → Execute → CommandComplete.
// Verifies the prepared-statement → portal → SQL recovery chain.
func TestForwardExtendedProtocol(t *testing.T) {
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
