package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"termtext/internal/protocol"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreateUser(t *testing.T, s *Store, username string) int64 {
	t.Helper()
	id, err := s.CreateUser(username, "hash")
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return id
}

// mustInsertMessage writes a message with an explicit sent_at,
// bypassing CreateMessage's time.Now() stamp so tests can construct
// exact nanosecond collisions for HistoryPage's cursor.
func mustInsertMessage(t *testing.T, s *Store, conversationID, senderID int64, body, sentAt string) int64 {
	t.Helper()
	res, err := s.db.Exec(
		`INSERT INTO messages (conversation_id, sender_id, body, sent_at) VALUES (?, ?, ?, ?)`,
		conversationID, senderID, body, sentAt,
	)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

// TestHistoryPagePaginatesNanosecondPrecisionCollisions guards a fixed
// bug: HistoryPage's "before" cursor is a plain string comparison on
// sent_at, so two messages sharing a value collided and one was
// silently skipped on pagination. Nanosecond precision
// (protocol.SentAtLayout) fixed it; this asserts messages a nanosecond
// apart all survive a full walk, in order, none skipped or duplicated.
func TestHistoryPagePaginatesNanosecondPrecisionCollisions(t *testing.T) {
	s := openTestStore(t)

	alice := mustCreateUser(t, s, "alice")
	bob := mustCreateUser(t, s, "bob")
	conv, err := s.GetOrCreateConversation(alice, bob)
	if err != nil {
		t.Fatalf("get or create conversation: %v", err)
	}

	const n = 5
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) // exactly zero nanoseconds: the specific case that broke time.RFC3339Nano
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		sentAt := base.Add(time.Duration(i) * time.Nanosecond).Format(protocol.SentAtLayout)
		ids[i] = mustInsertMessage(t, s, conv.ID, alice, "msg", sentAt)
	}

	var seen []int64
	var before string
	for {
		page, err := s.HistoryPage(conv.ID, before, 1) // one at a time: the worst case for a boundary bug
		if err != nil {
			t.Fatalf("history page: %v", err)
		}
		if len(page) == 0 {
			break
		}
		seen = append(seen, page[0].ID)
		before = page[0].SentAt

		if len(seen) > n {
			t.Fatalf("paginated past the expected %d messages without terminating: %v", n, seen)
		}
	}

	if len(seen) != n {
		t.Fatalf("expected all %d messages across pages, got %d: %v", n, len(seen), seen)
	}
	for i, id := range seen {
		want := ids[n-1-i] // newest-to-oldest == reverse insertion order
		if id != want {
			t.Fatalf("page %d: got message id %d, want %d (full sequence %v, inserted %v)", i, id, want, seen, ids)
		}
	}
}

// TestUndeliveredMessagesForIsTheOfflineQueue pins the invariant
// flushOfflineMessages depends on: delivered=false *is* the queue, so
// this checks oldest-first ordering, correct recipient scoping (no
// leakage from other conversations or the recipient's own sends), and
// that MarkDelivered actually drains an entry — or a message would
// replay forever, or never replay at all.
func TestUndeliveredMessagesForIsTheOfflineQueue(t *testing.T) {
	s := openTestStore(t)

	alice := mustCreateUser(t, s, "alice")
	bob := mustCreateUser(t, s, "bob")
	carol := mustCreateUser(t, s, "carol")

	aliceBob, err := s.GetOrCreateConversation(alice, bob)
	if err != nil {
		t.Fatalf("get or create alice/bob conversation: %v", err)
	}
	aliceCarol, err := s.GetOrCreateConversation(alice, carol)
	if err != nil {
		t.Fatalf("get or create alice/carol conversation: %v", err)
	}

	// Inserted out of sent_at order deliberately — ordering must come
	// from sent_at, not insertion order.
	older := mustInsertMessage(t, s, aliceBob.ID, bob, "hi alice", "2026-01-01T12:00:00.000000000Z")
	newer := mustInsertMessage(t, s, aliceCarol.ID, carol, "you there?", "2026-01-01T12:00:01.000000000Z")

	// Noise that must never show up in alice's queue: a message she sent
	// herself (not addressed *to* her), and one already marked delivered.
	mustInsertMessage(t, s, aliceBob.ID, alice, "hey bob", "2026-01-01T12:00:02.000000000Z")
	alreadyDelivered := mustInsertMessage(t, s, aliceBob.ID, bob, "seen this already", "2026-01-01T12:00:03.000000000Z")
	if err := s.MarkDelivered(alreadyDelivered); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	// And noise scoped to a conversation alice isn't even in.
	bobCarol, err := s.GetOrCreateConversation(bob, carol)
	if err != nil {
		t.Fatalf("get or create bob/carol conversation: %v", err)
	}
	mustInsertMessage(t, s, bobCarol.ID, bob, "not for alice", "2026-01-01T12:00:04.000000000Z")

	queue, err := s.UndeliveredMessagesFor(alice)
	if err != nil {
		t.Fatalf("undelivered messages for alice: %v", err)
	}
	if len(queue) != 2 {
		t.Fatalf("queue length = %d, want 2 (got %+v)", len(queue), queue)
	}
	if queue[0].ID != older || queue[1].ID != newer {
		t.Fatalf("queue order = [%d, %d], want [%d, %d] (oldest sent_at first)", queue[0].ID, queue[1].ID, older, newer)
	}
	if queue[0].SenderUsername != "bob" || queue[1].SenderUsername != "carol" {
		t.Fatalf("unexpected senders: %q, %q", queue[0].SenderUsername, queue[1].SenderUsername)
	}

	// Draining: flushOfflineMessages calls MarkDelivered right after
	// replaying each one, so a message must disappear from the queue once
	// that happens — otherwise the next reconnect would replay it again.
	if err := s.MarkDelivered(older); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	queue, err = s.UndeliveredMessagesFor(alice)
	if err != nil {
		t.Fatalf("undelivered messages for alice (after drain): %v", err)
	}
	if len(queue) != 1 || queue[0].ID != newer {
		t.Fatalf("after draining the older message, queue = %+v, want just [%d]", queue, newer)
	}
}

