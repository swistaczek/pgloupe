package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
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

// connState tracks per-connection inflight query state. See docs/DESIGN.md
// §"Query observation model".
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
	s.inflight = &Event{ConnID: s.id, Started: time.Now(), SQL: sql, TxStatus: s.txStatus}
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
		s.startInflight(s.preparedSQL[stmt])
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

// forward runs two goroutines: client→server forwards FrontendMessages and
// observes Query/Parse/Bind/Execute; server→client forwards BackendMessages
// and observes CommandComplete/ErrorResponse/ReadyForQuery. Both close
// when either direction errors. The StartupMessage must already have been
// forwarded by the caller.
func forward(client, server net.Conn, connID uint64, events chan<- Event) {
	defer client.Close()
	defer server.Close()

	backend := pgproto3.NewBackend(client, client)
	frontend := pgproto3.NewFrontend(server, server)

	state := newConnState(connID)
	done := make(chan struct{}, 2)

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

	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := frontend.Receive()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
					_ = err
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

// Serve listens on listenAddr, accepts connections, refuses SSL,
// dials upstreamAddr, forwards StartupMessage, then enters the bidirectional
// forwarding loop. Returns on listener error.
func Serve(listenAddr, upstreamAddr string, events chan<- Event) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	defer ln.Close()

	var nextConnID atomic.Uint64
	for {
		client, err := ln.Accept()
		if err != nil {
			return err
		}
		id := nextConnID.Add(1)
		go handleConn(client, upstreamAddr, id, events)
	}
}

func handleConn(client net.Conn, upstreamAddr string, connID uint64, events chan<- Event) {
	backend := pgproto3.NewBackend(client, client)
	startup, err := readStartup(backend, client)
	if err != nil {
		client.Close()
		return
	}

	server, err := net.Dial("tcp", upstreamAddr)
	if err != nil {
		client.Close()
		return
	}
	frontend := pgproto3.NewFrontend(server, server)
	frontend.Send(startup)
	if err := frontend.Flush(); err != nil {
		client.Close()
		server.Close()
		return
	}

	forward(client, server, connID, events)
}
