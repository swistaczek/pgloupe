# pgloupe — Design

> A jeweler's loupe for live Postgres traffic. Sit between a client (psql, app) and a server, observe wire-protocol messages as they pass, render queries — with timing, row counts, and errors — into a TUI.

## Goal

Give a developer a one-command way to *see* what their tunnel is sending: every query (simple or extended), how long it took, how many rows came back, and what failed. Pause and scroll back. Quit.

```
psql ──▶ pgloupe :25432 ──▶ tunnel :15432 ──▶ Postgres
              │
              ▼
           live TUI
```

## Non-goals (v1)

- **No write-blocking, no rewrite, no policy.** It's a transparent observer; bytes pass through unchanged.
- **No protocol-level encryption with the server.** Already handled outside (SSH tunnel). pgloupe refuses SSL upgrade between client↔proxy with a single `'N'` byte.
- **No multi-tenant deployment.** Local dev tool, single user.
- **No persistent storage.** All events are in memory, capped at 1,000.
- **No bound-parameter substitution into SQL.** Parameters in extended protocol are raw bytes (text *or* binary format); decoding binary requires per-OID type knowledge that `pgproto3` deliberately omits. We display `$1, $2, …` and a "5 params" hint, defer rendering.
- **No CancelRequest forwarding.** psql Ctrl-C uses a separate connection with `BackendKeyData` correlation; that's a v2 feature.
- **No COPY observation.** Bulk data flows are out of scope for v1.

## Architecture

Three responsibilities, three files.

```
main.go      flags, wiring, signal handling
proxy.go     TCP listener, per-conn pgwire forwarding + observation
tui.go       Bubble Tea model, async event consumption, rendering
events.go    Event type, ring buffer (shared between proxy producer + tui consumer)
```

Channel-based decoupling: `proxy` writes `Event` values to `chan Event`; `tui` drains via `Program.Send`. No shared mutable state.

```
                  ┌─────────────────────────────────┐
                  │           main.go               │
                  │  parse flags → start everything │
                  └──────────────┬──────────────────┘
                                 │
                ┌────────────────┴───────────────┐
                ▼                                ▼
    ┌─────────────────────┐         ┌────────────────────────┐
    │      proxy.go       │         │        tui.go          │
    │  Listen :25432      │  events │  Bubble Tea program    │
    │  per-conn goroutine │────────▶│  model.events []Event  │
    │  pgproto3 parse+fwd │  chan   │  Update / View         │
    └─────────────────────┘         └────────────────────────┘
```

## Wire-protocol observation

Built on `github.com/jackc/pgx/v5/pgproto3` (v5.7.2+). The library exposes two duplex parsers:

- `Backend` — server-side: reads `FrontendMessage`, writes `BackendMessage`. Used for the *downstream* (client-facing) socket.
- `Frontend` — client-side: reads `BackendMessage`, writes `FrontendMessage`. Used for the *upstream* (server-facing) socket.

Each connection runs two goroutines: client→server and server→client. Each is a tight `Receive → observe → Send → Flush` loop.

### Startup

```
ReceiveStartupMessage() returns one of:
  *SSLRequest      → write 'N' to raw conn, recurse
  *GSSEncRequest   → write 'N' to raw conn, recurse
  *CancelRequest   → drop (out of scope v1)
  *StartupMessage  → forward to upstream via Frontend.Send
```

The `'N'` byte is written *directly to net.Conn*, not via `Backend.Send` — it's a raw protocol byte, not a `BackendMessage`. (Verified against the canonical `pgfortune` example in pgx v5.7.2.)

`StartupMessage` *can* be forwarded with `Frontend.Send` — its `Encode` method emits the full length-prefixed wire bytes without a type-byte prefix, which is exactly what the wire expects for the first message.

### Steady-state forwarding

```go
// client → server
msg := backend.Receive()      // FrontendMessage
observe(msg, conn)            // emit Event for Query/Parse/Bind/Execute
frontend.Send(msg); frontend.Flush()

// server → client
msg := frontend.Receive()     // BackendMessage
observe(msg, conn)            // emit Event for CommandComplete/ErrorResponse
backend.Send(msg); backend.Flush()
```

**Aliasing pitfall.** `pgproto3.Receive` returns a pointer into an internal struct that gets clobbered on the next `Receive`. Any field we want for an `Event` must be copied out *synchronously* before re-receiving. The skeleton extracts scalar fields (`m.String`, `m.Query`, `m.CommandTag`) into the `Event` value before the next iteration — safe.

