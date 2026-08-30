# termtext server

A WebSocket relay server. It is the source of truth for every conversation:
clients never talk to each other directly, they each hold one outbound
WebSocket connection to this server, and every message, contact request, and
presence update is routed through it.

## The hub pattern

The core of the server is `Hub` (`hub.go`) — a single goroutine that owns the
only copy of `map[username]*Client` and is the only code in the process
allowed to read or write it.

```
        register            route              unregister
readPump ──────▶  ┌──────────────────────┐  ◀────────── readPump
(conn A)          │         Hub.Run()      │            (conn B)
writePump ◀────── │  map[username]*Client   │ ──────▶ writePump
(conn A)  outbox  └──────────────────────┘  outbox  (conn B)
```

Every connection gets two goroutines:

- **readPump** — the only goroutine that calls `conn.ReadMessage`. It
  decodes each frame into a `protocol.Envelope` and either hands it to the
  hub (`hub.route <- routedEnvelope{...}`) or, for control events like
  register/unregister, sends directly on `hub.register`/`hub.unregister`.
  It never touches `hub.clients` itself.
- **writePump** — the only goroutine that calls `conn.WriteMessage`,
  satisfying gorilla/websocket's single-writer-per-connection requirement.
  It just drains a channel (`Client.outbox`) that the hub feeds.

`Hub.Run()` is a single `for { select { ... } }` loop over three channels:
`register`, `unregister`, and `route`. Because it's the *only* goroutine that
indexes into `h.clients`, there is no need for a `sync.Mutex` guarding the
map and no possibility of one goroutine reading the map mid-write by
another — every mutation is serialized simply by being a single-threaded
`select` loop. This is the "share memory by communicating" idiom: instead of
multiple goroutines synchronizing access to shared state with locks, state
lives inside one goroutine and everyone else sends it messages.

Routing a chat message is then just: look up the recipient's `*Client` in
the map, and non-blocking-send the envelope onto their `outbox` channel. If
that send would block (the recipient's writePump isn't keeping up), the hub
treats it like a dead connection — it drops the client and closes its
outbox — rather than either blocking the entire hub (which would stall
message delivery for every other online user waiting behind it in the
`route` channel) or silently dropping messages forever while pretending the
connection is healthy.

Reconnecting as the same user (new terminal, stale session, etc.) replaces
the old entry: the hub closes the old client's `outbox`, which unblocks its
`writePump`, which closes the socket, which makes the old `readPump`'s
`ReadMessage` return an error and unwind on its own. There's exactly one
path to connection teardown regardless of who or what triggered it.

## Why channels, not a mutex

A `sync.RWMutex` around `map[username]*Client` would work too, but the hub
loop is a better fit here because routing isn't just "read the map" — it's
"read the map, then send to a channel, and decide what to do if that send
would block." Doing all of that atomically under a lock means holding the
lock across a channel send, which risks a distinct deadlock class (a slow
consumer stalls every other goroutine holding or waiting on that lock). The
hub goroutine sidesteps this by never blocking indefinitely on anything in
its own loop body — the one send that could block (`target.outbox <-`) is
wrapped in `select { ... default: }`.

## NAT / firewall traversal

Every client — including two people behind different home routers with no
port forwarding — makes a single **outbound** TCP connection to this
server (`ws://` or `wss://`, port 443/8080). That's it. There is no
inbound connection to any client, ever: the server relays B's message to A
by writing it down the WebSocket connection **A already opened to the
server**, not by connecting to A.

This is exactly why the relay/hub model (what WhatsApp, Slack, Discord,
etc. all do) sidesteps NAT traversal entirely, unlike peer-to-peer designs
(WebRTC-style) which need STUN/TURN/ICE to punch through NAT. As long as
the server itself has a public address — trivial on any small VM — every
client can reach it with a plain outbound connection, and the server does
the fan-out.

## Persistence and auth

`server/store` is the only package that touches SQLite (`modernc.org/sqlite`
— pure Go, no cgo, no system SQLite install required). It owns the schema
and exposes typed methods (`CreateUser`, `GetUserByToken`, ...); nothing
else in the server writes raw SQL. `server/auth` handles bcrypt hashing and
session-token generation and has no database or HTTP dependency of its own,
so the crypto is easy to reason about (and test) in isolation.

Auth is deliberately low-tech per the project's own constraints: `/register`
and `/login` are plain HTTP handlers that hand back an opaque, random
256-bit token recorded in a `sessions` table — no JWT library, nothing
decoded or verified client-side. `GET /ws` requires that same token via
`Authorization: Bearer <token>`, resolves it back to a user with one lookup
join (`sessions` → `users`), and registers the hub connection under that
user's username. There's no separate "who are you" step on the socket
itself — the token *is* the identity.

Being a database row rather than a signed blob is what makes the two
things a token needs cheap: it expires 30 days after issue (an `expires_at`
column, checked by that same lookup join, so there's no window where an
expired token still works) and it can be revoked immediately by deleting
the row, which is all `POST /logout` does. Revocation is per token, so
logging out on one device leaves other sessions alone.

`/register` and `/login` are rate-limited per client IP — 5 back-to-back
requests, then one per 12 seconds, `429` with `Retry-After` beyond that.
They're the only endpoints reachable without a token, and bcrypt's
deliberate slowness cuts both ways: unthrottled, `/login` is a guessing
oracle *and* a cheap way for one caller to saturate CPU. The bucket key is
the peer address off the TCP connection, never `X-Forwarded-For` — that
header is attacker-controlled unless a trusted proxy is known to be
rewriting it, and honoring it blindly would let one caller bypass the
limit just by varying a string. A deployment terminating TLS at a proxy
needs to teach `clientIP` about it explicitly.

