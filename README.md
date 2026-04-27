# pgloupe

> A jeweler's loupe for live Postgres traffic. `tail -f` for queries.

```
psql ──▶ pgloupe :25432 ──▶ Postgres :5432
              │
              ▼
           live TUI
```

Sit between any Postgres client (psql, your app) and any Postgres server. Watch every query (simple or extended-protocol prepared statement) with timing, row counts, and errors. Pause, scroll back, jump to newest. Quit with `q`.

Built for the moment you ask "what is my app actually sending to the database right now?" — answer it without modifying the app, the database, or restarting anything.

## Install

```bash
brew install swistaczek/tap/pgloupe
```

Or from source:

```bash
go install github.com/swistaczek/pgloupe@latest
```

Binaries published for **darwin** and **linux**, on **amd64** and **arm64**. Windows is not supported (the TUI needs a real terminal).

## Quick start

Three usage modes, in order of typical complexity.

### 1. Local Postgres

```bash
pgloupe --upstream localhost:5432
psql -h localhost -p 25432 -U you -d yourdb
```

### 2. Remote Postgres reachable via your own SSH tunnel

```bash
ssh -fN -L 15432:db-host:5432 you@bastion
pgloupe --upstream localhost:15432
psql -h localhost -p 25432 -U you -d yourdb
```

### 3. Self-contained: pgloupe opens the SSH tunnel

```bash
pgloupe --via you@bastion --upstream db-host:5432
psql -h localhost -p 25432 -U you -d yourdb
```

Reuses your `~/.ssh/config`, `ssh-agent`, `known_hosts`, jump hosts, MFA — everything you've already configured for `ssh`.

### 4. SSH + Docker container resolution

For Postgres running in a Docker container on the SSH host (no published port):

```bash
pgloupe --via root@server --container postgres-prod --docker-network bridge
```

pgloupe runs `ssh user@host docker inspect ...` to resolve the container's IP on the named network, then opens the tunnel automatically. The container IP can change on restart; pgloupe re-resolves on every run, so you don't have to.

## Keys

| Key | Action |
|---|---|
| `q` / `Ctrl-C` | Quit |
| `p` | Pause / resume autoscroll |
| `↑` / `k` | Scroll up |
| `↓` / `j` | Scroll down |
| `PgUp` / `Ctrl-B` | Page up |
| `PgDn` / `Ctrl-F` | Page down |
| `g` | Jump to newest, resume |
| `Home` / `G` | Jump to oldest |
| `?` | Toggle full help |

## Flags

```
--listen          127.0.0.1:25432   Local TCP address to listen on
--upstream        127.0.0.1:5432    Upstream Postgres address
--via             user@host         Open an SSH tunnel through this host
--container       name              With --via: resolve container IP via docker inspect
--docker-network  private           With --container: docker network to inspect
--remote-port     5432              With --container: remote Postgres port
--max-events      1000              Ring buffer size
--max-conns       64                Max concurrent client connections
--truncate-sql    80                SQL render width (0 = full)
--no-color                          Disable colors (also: NO_COLOR env var)
--version                           Print version and exit
```

## Security

pgloupe is a **local development tool**. Threat model and caveats:

- **Plaintext on the proxy hop.** pgloupe rejects SSL between the client and the proxy with a single `'N'` byte. The connection from psql/your app to `localhost:25432` is unencrypted. The proxy → upstream hop is whatever you set up (typically encrypted via SSH tunnel). Never run pgloupe on a non-trusted host or expose it on a non-loopback interface.
- **Listen address.** Default `127.0.0.1:25432`. pgloupe prints a stderr warning if you bind to anything else (empty host, `0.0.0.0`, public IP). Don't ignore the warning unless you've thought it through.
- **Query data in process memory.** Up to `--max-events` (default 1000) fully-rendered SQL statements live in pgloupe's heap, including any literal values your queries contain (passwords, API tokens, PII). A core dump or a process with debug access can read them. Don't run pgloupe on a shared machine. There's no `--redact` mode in v0.1.
- **Cleartext password auth.** If your Postgres uses `password` (cleartext) auth, the password is briefly in pgloupe's process memory as the `PasswordMessage` flows through. SCRAM (PG default since 10) and MD5 are challenge/response and don't expose the password on-wire. pgloupe explicitly does not observe `PasswordMessage` / `SASLResponse` — they're forwarded verbatim and not pushed into the events ring.
- **No telemetry, no auto-update, no remote callbacks.** pgloupe makes exactly two outbound TCP connections: one for the SSH tunnel (if `--via` is used; via the system `ssh` binary), and one to `--upstream`. Nothing else.

If you're handling EU customer data on this connection: the same considerations as for `psql` itself apply. pgloupe is not a database client; it's a passive observer in the same trust zone.

## What pgloupe does NOT do (yet)

- **No CancelRequest forwarding.** psql Ctrl-C uses a separate connection with `BackendKeyData` correlation; that round-trip is dropped in v0.1.
- **No bound parameter substitution.** Extended-protocol queries render as `WHERE id = $1` rather than `WHERE id = 42`. Decoding binary parameters requires per-OID type knowledge that the wire-protocol parser deliberately omits.
- **No persistent storage.** Events live in memory only. Quit pgloupe, the history is gone.
- **No filter view.** Coming in v0.2.
- **No COPY observation.** Bulk data flows are forwarded but not parsed into events.

## How it works

pgloupe is a transparent TCP proxy that parses the [Postgres wire protocol v3](https://www.postgresql.org/docs/current/protocol.html) using [`jackc/pgx/v5/pgproto3`](https://pkg.go.dev/github.com/jackc/pgx/v5/pgproto3). Two goroutines per connection forward bytes in each direction; observation hooks watch for `Query`, `Parse`, `Bind`, `Execute`, `CommandComplete`, `ErrorResponse`, and `ReadyForQuery` to emit one structured `Event` per logical query.

The TUI uses [Bubble Tea v2](https://github.com/charmbracelet/bubbletea) and renders events from a goroutine via `Program.Send`.

For the full design, see [`docs/DESIGN.md`](docs/DESIGN.md).

## Releasing

If you're maintaining a fork or releasing your own builds, see [`RELEASING.md`](RELEASING.md).

## Contributing

Bug reports and pull requests welcome. Run `go test -race ./...` before submitting. Keep changes small and focused.

## License

[MIT](LICENSE).
