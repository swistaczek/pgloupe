package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
)

// Injected by goreleaser ldflags at release time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	var (
		listenAddr   = flag.String("listen", "127.0.0.1:25432", "Local TCP address to listen on")
		upstreamAddr = flag.String("upstream", "127.0.0.1:5432", "Upstream Postgres address to forward to")
		maxEvents    = flag.Int("max-events", 1000, "Ring buffer size for in-memory events")
		maxConns     = flag.Int("max-conns", 64, "Maximum concurrent client connections")
		truncate     = flag.Int("truncate-sql", 80, "Truncate rendered SQL beyond this many chars (0 = full width)")
		noColor      = flag.Bool("no-color", false, "Disable colored output (also honored: NO_COLOR env)")
		showVersion  = flag.Bool("version", false, "Print version and exit")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `pgloupe — live TUI for inspecting Postgres wire-protocol traffic.

Sit between any Postgres client and any Postgres server, observe every
query (simple or extended-protocol prepared statements) with timing,
row counts, and errors.

Usage:
  pgloupe [flags]

Examples:

  # Inspect a local Postgres on the default port
  pgloupe --upstream localhost:5432
  psql -h localhost -p 25432 -U you -d yourdb

  # Inspect via an existing SSH tunnel surfaced at :15432
  ssh -fN -L 15432:db-host:5432 you@bastion
  pgloupe --upstream localhost:15432

Flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Printf("pgloupe %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	warnNonLoopback(*listenAddr)

	events := make(chan Event, 256)
	dropped := &atomic.Uint64{}

	ctx, cancel := signalContext()
	defer cancel()

	go func() {
		if err := Serve(ctx, *listenAddr, *upstreamAddr, *maxConns, events, dropped); err != nil {
			log.Printf("proxy: %v", err)
			cancel()
		}
	}()

	opts := []ProgramOption{WithTruncateWidth(*truncate)}
	if *noColor || os.Getenv("NO_COLOR") != "" {
		opts = append(opts, WithNoColor())
	}

	if err := RunTUI(events, *maxEvents, dropped, opts...); err != nil {
		log.Fatalf("tui: %v", err)
	}
}

// signalContext returns a cancellable context that fires on SIGINT/SIGTERM.
// Bubble Tea also installs its own Ctrl-C handler that calls tea.Quit; both
// paths converge cleanly because tea.Quit triggers RunTUI to return, which
// triggers cancel via this context, which stops the proxy goroutines.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1) // buffer 1 — signal package drops if unbuffered
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()
	return ctx, cancel
}

// warnNonLoopback prints a stderr warning if the listen address binds to
// anything other than loopback. pgloupe forwards plaintext PG between the
// client and the proxy, so a non-loopback bind exposes prod query data to
// the local network.
func warnNonLoopback(addr string) {
	host := addr
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		host = addr[:idx]
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return
	case "":
		fmt.Fprintln(os.Stderr, "pgloupe: warning — empty host binds dual-stack INADDR_ANY; query data is exposed to the local network. Use 127.0.0.1 explicitly.")
	default:
		fmt.Fprintf(os.Stderr, "pgloupe: warning — listening on %q (non-loopback); query data is exposed to anyone who can reach this address.\n", host)
	}
}