### Query observation model (per-connection state)

We track in-flight state per connection. Two lifecycle paths:

**Simple protocol:**
```
Query{sql} → CommandComplete{tag} | ErrorResponse → ReadyForQuery
```
Trivial: emit `started{sql, t0}` on Query, finalize on the next CommandComplete or ErrorResponse.

**Extended protocol:**
```
Parse(name, sql) → ParseComplete
Bind(portal, name, params) → BindComplete
Execute(portal) → CommandComplete | PortalSuspended | EmptyQueryResponse | ErrorResponse
Sync → ReadyForQuery
```

Per-conn state:

```go
type connState struct {
    preparedSQL  map[string]string  // stmtName → SQL
    portalToStmt map[string]string  // portalName → stmtName
    inflight     *Event             // current query event
    rfqAwaited   int                // count of pending ReadyForQuery
    txStatus     byte               // 'I'/'T'/'E'
}
```

- `Parse`: store `preparedSQL[m.Name] = m.Query`. Unnamed (`""`) is overwritten by each new Parse — that's fine, it matches Postgres semantics.
- `Bind`: store `portalToStmt[m.DestinationPortal] = m.PreparedStatement`.
- `Execute`: look up SQL via portal→stmt→preparedSQL, start the timer, set `inflight`.
- `CommandComplete` / `EmptyQueryResponse` / `ErrorResponse` / `PortalSuspended`: end the inflight, emit completed `Event`.
- `ReadyForQuery`: update `txStatus`. Useful as pipeline boundary and tx-state indicator.

Pipelining (multiple Parse+Bind+Execute before one Sync) is handled correctly because each Execute has its own terminator. Mid-pipeline ErrorResponse causes Postgres to skip until Sync; Bind/Execute messages between the error and Sync produce no terminator — we mark them `skipped` when we see the next ReadyForQuery.

### CommandTag → row count

`CommandComplete.CommandTag` is `[]byte` like `"SELECT 5"`, `"INSERT 0 1"`, `"UPDATE 3"`, `"BEGIN"`. Row count = **last numeric token** of the tag — that single rule covers every variant including `INSERT oid rows` (where the oid is always 0 in modern PG). Tags without a trailing number (`BEGIN`, `COMMIT`, `SET`, `CREATE TABLE`) yield no row count; we display the verb only.

### Error fields

From `ErrorResponse` we surface (in priority order): `Severity` (badge), `Code` (5-char SQLSTATE), `Message` (one-liner), `Detail` (secondary). Skip `Where`/`File`/`Line`/`Routine` for v1; expose them in a future "details" pane.

### Out-of-scope but still relayed

`AuthenticationOk`, `AuthenticationCleartextPassword`, `AuthenticationMD5Password`, `AuthenticationSASL*`, `ParameterStatus`, `BackendKeyData`, `NoticeResponse` — all relayed verbatim. None affect the query observation state machine.

## Threading model

```
main goroutine          → tea.Program.Run() (TUI render loop)
listen goroutine        → Accept loop (one per Listen)
per-connection × 2      → client→server forwarding, server→client forwarding
events channel consumer → goroutine that calls program.Send(eventMsg)
```

`Program.Send` is goroutine-safe (verified at `bubbletea/tea.go:1183-1188`). Per-conn state is owned by the conn's two goroutines via a `sync.Mutex` (or a per-conn dispatcher goroutine if we want lock-free).

## TUI

Built on `github.com/charmbracelet/bubbletea/v2` v2.0.6 + `bubbles/v2/key` + `lipgloss/v2`.

**Render strategy: custom `View()`, not `viewport`.** Reasons:
- `viewport.SetContent(string)` re-splits on `\n` every call (`bubbles/viewport/viewport.go:225-227`) — O(N) per event with N=1000 events appended one-by-one ⇒ O(N²) total. Bad for streams.
- Newest-at-top + autoscroll-with-pause is awkward for `viewport`'s `GotoBottom`/`AtBottom` model.
- Custom render is ~10 lines: render `events[scrollOffset : scrollOffset + visible]` with `visible = windowHeight − chrome`.

**Model:**
```go
type model struct {
    events       []Event   // newest at index 0, capped at 1000
    windowW      int
    windowH      int
    paused       bool
    scrollOffset int
}
```

