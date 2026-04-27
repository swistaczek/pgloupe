package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

const (
	// defaultMaxBodyLen caps a single pgwire message body. Defends against
	// a malicious or buggy upstream sending a 2GB length-prefixed frame and
	// OOMing pgloupe. 16MiB comfortably covers normal psql/COPY traffic.
	defaultMaxBodyLen = 16 * 1024 * 1024
	// keepAlivePeriod sets TCP keepalive on accepted client + dialed upstream
	// conns so half-open connections don't leak goroutines forever.
	keepAlivePeriod = 30 * time.Second
)

// readStartup loops on ReceiveStartupMessage, refusing SSL/GSS encryption
// requests with a single 'N' byte (raw, not a BackendMessage), until the
// real StartupMessage arrives.
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
				return nil, fmt.Errorf("refuse encryption: %w", werr)
			}
		case *pgproto3.CancelRequest:
			return nil, errors.New("cancel request not supported in v1")
		default:
			return nil, fmt.Errorf("unexpected startup message %T", m)
		}
	}
}

// connState tracks per-connection query lifecycle. Mutated by both
// forwarding goroutines (frontend→server observes Query/Parse/Bind/Execute,
// server→client observes CommandComplete/Error/ReadyForQuery), so all
// public methods take the mutex.
//
// Two parallel inflight tracks:
//   - pending []*Event   FIFO of extended-protocol Executes awaiting terminator
//                        (pipelining: Parse-Bind-Execute, Parse-Bind-Execute, Sync)
//   - simpleSQL          set by Query, used to synthesize one Event per
//                        CommandComplete in a multi-statement simple query
//                        ("SELECT 1; SELECT 2;" yields two CCs)
type connState struct {
	mu          sync.Mutex
	id          uint64
	pending     []*Event
	simpleSQL   string
	simpleStart time.Time
	inSimple    bool
	preparedSQL map[string]string // Parse.Name → SQL text
	portalStmt  map[string]string // Bind.DestinationPortal → Bind.PreparedStatement
	txStatus    byte
	dropped     *atomic.Uint64 // shared with TUI for footer counter; nil in tests
}

func newConnState(id uint64) *connState {
	return &connState{
		id:          id,
		preparedSQL: map[string]string{},
		portalStmt:  map[string]string{},
		txStatus:    'I',
	}
}

func (s *connState) emit(e Event, events chan<- Event) {
	select {
	case events <- e:
	default:
		if s.dropped != nil {
			s.dropped.Add(1)
		}
	}
}

// finishOne consumes the next pending terminator. Order of preference:
//
//  1. extended-protocol pending queue (one entry per Execute)
//  2. simple-protocol synthesised event (one per CommandComplete during inSimple)
//
// Returns true if an event was emitted.
func (s *connState) finishOne(tag string, rows int64, errStr string, events chan<- Event) bool {
	if len(s.pending) > 0 {
		e := s.pending[0]
		s.pending = s.pending[1:]
		e.Finished = time.Now()
		e.Tag = tag
		e.Rows = rows
		e.Err = errStr
		s.emit(*e, events)
		return true
	}
	if s.inSimple {
		now := time.Now()
		e := Event{
			ConnID:   s.id,
			Started:  s.simpleStart,
			Finished: now,
			SQL:      s.simpleSQL,
			Tag:      tag,
			Rows:     rows,
			Err:      errStr,
			TxStatus: s.txStatus,
		}
		s.simpleStart = now // next CC of the same multi-stmt query starts here
		s.emit(e, events)
		return true
	}
	return false
}

func (s *connState) observeFrontend(msg pgproto3.FrontendMessage, _ chan<- Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// pgproto3.Receive returns pointers into an internal struct that gets
	// clobbered on the next Receive. Always copy out scalar fields below
	// (the field accesses below already do — m.String, m.Query, m.Name —
	// because Go's `string` is value-typed).
	switch m := msg.(type) {
	case *pgproto3.Query:
		s.inSimple = true
		s.simpleSQL = m.String
		s.simpleStart = time.Now()
	case *pgproto3.Parse:
		s.preparedSQL[m.Name] = m.Query
	case *pgproto3.Bind:
		s.portalStmt[m.DestinationPortal] = m.PreparedStatement
	case *pgproto3.Execute:
		stmt := s.portalStmt[m.Portal]
		s.pending = append(s.pending, &Event{
			ConnID:   s.id,
			Started:  time.Now(),
			SQL:      s.preparedSQL[stmt],
			TxStatus: s.txStatus,
		})
		// PasswordMessage / SASLResponse / SASLInitialResponse are intentionally
		// not observed — they may carry credentials.
	}
}

