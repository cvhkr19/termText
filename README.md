# termtext

A WhatsApp-style real-time chat app that runs entirely in the terminal — a
Go WebSocket relay server and a [Bubble Tea](https://github.com/charmbracelet/bubbletea)
TUI client. No external chat SDK: the protocol, the hub, and the relay logic
are all hand-written.

```
 ┌─ contacts ──────┐ ┌─ conversation ─────────────────────────────┐
 │ ● alice          │ │                        hey, you around?    │
 │ ○ bob            │ │                                  17:04 ✓✓  │
 │                  │ │  yep, what's up                            │
 │                  │ │  17:05                                     │
 └──────────────────┘ └─────────────────────────────────────────────┘
   message, or /add <username>
```

## Features

- Real-time messaging over a single relay server — clients never connect to
  each other directly.
- Accounts, username-based contacts (request/accept/decline), and a live
  friends sidebar with online/offline presence.
- Delivery and read receipts (`✓` sent, `✓✓` delivered, `✓✓` read), a live
  typing indicator, and durable offline delivery — messages queue and flush
  on reconnect.
- Paginated chat history, loaded as you scroll.
- File transfer (`/send <path>`, `o` to download).
- Automatic reconnection with backoff, and a versioned wire protocol so an
  outdated client is told to upgrade instead of failing in a confusing way.

See [PROTOCOL.md](./PROTOCOL.md) for the wire format and
[server/README.md](./server/README.md) for the hub's concurrency design.

## Try it

The client is a single binary — grab the one for your OS from
[Releases](../../releases), then:

```
./termtext-client -server chat.example.com
```

First run drops you on a login/register screen; your session token is saved
to `~/.chattui/config.json` so future launches skip straight to the app. To
actually talk to someone, you both need to know each other's username —
there's no public directory, by design. Whoever you're demoing this to:
register any username, then `/add <the account you want to reach>`.

Flags:

| Flag | Purpose |
|---|---|
| `-server` | Server address — `host:port` or a full URL. Saved after first use. |
| `-tls` | Use `https://`/`wss://`. Implied automatically by an `https`/`wss` URL in `-server`. |
| `-new` | Use a throwaway config instead of `~/.chattui/config.json` — handy for running two clients locally to test with. |
| `-registration-code` | Only needed if the server you're connecting to has self-registration gated. |

## Running your own server

```
git clone https://github.com/you/termtext
cd termtext
docker compose up -d --build
```

Builds and starts the server locally on whatever machine you run this on
— no image registry involved. The container persists its SQLite database
and uploaded files in a `/data` volume, so `docker compose down` or a
rebuild doesn't lose anything.

That's enough for a local test. For a real deployment — a VM with a real
domain and TLS, or running it from your own machine long-term — see
[DEPLOYMENT.md](./DEPLOYMENT.md) for the full walkthrough (the binary
itself only speaks plain `http://`/`ws://`, so it needs a reverse proxy
in front of it before facing the internet). See also
[SECURITY.md](./SECURITY.md) for what's hardened and what isn't.

### Configuration

Every setting is a flag or an equivalent environment variable — the env var
is what you'll actually use in Docker/Compose; the flag is for running the
binary directly.

| Env var | Flag | Default | Purpose |
|---|---|---|---|
| `PORT` | `-addr` | `8080` | Listen port. |
| `DATABASE_PATH` | `-db` | `termtext.db` | SQLite file path. |
| `UPLOAD_DIR` | `-uploads` | `uploads` | Where uploaded files are stored. |
| `MAX_UPLOAD_SIZE` | `-max-upload-size` | `25MB` | Per-file upload cap. |
| `ALLOWED_ORIGINS` | `-allowed-origins` | *(none)* | Comma-separated browser origins allowed to open a WebSocket. Empty refuses all browser origins; native clients send no `Origin` and are unaffected. |
| `REGISTRATION_CODE` | `-registration-code` | *(none)* | If set, `/register` requires this exact code in the request body. Empty allows open self-registration. |

## Building from source

Requires Go 1.26+.

```
git clone https://github.com/you/termtext
cd termtext
go build -o bin/termtext-server ./server
go build -o bin/termtext-client ./client
```

Run the tests with `go test ./...`.

## Architecture, briefly

Two independent Go programs sharing one module: `/server` (the relay) and
`/client` (the TUI). The server never assumes anything about how many
clients are connected or who they are ahead of time — everything routes
through a single hub goroutine that owns the only copy of
`map[username]*Connection`, so there's no mutex around shared connection
state, just channels. Every write is durable — a message is in SQLite
before it's ever relayed, which is also what makes offline delivery work:
an undelivered message *is* the offline queue, not a separate structure.

Full design writeups:

- [PROTOCOL.md](./PROTOCOL.md) — the wire format, message by message.
- [server/README.md](./server/README.md) — the hub pattern and why it's
  channels instead of a mutex.
- [SECURITY.md](./SECURITY.md) — what's been hardened and what's
  explicitly out of scope.
- [DEPLOYMENT.md](./DEPLOYMENT.md) — running your own server on a real
  VM with a domain and TLS.

## Roadmap

- End-to-end encryption (X25519 + AES-GCM per conversation) — the protocol
  already treats message bodies as opaque, so this is additive, not a
  redesign.
- Group conversations.
- Configurable accent-color themes.
- Bubbles v2 migration, for click-to-position cursor editing in the
  compose box.

## License

MIT — see [LICENSE](./LICENSE).
