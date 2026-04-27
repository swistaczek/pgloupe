package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
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

	go func() {
		if err := Serve(*listenAddr, *upstreamAddr, events); err != nil {
			log.Printf("proxy: %v", err)
		}
	}()

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
