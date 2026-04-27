# pgloupe

Live TUI for inspecting Postgres wire-protocol traffic through a tunnel — like `tail -f` for queries.

```
psql ──▶ pgloupe :25432 ──▶ tunnel :15432 ──▶ Postgres
              │
              ▼
           live TUI
```

Sit between any Postgres client and any Postgres server. Watch every query (simple or extended-protocol prepared statements) with timing, row counts, and errors. Pause and scroll back. Quit with `q`.

## Install

```bash
brew install swistaczek/tap/pgloupe
```

Or build from source:

```bash
go install github.com/swistaczek/pgloupe@latest
```

## Usage

```bash
# Terminal 1 — your existing tunnel surfacing prod Postgres at localhost:15432
make dbtunnel

# Terminal 2 — point pgloupe at the tunnel
pgloupe                 # listens on :25432, forwards to :15432

# Terminal 3 — connect psql to pgloupe instead of the raw tunnel
psql -h localhost -p 25432 -U you -d yourdb
```

Every query you type into psql now also appears live in the pgloupe TUI in Terminal 2.

## Flags

```
--listen      127.0.0.1:25432   TCP address to listen on
--upstream    127.0.0.1:15432   Upstream Postgres address (typically the SSH-tunnel local end)
--max-events  1000              Ring buffer size for in-memory events
--version                        Print version and exit
```

## Keys

| Key | Action |
|---|---|
| `q` / `Ctrl-C` | Quit |
| `p` | Toggle pause |
| `↑` / `k` | Scroll up |
| `↓` / `j` | Scroll down |
| `g` | Jump to newest |

## Design

See [`docs/DESIGN.md`](docs/DESIGN.md) for architecture, wire-protocol observation model, and TUI strategy.

## Implementation plan

The repo was built test-first following [`docs/IMPLEMENTATION_PLAN.md`](docs/IMPLEMENTATION_PLAN.md).

## License

MIT
