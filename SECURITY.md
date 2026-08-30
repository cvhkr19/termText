# Security

termtext is a portfolio project, not a vetted production messaging system.
It has had one focused security review and a round of fixes, documented
below, but it has not been independently audited. Don't use it for anything
where a compromise would actually matter to you.

## What's already hardened

- **Passwords** are hashed with bcrypt before storage; the raw password
  never touches disk. Length is capped at 72 bytes (bcrypt's own limit) so
  an over-long password fails cleanly with a `400` instead of an opaque
  `500`.
- **Sessions** are 256-bit tokens from `crypto/rand`, stored server-side,
  and expire after 30 days (`server/store/store.go`, `SessionTTL`).
  `/logout` revokes the presented token only, leaving other sessions for
  the same account untouched.
- **Rate limiting**: `/register` and `/login` share a token-bucket limiter
  keyed on the TCP peer address (never `X-Forwarded-For`, which is
  attacker-controlled unless a trusted proxy sets it — see
  `server/ratelimit.go`).
- **WebSocket origin checking**: `CheckOrigin` refuses any browser `Origin`
  not in `-allowed-origins`/`ALLOWED_ORIGINS`. Native clients (including
  termtext's own) send no `Origin` header and are unaffected; an empty
  allowlist refuses all browser origins by design.
- **Protocol versioning**: the WebSocket upgrade is rejected with `426` if
  the client's `protocol_version` doesn't match the server's, before any
  auth or envelope is processed. See `PROTOCOL.md`.
- **File transfer**: uploads are capped at `-max-upload-size`/
  `MAX_UPLOAD_SIZE` (default 25MB), IDs are `crypto/rand`-generated UUIDs,
  and a failed upload can't leave an orphaned file on disk. Downloads are
  only served to a participant in that file's conversation (`403`
  otherwise) and always sent as `application/octet-stream` with
  `X-Content-Type-Options: nosniff` — the client saves the file and never
  auto-opens it.
- **Auth token transport**: the WebSocket upgrade only accepts a token via
  the `Authorization: Bearer` header. There is deliberately no `?token=`
  query-string fallback — URLs leak into logs, browser history, and
  `Referer` headers in a way headers don't.
- **`govulncheck`** is clean as of the last toolchain bump (Go 1.26.6).

## What's explicitly out of scope for v0.1.0

- **End-to-end encryption.** The server can read every message body; it's
  a relay, not a zero-knowledge system. `body` is opaque bytes as far as
  the protocol is concerned, so E2E (X25519 + AES-GCM per conversation) is
  architecturally possible as a later addition, but isn't built.
- **TLS termination.** The Go binary itself speaks plain `http://`/`ws://`.
  Real deployments are expected to put a reverse proxy (Caddy, nginx) in
  front for TLS — see the deployment section of the README.
- **Multi-tenant isolation / abuse moderation.** There's no admin surface,
  no blocking/reporting, and no per-user resource quotas beyond the
  upload size cap and the auth rate limiter.

## Self-hosting notes

If you run your own instance:

- Set `ALLOWED_ORIGINS` explicitly if you ever expose the server to a
  browser-based client; leave it unset otherwise.
- Consider `REGISTRATION_CODE` if you don't want the instance open to
  arbitrary self-registration.
- Put a TLS-terminating reverse proxy in front of it — don't expose the
  plain `http://`/`ws://` port directly to the internet.

## Reporting a vulnerability

This is a personal project without a dedicated security contact or bug
bounty. If you find something, please open a GitHub issue describing the
problem, or reach out to the maintainer directly if it's sensitive enough
that you'd rather not post it publicly first.