**Keybindings:**
| Key | Action |
|---|---|
| `q` / Ctrl-C | Quit |
| `p` | Toggle pause (freezes autoscroll) |
| `↑` / `k` | Scroll up (auto-pauses) |
| `↓` / `j` | Scroll down |
| `g` | Jump to newest, unpause |

**Row rendering:**

```
HH:MM:SS.mmm  3.4ms   SELECT 5    SELECT id, name FROM hiring_applications WHERE…
HH:MM:SS.mmm  ERR     —           SELECT * FROM nonexistent_table  →  42P01: relation does not exist
HH:MM:SS.mmm  120ms   UPDATE 1    UPDATE users SET last_seen_at = NOW() WHERE id = $1
HH:MM:SS.mmm  …       —           SELECT pg_sleep(5)                                       (in flight)
```

Color via `lipgloss/v2`:
- Errors → red (`Color("9")`)
- In-flight → yellow (`Color("11")`)
- Completed → dim (`Color("245")`)
- Chrome (header/footer) → bold blue (`Color("63")`)

## Error handling

- TCP accept errors → log, keep listening.
- Per-connection errors → log, close both halves, drop in-flight events for that conn.
- `pgproto3` parse errors → log + close conn (the protocol is broken; can't recover).
- Channel-full (events buffer overflow) → drop the oldest event ("we'd rather lose history than block the proxy").

## Security / privacy

- **No raw bytes are logged by default.** We render structured fields (SQL text, error message, tags) but never dump packet hex. A `--debug` flag could enable hex dumping later but is off by default.
- **No remote callbacks, no telemetry, no auto-update.** Strictly local.
- **The proxy does not authenticate clients.** Anything that connects to `:25432` reaches the upstream. This is a *local* dev tool — bind to `127.0.0.1` only (default), require explicit `--listen 0.0.0.0` to expose.
- **The proxy does not see decrypted credentials in any meaningful way** because SSL between client↔proxy is refused, and the SSH tunnel handles transport encryption to the real server. Auth messages (MD5 challenge, SCRAM exchange) pass through verbatim — pgloupe doesn't observe them.

## Configuration

Single binary, flags only. No config file.

```
pgloupe \
  --listen 127.0.0.1:25432   \  # where psql connects
  --upstream 127.0.0.1:15432 \  # where the SSH tunnel surfaces
  --max-events 1000          \  # ring buffer size
  --truncate-sql 200         \  # render SQL up to N chars
```

All flags optional with sensible defaults. `pgloupe` with no args = `--listen 127.0.0.1:25432 --upstream 127.0.0.1:15432`.

## Future work (not v1)

- CancelRequest forwarding via BackendKeyData correlation.
- Param substitution (text-format only) — render `WHERE id = 42` instead of `WHERE id = $1`.
- Slow-query alert (highlight queries > 1s, red flash > 5s).
- Filter view (`/foo` shows only events matching regex `foo`).
- Save session to JSONL on quit (`-o session.jsonl`).
- `EXPLAIN` injection mode (intercept `Q` for `SELECT`, prepend `EXPLAIN`, render plan inline).
- `--debug` hex dumps for protocol-level debugging.
- Pgbouncer/pgcat compatibility checks (different framing, prepared statement reuse).

## Test strategy

Built TDD-first. Tests live next to source, `go test -race`.

- **Unit tests** for event ring buffer, CommandTag parser, per-conn state machine.
- **Wire tests** for the proxy: spin up a `net.Pipe` pair, feed canned `pgproto3` messages, assert the right events were emitted and bytes were forwarded byte-for-byte.
- **TUI tests** with [`teatest`](https://github.com/charmbracelet/x/tree/main/exp/teatest) for keybindings + render output.
- **Integration test** behind a build tag (`//go:build integration`) that runs against a local Postgres if `$PGLOUPE_INTEGRATION_DSN` is set. Default CI skips it.

## Sources

This design is grounded in source-verified research:

- pgproto3 v5.7.2 — `pgproto3/{backend,frontend,startup_message}.go`, `pgproto3/example/pgfortune/server.go`
- Bubble Tea v2.0.6 — `bubbletea/tea.go:1183-1188` (Program.Send), `examples/send-msg`, `examples/realtime`, `bubbles/viewport/viewport.go:225-227`
- GoReleaser v2.15.4 + goreleaser-action v7 — official `homebrew_formulas` docs, `charmbracelet/meta/goreleaser-full.yaml`, `cli/cli/.goreleaser.yml`
- PostgreSQL 17 protocol — §53.2 (Message Flow), §53.7 (Message Formats)
