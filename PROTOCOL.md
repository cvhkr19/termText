# termtext wire protocol

All communication between client and server happens over a single WebSocket
connection, opened after the client authenticates over plain HTTP (see below).
Every frame on that socket is a JSON **envelope**:

```json
{
  "type": "message",
  "payload": { ... }
}
```

- `type` selects which payload shape follows and which side is expected to
  act on it.
- `payload` is `type`-specific and documented per message below.
- The envelope is intentionally flat and untyped at the Go level
  (`Payload json.RawMessage`) so the hub can route on `type` alone without
  unmarshalling the body, and each handler decodes only the shape it needs.

This file is the source of truth for the protocol. It's written up front
(before most of these types are implemented) so the wire format doesn't
drift as the server is built in phases — implementation status is called out
per message.

## HTTP endpoints (pre-WebSocket)

Auth happens over plain HTTP, not over the socket, because it's a one-shot
request/response with no need for the hub's routing — using REST here keeps
the WebSocket protocol focused on real-time chat events only.

### `POST /register`
**Status: implemented (phase 3).**

Request:
```json
{ "username": "alice", "password": "hunter2" }
```

Response `201`:
```json
{ "token": "<opaque session token>" }
```

`409` if the username is already taken, `400` for a missing/empty username
or password. Passwords are bcrypt-hashed before storage; the plaintext is
never persisted or logged.

`400` also covers a username over 32 characters or a password over 72
bytes. The password cap is bcrypt's own hard limit — it refuses longer
input outright rather than truncating — so checking it here is what makes
that an answerable error naming the field instead of an opaque `500`.

Rate-limited per client IP, shared with `/login`: 5 requests back to back,
then one per 12 seconds. Over that gets `429` with a `Retry-After` header.
These two are the only endpoints an unauthenticated caller can reach, and
bcrypt is deliberately slow enough that an unthrottled `/login` is both a
guessing oracle and a cheap way to burn server CPU.

### `POST /login`
**Status: implemented (phase 3).**

Request:
```json
{ "username": "alice", "password": "hunter2" }
```

Response `200`:
```json
{ "token": "<opaque session token>" }
```