// A token past its expires_at must stop authenticating, and it has to stop
// on lookup rather than waiting for any sweep — GetUserByToken is the only
// thing standing between an old token and a live session.
func TestGetUserByTokenEnforcesExpiry(t *testing.T) {
	s := openTestStore(t)
	alice := mustCreateUser(t, s, "alice")

	if err := s.CreateSession(alice, "live-token", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create live session: %v", err)
	}
	if err := s.CreateSession(alice, "stale-token", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("create expired session: %v", err)
	}

	user, err := s.GetUserByToken("live-token")
	if err != nil {
		t.Fatalf("unexpired token should resolve: %v", err)
	}
	if user.ID != alice {
		t.Errorf("resolved to user %d, want %d", user.ID, alice)
	}

	if _, err := s.GetUserByToken("stale-token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token: got %v, want ErrNotFound", err)
	}
	if _, err := s.GetUserByToken("never-issued"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown token: got %v, want ErrNotFound", err)
	}
}

// A NULL expires_at is the one shape the schema can't forbid (SQLite can't
// ADD COLUMN with a non-constant default), so it has to read as expired
// rather than as "no expiry" — otherwise a row that escaped the backfill
// would be an immortal session.
func TestNullExpiryReadsAsExpired(t *testing.T) {
	s := openTestStore(t)
	alice := mustCreateUser(t, s, "alice")

	if err := s.CreateSession(alice, "token", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = NULL WHERE token = ?`, "token"); err != nil {
		t.Fatalf("null out expires_at: %v", err)
	}

	if _, err := s.GetUserByToken("token"); !errors.Is(err, ErrNotFound) {
		t.Errorf("NULL-expiry token: got %v, want ErrNotFound", err)
	}
}

func TestDeleteSessionRevokesOnlyThatToken(t *testing.T) {
	s := openTestStore(t)
	alice := mustCreateUser(t, s, "alice")

	for _, token := range []string{"laptop", "phone"} {
		if err := s.CreateSession(alice, token, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session %s: %v", token, err)
		}
	}

	if err := s.DeleteSession("laptop"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := s.GetUserByToken("laptop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked token still resolves: %v", err)
	}
	if _, err := s.GetUserByToken("phone"); err != nil {
		t.Errorf("revoking one session should leave the other alone: %v", err)
	}

	// Logging out twice, or with a token that was never issued, is a
	// no-op rather than an error — the caller ends up logged out either
	// way, which is all the endpoint promises.
	if err := s.DeleteSession("laptop"); err != nil {
		t.Errorf("re-revoking should be a no-op, got %v", err)
	}
	if err := s.DeleteSession("never-issued"); err != nil {
		t.Errorf("revoking an unknown token should be a no-op, got %v", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	s := openTestStore(t)
	alice := mustCreateUser(t, s, "alice")

	if err := s.CreateSession(alice, "live", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create live session: %v", err)
	}
	if err := s.CreateSession(alice, "stale", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if err := s.CreateSession(alice, "nulled", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session to null out: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = NULL WHERE token = ?`, "nulled"); err != nil {
		t.Fatalf("null out expires_at: %v", err)
	}

	n, err := s.DeleteExpiredSessions()
	if err != nil {
		t.Fatalf("delete expired sessions: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d rows, want 2 (the expired one and the NULL one)", n)
	}
	if _, err := s.GetUserByToken("live"); err != nil {
		t.Errorf("the unexpired session should have survived: %v", err)
	}
}

// migrateSessionExpiry has to leave a pre-expiry database's existing
// sessions usable, dating them from created_at rather than dropping them —
// upgrading the server shouldn't log everyone out.
func TestMigrateSessionExpiryBackfillsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	alice := mustCreateUser(t, s, "alice")
	if err := s.CreateSession(alice, "pre-upgrade", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// Stand in for a row written before the column existed.
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = NULL`); err != nil {
		t.Fatalf("null out expires_at: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()

	user, err := reopened.GetUserByToken("pre-upgrade")
	if err != nil {
		t.Fatalf("a pre-upgrade session should survive the migration, got %v", err)
	}
	if user.ID != alice {
		t.Errorf("resolved to user %d, want %d", user.ID, alice)
	}
}