## Contacts, conversations, and messages

`contacts` holds one row per pair of users (`user_a`, `user_b`, `status`).
Accepting a request flips `status` to `accepted` *and* calls
`GetOrCreateConversation`, which finds-or-creates the row in
`conversations` for that pair — so every accepted contact has a
`conversation_id` ready to hand the client immediately, with no separate
round trip to set one up. `contact_list` and `contact_accept` both include
it for exactly that reason.

Every `message` is written to SQLite before anything else happens to it —
that write is the source of truth, not an afterthought. If the recipient
is connected, the hub relays it live and the row's `delivered` flag flips
to true; if not, the send handler simply returns after the write. There is
no separate offline-message queue table: `delivered = 0` *is* the queue,
and `Store.UndeliveredMessagesFor` is exactly "what's still queued for this
user," replayed in order the next time they connect
(`flushOfflineMessages` in `messages.go`). The same idea extends to
contacts — a `pending` row in `contacts` is what gets replayed as a
`contact_request` on the recipient's next connect, so there's a consistent
pattern across the whole server: **durable state in SQLite is the queue;
live delivery over the hub is just an optimization on top of it.**

`typing` and `presence` are the exception — genuinely ephemeral, never
written to SQLite, dropped outright if the other side isn't connected to
receive them live. Presence in particular self-heals for free: even if an
online/offline broadcast is missed, the next `contact_list` a contact
receives reports current status anyway, so there's nothing to reconcile.

Knowing whether to relay live at all requires asking the hub who's
currently connected — `Hub.OnlineStatus` (for `contact_list`) and the
`delivered` reply channel on `routedEnvelope` (for a single message send)
are both synchronous request/response calls over channels, the same
"talk to the goroutine that owns the state" pattern as `register`/
`unregister`/`route`, just answering a question instead of mutating
anything.

## Self-hosting

```sh
go run ./server -addr :8080 -db termtext.db
```

`-db` defaults to `termtext.db` in the working directory and is created
automatically on first run, schema included. Quick manual check:

```sh
curl -s -X POST localhost:8080/register -d '{"username":"alice","password":"hunter2"}'
# => {"token":"..."}

curl -s -X POST localhost:8080/login -d '{"username":"alice","password":"hunter2"}'
# => {"token":"..."}
```

### Configuration

| Setting | Flag | Env var | Default | Meaning |
|---|---|---|---|---|
| Listen address | `-addr` | — | `:8080` | HTTP/WebSocket listen address |
| Database path | `-db` | — | `termtext.db` | SQLite file, created on first run |
| Upload directory | `-uploads` | `UPLOAD_DIR` | `./uploads` | Where uploaded files (phase 11) are stored on disk |
| Upload size cap | `-max-upload-size` | `MAX_UPLOAD_SIZE` | `25MB` | Per-file cap enforced in `POST /upload`, human-readable (`500KB`, `1GB`, ...) |
| Allowed browser origins | `-allowed-origins` | `ALLOWED_ORIGINS` | *(empty)* | Comma-separated origins permitted to open a WebSocket, e.g. `https://chat.example.com`. Empty refuses every browser origin; native clients send no `Origin` and are unaffected |

`-addr` and `-db` are flag-only — they're the kind of setting that varies
per invocation (e.g. running two local instances side by side), whereas
`UPLOAD_DIR`/`MAX_UPLOAD_SIZE` are the kind a container image or systemd
unit sets once as part of the deployment environment, without touching the
command line at all. Where both a flag and an env var are supported, the
flag wins if both are given; leaving the flag unset falls back to the env
var, then to the hardcoded default. `MAX_UPLOAD_SIZE` accepts a bare
number of bytes or a size with a `B`/`KB`/`MB`/`GB` suffix (case-
insensitive, decimals allowed — `1.5GB` is valid); an invalid value fails
fast at startup rather than silently falling back to the default.

```sh
UPLOAD_DIR=/var/lib/termtext/uploads MAX_UPLOAD_SIZE=100MB \
  go run ./server -addr :8080 -db /var/lib/termtext/termtext.db
```

### Pointing a client at a deployed server

The client (`/client`) defaults to `localhost:8080`, or a server address
saved in `~/.chattui/config.json` from a previous successful login. To
point it at a server deployed elsewhere:

```sh
go run ./client -server example.com:8080
```

The address is saved to the config file on the next successful
login/register, so subsequent launches against the same server don't need
`-server` repeated. `-new` (also client-only) skips the saved config
entirely and starts a fresh throwaway identity — handy for running
multiple independent clients against the same server locally.

`-server` also accepts a full URL, which is how you reach a server behind
TLS — `https://` implies `wss://` for the socket, and there's a `-tls`
flag for the same effect with a bare `host:port`:

```sh
go run ./client -server https://chat.example.com
```

Plain HTTP remains the default, so nothing changes for a local server.
Whichever you use is saved alongside the address, since it's part of how
to reach that address rather than a per-launch choice — a saved `https`
session comes back up as `https` without re-passing the flag. Note that
the server itself doesn't terminate TLS; put it behind a reverse proxy
that does, and set `ALLOWED_ORIGINS` if anything browser-based will
connect.

See `PROTOCOL.md` at the repo root for the full wire format and phased
implementation status.