`401` on a bad username or password (the same message either way, so the
endpoint doesn't reveal whether a username exists). `400` and `429` as for
`/register` above.

The token is a cryptographically random 256-bit value, base64url-encoded,
recorded in the server-side `sessions` table — not a JWT, nothing is
decoded or verified client-side, the token is simply looked up on every
authenticated request.

Tokens expire 30 days after they're issued. Because a token carries no
claims of its own, expiry lives in the `sessions` row and is checked by
that same lookup on every request — there's no signed expiry a client
could be trusted to honor, and no window where an expired token still
works until some sweep catches up.

### `POST /logout`
**Status: implemented.**

Authenticated with `Authorization: Bearer <token>`. Deletes that one
session row and returns `204`. Other sessions for the same user (a second
device, say) are untouched — revocation is per token, not per user.
`401` if the token is missing or already invalid.

### `GET /ws`
**Status: implemented (phase 3).**

Upgrades to a WebSocket connection. The client must present the token
returned by `/register` or `/login` as `Authorization: Bearer <token>`.
`401` if the token is missing, unknown, expired, or malformed. The
connection is registered with the hub under the token's owner's username —
there is no separate "declare your identity" step, the identity comes
entirely from the token.

The client must also pass `?protocol_version=<n>` naming the wire-format
version it speaks (`internal/protocol.ProtocolVersion`, currently `1`).
`426 Upgrade Required` if it's missing or not a version this server
speaks, naming both versions in the error body. This is checked once,
here, at connect time — not per envelope — because the whole connection
is versioned, not individual messages: everything that crosses the wire
for the life of the connection is assumed to be in the format this one
number identifies. There's no compatibility-negotiation logic behind
it; bumping `ProtocolVersion` is a declaration that the wire format
changed in a way older builds can't parse, and every build on both ends
needs to agree on the same number to talk to each other at all.

The header is the only accepted place for the token. A `?token=<token>`
query-param fallback used to exist for tools that can't set a custom
header on a WebSocket upgrade; it was removed because a URL is exactly the
part of a request that predictably ends up in proxy and server access
logs, browser history, and outbound `Referer` headers, so a token carried
there leaks in ways a header doesn't.

If the request carries an `Origin` header it must match the server's
configured allowlist (`ALLOWED_ORIGINS`/`-allowed-origins`), else the
upgrade is refused. `Origin` is browser-set, and the risk it guards
against is a page on another site opening a socket here from a visitor's
browser with that visitor's ambient credentials attached. Native clients
— the TUI, `curl`, `wscat` — send no `Origin` at all and are unaffected;
the default allowlist is empty, meaning no browser origin is trusted until
a deployment names one.

### `POST /upload`
**Status: implemented (phase 11).**

Authenticated the same way as every other endpoint here (`Authorization:
Bearer <token>`). Body is `multipart/form-data` with two parts: a
`conversation_id` field and a `file` field. The uploader must actually be a
participant in `conversation_id` — `403` if not, `400` if it's missing or
names a conversation that doesn't exist.

The file part is streamed straight to disk (named by a generated file ID,
never by the original filename — sidesteps both collisions and path
traversal from a crafted filename) as it's read, capped at 25MB by default
(`internal/protocol.MaxUploadBytes`) — a deployment can raise or lower
this via `MAX_UPLOAD_SIZE`/`-max-upload-size`, see the server README's
self-hosting section; anything over the configured cap gets `413`. Uploaded
files are stored under `./uploads` by default, configurable via
`UPLOAD_DIR`/`-uploads`.

Response `201`:
```json
{ "file_id": "3fa2c1d4-9b7e-4a1c-8e2f-1a2b3c4d5e6f" }
```

File transfer is deliberately kept off the WebSocket entirely — this
endpoint and `GET /download/{file_id}` below are how the bytes actually
move. The WebSocket only ever carries a `file_id` reference inside an
ordinary `message` envelope (see below), never file content itself, so a
large upload can never clog the single-writer outbox that real-time
messages/typing/presence also depend on.

### `GET /download/{file_id}`
**Status: implemented (phase 11).**

Authenticated the same way. Streams the file back (never buffered whole
into memory server-side) with `Content-Disposition: attachment` and
`Content-Length` from the stored upload record.

`Content-Type` is always `application/octet-stream`, alongside
`X-Content-Type-Options: nosniff` — deliberately *not* the `mime_type` on
the record. That value is whatever the uploader claimed at upload time, so
echoing it back would let one user decide how another user's browser
interprets bytes they control (`text/html` being the obvious way to turn a
file share into stored XSS against this origin). `nosniff` then stops the
browser from second-guessing that by inspecting the content anyway. The
stored `mime_type` is still returned in message envelopes for display. `403` if the requester
isn't a participant in the conversation the file was shared in — a
`file_id` is unguessable but not treated as a secret capability on its
own, since it's carried in plain in every `message` envelope and history
page shown to both participants; the actual access check is participation,
same as every other conversation-scoped action. `404` for an unknown
`file_id`.

## WebSocket envelope types

| `type`              | Direction         | Status    | Purpose                                   |
|---------------------|--------------------|-----------|--------------------------------------------|
| `message`            | client ↔ server    | implemented | Chat message, durably persisted then relayed |
| `message_ack`         | server → client    | implemented | Confirms a sent message was persisted, with its real id |
| `typing`              | client ↔ server    | implemented | Ephemeral typing indicator                 |
| `presence`            | server → client    | implemented | Online/offline status change for a contact |
| `ack_delivered`       | server → client    | implemented | Message reached the recipient's connection |
| `ack_read`            | client → server → client | implemented | Recipient has read a message         |
| `contact_request`     | client ↔ server    | implemented | `/add <username>` request               |
| `contact_accept`      | client ↔ server    | implemented | Accept a pending contact request        |
| `contact_decline`     | client ↔ server    | implemented | Decline a pending contact request       |
| `contact_list`        | server → client    | implemented | Sent right after auth: accepted contacts + presence |
| `history_request`     | client → server    | implemented | Ask for a page of past messages            |
| `history_response`    | server → client    | implemented | A page of past messages, newest-to-oldest  |
| `error`               | server → client    | implemented | Malformed/unroutable envelope              |

### `message`

Addressed by `conversation_id`, not username — the client learns each
contact's `conversation_id` from `contact_list`/`contact_accept`, so it
never has to resolve one itself. Client → server:

```json
{
  "type": "message",
  "payload": {
    "conversation_id": 7,
    "client_msg_id": 14,
    "body": "hey"
  }
}
```

`client_msg_id` is a value the client makes up itself — a per-session
counter is enough, it just has to be unique among that client's own
in-flight sends. It exists solely so the client can correlate this
specific send with the `message_ack` the server sends back (see below);
the server stores it nowhere and never includes it in the copy relayed to
the recipient.

Server → recipient only, with sender identity, the durable message `id`,
and a server timestamp attached (`client_msg_id` is deliberately absent —
it's meaningless to anyone but the original sender, and the sender never
receives this envelope for their own message, only `message_ack`):

```json
{
  "type": "message",
  "payload": {
    "id": 42,
    "conversation_id": 7,
    "from": "alice",
    "body": "hey",
    "sent_at": "2026-08-17T20:04:11.482913Z"
  }
}
```

#### File messages
**Status: implemented (phase 11).**

`file_id`/`file_name`/`file_size` turn this same envelope into a file
message instead of a text one — set together, using the `file_id` a
`POST /upload` response returned, in place of (or, for a captioned file,
alongside) a non-empty `body`:

```json
{
  "type": "message",
  "payload": {
    "conversation_id": 7,
    "client_msg_id": 15,
    "file_id": "3fa2c1d4-9b7e-4a1c-8e2f-1a2b3c4d5e6f",
    "file_name": "report.pdf",
    "file_size": 245760
  }
}
```

The envelope type, `message_ack`, offline-queueing (`delivered=0`), and
history pagination are all unchanged — a file message flows through
exactly the same send/persist/relay path a text message does, with
`file_id`/`file_name`/`file_size` just along for the ride (and included in
`history_response`'s per-message entries too, see below). The WebSocket
never carries the file's actual bytes; those move separately over
`POST /upload`/`GET /download/{file_id}` above.

### `message_ack`

```json
{
  "type": "message_ack",
  "payload": {
    "client_msg_id": 14,
    "server_id": 42,
    "sent_at": "2026-08-17T20:04:11.482913Z"
  }
}
```

Server → the sender's own connection only, sent immediately after
`CreateMessage` durably writes the row — *before* attempting live relay,
and independent of whether the recipient is even online. This exists
because nothing else in the protocol tells the sender its own message's
real `id`: the relayed `message` envelope above only goes to the
recipient, and `ack_delivered`/`ack_read` (next section) carry only that
`id`, arriving however much later delivery/reading actually happens —
which for an offline recipient could be a long time, or never in the same
session. Without `message_ack`, a client would have no reliable way to
attach a later `ack_delivered`/`ack_read` to the correct optimistically-
rendered outgoing message once more than one conversation had a send in
flight at the same time; a client only has one message-id-shaped thing to
key off (`client_msg_id`) until this arrives and hands it the real one.
`message_ack` is always written to the sender's connection before
`ack_delivered` could possibly follow for the same message — both go
through that one connection's single-writer outbox, so ordering is
structural, not incidental.

Every message is durably written to `messages` (`delivered = 0`) *before*
being relayed — that write, not the live relay, is the source of truth.
If the recipient is connected, the server relays it immediately, flips
`delivered`, and sends the sender an `ack_delivered`. If not, nothing
further happens at send time: the row's `delivered = 0` state *is* the
offline queue. The next time the recipient connects, every undelivered
message addressed to them is replayed in order (oldest first) as this
same `message` envelope, and `delivered` is flipped then instead.

### `typing`

```json
{ "type": "typing", "payload": { "conversation_id": 7, "is_typing": true } }
```

Client → server sets `conversation_id`/`is_typing` only; the server stamps
`from` before relaying to the other participant live. Never written to
SQLite — if the recipient isn't connected, the event is simply dropped.

### `presence`

```json
{ "type": "presence", "payload": { "username": "alice", "status": "online" } }
```

Broadcast by the server to a user's accepted contacts when that user
connects or disconnects. `status` is `online` or `offline`. Not persisted
or queued for offline delivery — a missed live update self-heals the next
time each contact connects and gets a fresh `contact_list`.

### `ack_delivered` / `ack_read`

```json
{ "type": "ack_delivered", "payload": { "message_id": 123 } }
```
```json
{ "type": "ack_read", "payload": { "message_id": 123 } }
```

`ack_delivered` is server → client only, generated the moment a message is
either relayed live to a connected recipient or replayed to them on their
next connect (both flip `delivered` in the same step). `ack_read` starts
as client → server: the recipient's client sends it when the message is
scrolled into view / the conversation is focused. The server verifies the
sender is actually the message's recipient, marks it read (which implies
delivered), and relays `ack_read` on to the original sender so their
client can flip ✓✓ to the "read" color.

### `contact_request` / `contact_accept` / `contact_decline`

In both directions, the payload for all three is just `{"username": "..."}`
naming the *other* party — client → server to act on that username, server
→ client to report who the action concerns.

```json
{ "type": "contact_request", "payload": { "username": "bob" } }
```

Sent by alice's client for `/add bob`. Creates a `pending` row in
`contacts` (alice → bob). If bob is online, the server immediately pushes
him a `contact_request` envelope with `{"username": "alice"}`. If he's not,
nothing is queued separately — the pending row itself is the durable
record, and it's replayed to him as the same `contact_request` envelope
type the next time he connects (right after his `contact_list`, before any
other traffic).

If bob had *already* sent alice a pending request (both tried to add each
other before either explicitly accepted), the server auto-accepts instead
of leaving two independent pending rows: both sides immediately receive
`contact_accept` rather than one receiving `contact_request`.

```json
{ "type": "contact_accept",  "payload": { "username": "alice" } }
{ "type": "contact_decline", "payload": { "username": "alice" } }
```

Sent by bob's client to act on alice's pending request. `contact_accept`
flips the `contacts` row to `accepted`, gets-or-creates the conversation
between them, and both sides receive a live **`contact_accept`** envelope
(bob as a direct reply, alice via the hub if she's online — if not, the
row shows up as an accepted contact in her own `contact_list` next time
she connects) shaped as:

```json
{ "type": "contact_accept", "payload": { "username": "alice", "conversation_id": 7 } }
```

`conversation_id` is included so neither side needs a separate round trip
to learn it before they can send a `message` or `history_request`.
`contact_decline` deletes the row outright (plain `ContactPayload`, no
conversation involved); only alice is notified (bob never had it in his
list to begin with).

### `contact_list`

```json
{
  "type": "contact_list",
  "payload": {
    "contacts": [
      { "username": "bob", "status": "online", "conversation_id": 7 },
      { "username": "carol", "status": "offline", "conversation_id": 9 }
    ]
  }
}
```

Pushed by the server immediately after a successful WebSocket auth, so the
client can render its sidebar — including which conversation to open for
each contact — without waiting on any further round trip.

### `history_request` / `history_response`

```json
{
  "type": "history_request",
  "payload": { "conversation_id": 7, "before": "2026-08-17T20:00:00.483921700Z", "limit": 50 }
}
```

```json
{
  "type": "history_response",
  "payload": {
    "conversation_id": 7,
    "messages": [
      { "id": 42, "from": "alice", "body": "hey", "sent_at": "...", "delivered": true, "read": false },
      { "id": 43, "from": "alice", "body": "", "sent_at": "...", "delivered": true, "read": false,
        "file_id": "3fa2c1d4-9b7e-4a1c-8e2f-1a2b3c4d5e6f", "file_name": "report.pdf", "file_size": 245760 }
    ]
  }
}
```

`messages` is ordered newest-to-oldest, capped at `limit` (server clamps to
1–200, default 50). The client uses the `sent_at` of the *last* (oldest)
message in the page as the next request's `before` cursor to keep
paginating backwards as the user scrolls up; omitting `before` requests
the most recent page. `delivered`/`read` are included so the client can
rebuild each message's ✓/✓✓ state on reconnect without a separate query.
`file_id`/`file_name`/`file_size` (phase 11) are present on a file message
the same as in the live `message` envelope above, omitted entirely for an
ordinary text message.

`sent_at` (and therefore `before`) is always the fixed 9-digit-fraction
format `internal/protocol.SentAtLayout`, not the stdlib's `time.RFC3339Nano`
— both are nanosecond-precision, but `RFC3339Nano` trims trailing zero
digits when formatting (and drops the fraction entirely at exactly zero
nanoseconds), producing a variable-width string. Since the server's
`before` comparison is a plain SQL string comparison
(`sent_at < before`), a variable-width cursor can silently land on the
wrong side of a boundary message rather than failing loudly, shifting
pagination by however many messages fall in the gap — this bit the
client's own cursor construction during development. Server and client
share the one layout constant specifically so this can't drift out of
sync between them again; a client in another language should format
`before` as a literal fixed-width 9-digit fraction, not rely on whatever
its own "nanosecond RFC3339" formatter happens to produce.

### `error`

```json
{ "type": "error", "payload": { "message": "not a participant in this conversation" } }
```

Sent back to the originating connection only, for malformed envelopes or
requests the server rejects (unknown user/conversation, acting on a
conversation you're not a participant in, etc).

## End-to-end encryption (stretch goal, not yet implemented)

If implemented, this section will describe an X25519 key exchange performed
per-conversation (keys exchanged via dedicated envelope types, public keys
only ever touching the server) and AES-GCM for `message.body`, so the
server only ever relays ciphertext it cannot read. Left unimplemented until
the rest of the protocol above is working end to end.