func (s *connState) observeBackend(msg pgproto3.BackendMessage, events chan<- Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch m := msg.(type) {
	case *pgproto3.CommandComplete:
		// CommandTag aliases the read buffer; parseCommandTag iterates the
		// bytes synchronously and string() copies before next Receive clobbers.
		rows, _ := parseCommandTag(m.CommandTag)
		s.finishOne(string(m.CommandTag), rows, "", events)
	case *pgproto3.EmptyQueryResponse:
		s.finishOne("EMPTY", 0, "", events)
	case *pgproto3.PortalSuspended:
		// Treat as a terminator for v1; multi-Execute on same portal will be
		// rendered as multiple SUSPENDED rows. v2 may accumulate.
		s.finishOne("SUSPENDED", 0, "", events)
	case *pgproto3.ErrorResponse:
		// Severity duplicates with the TUI's ERR badge; store SQLSTATE+Message only.
		errStr := fmt.Sprintf("%s: %s", m.Code, m.Message)
		s.finishOne("", 0, errStr, events)
	case *pgproto3.ReadyForQuery:
		s.txStatus = m.TxStatus
		s.inSimple = false
		s.simpleSQL = ""
		// Drain any unterminated pending entries — the server skipped past
		// Bind/Execute messages after a mid-pipeline ErrorResponse.
		for len(s.pending) > 0 {
			e := s.pending[0]
			s.pending = s.pending[1:]
			e.Finished = time.Now()
			e.Err = "skipped: prior error in pipeline"
			s.emit(*e, events)
		}
	}
}

// forward runs two goroutines that bidirectionally relay pgproto3 messages
// between client and server, observing query lifecycle into events. Returns
// when either side closes or context is cancelled. The StartupMessage must
// already have been forwarded by the caller.
func forward(ctx context.Context, client, server net.Conn, connID uint64, events chan<- Event, dropped *atomic.Uint64) {
	defer client.Close()
	defer server.Close()

	backend := pgproto3.NewBackend(client, client)
	frontend := pgproto3.NewFrontend(server, server)
	backend.SetMaxBodyLen(defaultMaxBodyLen)
	frontend.SetMaxBodyLen(defaultMaxBodyLen)

	state := newConnState(connID)
	state.dropped = dropped

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		client.Close()
		server.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer cancel()
		for {
			msg, err := backend.Receive()
			if err != nil {
				if !isClosedConnErr(err) {
					log.Printf("conn %d frontend: %v", connID, err)
				}
				return
			}
			state.observeFrontend(msg, events)
			frontend.Send(msg)
			if err := frontend.Flush(); err != nil {
				if !isClosedConnErr(err) {
					log.Printf("conn %d flush upstream: %v", connID, err)
				}
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		defer cancel()
		for {
			msg, err := frontend.Receive()
			if err != nil {
				if !isClosedConnErr(err) {
					log.Printf("conn %d backend: %v", connID, err)
				}
				return
			}
			state.observeBackend(msg, events)
			backend.Send(msg)
			if err := backend.Flush(); err != nil {
				if !isClosedConnErr(err) {
					log.Printf("conn %d flush client: %v", connID, err)
				}
				return
			}
		}
	}()

	wg.Wait()
}

func isClosedConnErr(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed)
}

// Serve listens on listenAddr, accepts connections (capped at maxConns),
// refuses SSL, dials upstreamAddr, forwards StartupMessage, then enters
// the bidirectional forwarding loop. Returns on ctx cancellation or
// listener error.
func Serve(ctx context.Context, listenAddr, upstreamAddr string, maxConns int, events chan<- Event, dropped *atomic.Uint64) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	defer ln.Close()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	sem := make(chan struct{}, maxConns)
	var nextConnID atomic.Uint64

	for {
		client, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			log.Printf("max-conns=%d reached; dropping client %s", maxConns, client.RemoteAddr())
			client.Close()
			continue
		}
		setKeepAlive(client)
		id := nextConnID.Add(1)
		go func() {
			defer func() { <-sem }()
			handleConn(ctx, client, upstreamAddr, id, events, dropped)
		}()
	}
}

func handleConn(ctx context.Context, client net.Conn, upstreamAddr string, connID uint64, events chan<- Event, dropped *atomic.Uint64) {
	backend := pgproto3.NewBackend(client, client)
	backend.SetMaxBodyLen(defaultMaxBodyLen)

	startup, err := readStartup(backend, client)
	if err != nil {
		log.Printf("conn %d startup: %v", connID, err)
		client.Close()
		return
	}

	server, err := net.Dial("tcp", upstreamAddr)
	if err != nil {
		log.Printf("conn %d dial upstream %s: %v", connID, upstreamAddr, err)
		client.Close()
		return
	}
	setKeepAlive(server)

	frontend := pgproto3.NewFrontend(server, server)
	frontend.SetMaxBodyLen(defaultMaxBodyLen)
	frontend.Send(startup)
	if err := frontend.Flush(); err != nil {
		log.Printf("conn %d forward startup: %v", connID, err)
		client.Close()
		server.Close()
		return
	}

	forward(ctx, client, server, connID, events, dropped)
}

func setKeepAlive(c net.Conn) {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tcp.SetKeepAlive(true)
	_ = tcp.SetKeepAlivePeriod(keepAlivePeriod)
}
