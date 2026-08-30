// Package store is the SQLite persistence layer — the only package
// that writes raw SQL.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"termtext/internal/protocol"
)

var (
	ErrUsernameTaken = errors.New("username already taken")
	ErrNotFound      = errors.New("not found")
	ErrSelfContact   = errors.New("cannot add yourself as a contact")
)

type User struct {
	ID           int64
	Username     string
	PasswordHash string
}

type Store struct {
	db *sql.DB
}

// schema is applied with CREATE TABLE IF NOT EXISTS on every Open.
const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Opaque bearer tokens from /register and /login — not a JWT, just a
-- random key looked up per request; DELETE revokes it (see /logout).
--
-- expires_at is nullable only because SQLite can't ADD COLUMN with a
-- non-constant default; NULL compares as expired, never as unlimited.
CREATE TABLE IF NOT EXISTS sessions (
	token      TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id),
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	expires_at TEXT
);

-- One row per contact relationship. user_a is the original requester;
-- direction stops mattering once status is 'accepted' (symmetric).
CREATE TABLE IF NOT EXISTS contacts (
	user_a     INTEGER NOT NULL REFERENCES users(id),
	user_b     INTEGER NOT NULL REFERENCES users(id),
	status     TEXT NOT NULL CHECK (status IN ('pending', 'accepted')),
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (user_a, user_b)
);

-- One row per user pair. user_a/user_b are normalized (see
-- orderedPair) so a UNIQUE constraint prevents duplicate conversations.
CREATE TABLE IF NOT EXISTS conversations (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	user_a     INTEGER NOT NULL REFERENCES users(id),
	user_b     INTEGER NOT NULL REFERENCES users(id),
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE (user_a, user_b)
);

-- sent_at is protocol.SentAtLayout — fixed-width, not SQLite's
-- CURRENT_TIMESTAMP or time.RFC3339Nano — so plain string comparison
-- sorts correctly for the history_request "before" cursor.
--
-- file_id/file_name/file_size are NULL for a text message.
CREATE TABLE IF NOT EXISTS messages (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id INTEGER NOT NULL REFERENCES conversations(id),
	sender_id       INTEGER NOT NULL REFERENCES users(id),
	body            TEXT NOT NULL,
	sent_at         TEXT NOT NULL,
	delivered       INTEGER NOT NULL DEFAULT 0,
	read            INTEGER NOT NULL DEFAULT 0,
	file_id         TEXT,
	file_name       TEXT,
	file_size       INTEGER
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages (conversation_id, sent_at);

-- One row per upload (phase 11). storage_path is named by file_id, not
-- original_filename — avoids collisions and path traversal.
CREATE TABLE IF NOT EXISTS files (
	file_id           TEXT PRIMARY KEY,
	uploader_id       INTEGER NOT NULL REFERENCES users(id),
	original_filename TEXT NOT NULL,
	size              INTEGER NOT NULL,
	mime_type         TEXT NOT NULL,
	storage_path      TEXT NOT NULL,
	conversation_id   INTEGER NOT NULL REFERENCES conversations(id),
	created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite allows one writer at a time; cap the pool at 1 connection
	// to avoid SQLITE_BUSY from a second writer.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	if err := migrateFileColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	if err := migrateSessionExpiry(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// migrateFileColumns adds messages' file_id/file_name/file_size to a
// pre-phase-11 database.
func migrateFileColumns(db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE messages ADD COLUMN file_id TEXT`,
		`ALTER TABLE messages ADD COLUMN file_name TEXT`,
		`ALTER TABLE messages ADD COLUMN file_size INTEGER`,
	} {
		if err := addColumnIfMissing(db, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateSessionExpiry adds expires_at, backfilling existing rows to
// created_at+SessionTTL rather than NULL (= expired) so upgrading
// doesn't log everyone out.
func migrateSessionExpiry(db *sql.DB) error {
	if err := addColumnIfMissing(db, `ALTER TABLE sessions ADD COLUMN expires_at TEXT`); err != nil {
		return err
	}
	// datetime() matches sessionTimeLayout exactly, so rows compare directly.
	_, err := db.Exec(
		`UPDATE sessions SET expires_at = datetime(created_at, ?) WHERE expires_at IS NULL`,
		fmt.Sprintf("+%d seconds", int64(SessionTTL/time.Second)),
	)
	return err
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN, treating "duplicate
// column name" as success (SQLite has no ADD COLUMN IF NOT EXISTS).
func addColumnIfMissing(db *sql.DB, stmt string) error {
	if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// CreateUser inserts a user with an already-hashed password, returning
// ErrUsernameTaken if not unique.
func (s *Store) CreateUser(username, passwordHash string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash) VALUES (?, ?)`, username, passwordHash)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrUsernameTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetUserByUsername(username string) (User, error) {
	var u User
	err := s.db.QueryRow(`SELECT id, username, password_hash FROM users WHERE username = ?`, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// SessionTTL is how long an issued token stays valid — enforced by
// GetUserByToken on every lookup, since tokens carry no expiry claim.
const SessionTTL = 30 * 24 * time.Hour

// sessionTimeLayout matches SQLite's own datetime() rendering.
const sessionTimeLayout = "2006-01-02 15:04:05"

// CreateSession stores a new token for userID, valid until expiresAt.
func (s *Store) CreateSession(userID int64, token string, expiresAt time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`,
		token, userID, expiresAt.UTC().Format(sessionTimeLayout),
	)
	return err
}

// DeleteSession revokes token. Revoking a token that's already gone is
// not an error.
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// DeleteExpiredSessions is housekeeping only — expiry is already
// enforced on lookup regardless of this ever running.
func (s *Store) DeleteExpiredSessions() (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM sessions WHERE expires_at IS NULL OR expires_at <= ?`,
		time.Now().UTC().Format(sessionTimeLayout),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetUserByToken resolves a token to its user. Expired or NULL-expiry
// rows are excluded by the query itself — checked on every use, not by
// a lagging sweep.
func (s *Store) GetUserByToken(token string) (User, error) {
	var u User
	err := s.db.QueryRow(`
		SELECT users.id, users.username, users.password_hash
		FROM sessions
		JOIN users ON users.id = sessions.user_id
		WHERE sessions.token = ? AND sessions.expires_at > ?`,
		token, time.Now().UTC().Format(sessionTimeLayout)).
		Scan(&u.ID, &u.Username, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// RequestContact records a request, auto-accepting a mutual pending
// pair. Returns "pending" or "accepted".
func (s *Store) RequestContact(requesterID, targetID int64) (string, error) {
	if requesterID == targetID {
		return "", ErrSelfContact
	}

	// Did we already request (or already have) this contact?
	if status, err := s.contactStatus(requesterID, targetID); err == nil {
		return status, nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}

	// Did target already request us?
	if status, err := s.contactStatus(targetID, requesterID); err == nil {
		if status == "accepted" {
			return "accepted", nil
		}
		if _, err := s.db.Exec(
			`UPDATE contacts SET status = 'accepted' WHERE user_a = ? AND user_b = ?`,
			targetID, requesterID,
		); err != nil {
			return "", err
		}
		return "accepted", nil
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}

	if _, err := s.db.Exec(
		`INSERT INTO contacts (user_a, user_b, status) VALUES (?, ?, 'pending')`,
		requesterID, targetID,
	); err != nil {
		return "", err
	}
	return "pending", nil
}

func (s *Store) contactStatus(a, b int64) (string, error) {
	var status string
	err := s.db.QueryRow(`SELECT status FROM contacts WHERE user_a = ? AND user_b = ?`, a, b).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}

// AcceptContact accepts a pending request that otherID previously sent to
// userID. ErrNotFound if there's no such pending request.
func (s *Store) AcceptContact(userID, otherID int64) error {
	res, err := s.db.Exec(
		`UPDATE contacts SET status = 'accepted' WHERE user_a = ? AND user_b = ? AND status = 'pending'`,
		otherID, userID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeclineContact removes a pending request that otherID previously sent to
// userID. ErrNotFound if there's no such pending request.
func (s *Store) DeclineContact(userID, otherID int64) error {
	res, err := s.db.Exec(
		`DELETE FROM contacts WHERE user_a = ? AND user_b = ? AND status = 'pending'`,
		otherID, userID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AcceptedContacts returns the users userID has an accepted contact
// relationship with, regardless of who originally sent the request.
func (s *Store) AcceptedContacts(userID int64) ([]User, error) {
	rows, err := s.db.Query(`
		SELECT u.id, u.username
		FROM contacts c
		JOIN users u ON u.id = CASE WHEN c.user_a = ? THEN c.user_b ELSE c.user_a END
		WHERE (c.user_a = ? OR c.user_b = ?) AND c.status = 'accepted'`,
		userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsers(rows)
}

// PendingIncoming returns users with an outstanding request to userID —
// pushed as contact_request right after connect.
func (s *Store) PendingIncoming(userID int64) ([]User, error) {
	rows, err := s.db.Query(`
		SELECT u.id, u.username
		FROM contacts c
		JOIN users u ON u.id = c.user_a
		WHERE c.user_b = ? AND c.status = 'pending'`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUsers(rows)
}

// GetUserByID looks up a user by numeric ID (e.g. from a message's sender_id).
func (s *Store) GetUserByID(id int64) (User, error) {
	var u User
	err := s.db.QueryRow(`SELECT id, username, password_hash FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.Username, &u.PasswordHash)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Conversation is always between exactly two users (no group chat).
type Conversation struct {
	ID    int64
	UserA int64
	UserB int64
}

// OtherParticipant returns the participant that isn't userID, and
// false if userID isn't a participant at all.
func (c Conversation) OtherParticipant(userID int64) (int64, bool) {
	switch userID {
	case c.UserA:
		return c.UserB, true
	case c.UserB:
		return c.UserA, true
	default:
		return 0, false
	}
}

// GetOrCreateConversation returns the pair's conversation, creating it
// if needed. Idempotent — safe to call on every accept.
func (s *Store) GetOrCreateConversation(userA, userB int64) (Conversation, error) {
	a, b := orderedPair(userA, userB)

	conv, err := s.getConversationByPair(a, b)
	if err == nil {
		return conv, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Conversation{}, err
	}

	res, err := s.db.Exec(`INSERT INTO conversations (user_a, user_b) VALUES (?, ?)`, a, b)
	if err != nil {
		// Lost a race creating this pair concurrently — re-fetch.
		if isUniqueViolation(err) {
			return s.getConversationByPair(a, b)
		}
		return Conversation{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Conversation{}, err
	}
	return Conversation{ID: id, UserA: a, UserB: b}, nil
}

func (s *Store) getConversationByPair(a, b int64) (Conversation, error) {
	var conv Conversation
	err := s.db.QueryRow(`SELECT id, user_a, user_b FROM conversations WHERE user_a = ? AND user_b = ?`, a, b).
		Scan(&conv.ID, &conv.UserA, &conv.UserB)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	return conv, err
}

// GetConversation looks up a conversation by ID, as named in a client's
// message/typing/history_request payload.
func (s *Store) GetConversation(id int64) (Conversation, error) {
	var conv Conversation
	err := s.db.QueryRow(`SELECT id, user_a, user_b FROM conversations WHERE id = ?`, id).
		Scan(&conv.ID, &conv.UserA, &conv.UserB)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	return conv, err
}

func orderedPair(a, b int64) (int64, int64) {
	if a < b {
		return a, b
	}
	return b, a
}

// FileID's empty-string zero value means "plain text message" — matches
// the stored NULL and the wire payload's omitempty.
type Message struct {
	ID             int64
	ConversationID int64
	SenderID       int64
	Body           string
	SentAt         string
	Delivered      bool
	Read           bool
	FileID         string
	FileName       string
	FileSize       int64
}

// MessageWithSender is a Message plus the sender's username.
type MessageWithSender struct {
	Message
	SenderUsername string
}

// CreateMessage durably writes a message before any live-delivery
// attempt. sent_at uses protocol.SentAtLayout's fixed nanosecond
// precision so the history "before" cursor never ties/misorders.
func (s *Store) CreateMessage(conversationID, senderID int64, body string) (Message, error) {
	return s.insertMessage(conversationID, senderID, body, "", "", 0)
}

// CreateFileMessage is CreateMessage plus file_id/file_name/file_size,
// so a file flows through every send/offline/history path unchanged.
func (s *Store) CreateFileMessage(conversationID, senderID int64, body, fileID, fileName string, fileSize int64) (Message, error) {
	return s.insertMessage(conversationID, senderID, body, fileID, fileName, fileSize)
}

// insertMessage writes NULL (not "") into file_id/file_name/file_size
// for a text message, so "is this a file" stays a clean IS NOT NULL.
func (s *Store) insertMessage(conversationID, senderID int64, body, fileID, fileName string, fileSize int64) (Message, error) {
	sentAt := time.Now().UTC().Format(protocol.SentAtLayout)

	var res sql.Result
	var err error
	if fileID == "" {
		res, err = s.db.Exec(
			`INSERT INTO messages (conversation_id, sender_id, body, sent_at) VALUES (?, ?, ?, ?)`,
			conversationID, senderID, body, sentAt,
		)
	} else {
		res, err = s.db.Exec(
			`INSERT INTO messages (conversation_id, sender_id, body, sent_at, file_id, file_name, file_size) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			conversationID, senderID, body, sentAt, fileID, fileName, fileSize,
		)
	}
	if err != nil {
		return Message{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, err
	}
	return s.GetMessage(id)
}

func (s *Store) GetMessage(id int64) (Message, error) {
	var m Message
	var fileID, fileName sql.NullString
	var fileSize sql.NullInt64
	err := s.db.QueryRow(
		`SELECT id, conversation_id, sender_id, body, sent_at, delivered, read, file_id, file_name, file_size FROM messages WHERE id = ?`, id,
	).Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.SentAt, &m.Delivered, &m.Read, &fileID, &fileName, &fileSize)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, err
	}
	m.FileID, m.FileName, m.FileSize = fileID.String, fileName.String, fileSize.Int64
	return m, nil
}

// File is one uploaded-file record (phase 11).
type File struct {
	FileID           string
	UploaderID       int64
	OriginalFilename string
	Size             int64
	MimeType         string
	StoragePath      string
	ConversationID   int64
}

// CreateFile records an already-written upload; fileID is generated by
// the caller before the bytes are written.
func (s *Store) CreateFile(fileID string, uploaderID int64, originalFilename string, size int64, mimeType, storagePath string, conversationID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO files (file_id, uploader_id, original_filename, size, mime_type, storage_path, conversation_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		fileID, uploaderID, originalFilename, size, mimeType, storagePath, conversationID,
	)
	return err
}

// GetFile looks up an upload by its file_id, as named in a client's
// GET /download/{file_id} request or a message's file_id.
func (s *Store) GetFile(fileID string) (File, error) {
	var f File
	err := s.db.QueryRow(
		`SELECT file_id, uploader_id, original_filename, size, mime_type, storage_path, conversation_id FROM files WHERE file_id = ?`, fileID,
	).Scan(&f.FileID, &f.UploaderID, &f.OriginalFilename, &f.Size, &f.MimeType, &f.StoragePath, &f.ConversationID)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, ErrNotFound
	}
	return f, err
}

func (s *Store) MarkDelivered(id int64) error {
	_, err := s.db.Exec(`UPDATE messages SET delivered = 1 WHERE id = ?`, id)
	return err
}

// MarkRead also sets delivered, so read never outpaces delivered.
func (s *Store) MarkRead(id int64) error {
	_, err := s.db.Exec(`UPDATE messages SET read = 1, delivered = 1 WHERE id = ?`, id)
	return err
}

// UndeliveredMessagesFor returns undelivered messages for userID,
// oldest first. delivered=false *is* the offline queue.
func (s *Store) UndeliveredMessagesFor(userID int64) ([]MessageWithSender, error) {
	rows, err := s.db.Query(`
		SELECT m.id, m.conversation_id, m.sender_id, m.body, m.sent_at, m.delivered, m.read, m.file_id, m.file_name, m.file_size, u.username
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN users u ON u.id = m.sender_id
		WHERE (c.user_a = ? OR c.user_b = ?) AND m.sender_id != ? AND m.delivered = 0
		ORDER BY m.sent_at ASC, m.id ASC`,
		userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

// HistoryPage returns up to limit messages, newest first, older than
// before if set. limit is clamped server-side.
func (s *Store) HistoryPage(conversationID int64, before string, limit int) ([]MessageWithSender, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := `
		SELECT m.id, m.conversation_id, m.sender_id, m.body, m.sent_at, m.delivered, m.read, m.file_id, m.file_name, m.file_size, u.username
		FROM messages m
		JOIN users u ON u.id = m.sender_id
		WHERE m.conversation_id = ?`
	args := []any{conversationID}
	if before != "" {
		query += ` AND m.sent_at < ?`
		args = append(args, before)
	}
	query += ` ORDER BY m.sent_at DESC, m.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMessages(rows)
}

func scanMessages(rows *sql.Rows) ([]MessageWithSender, error) {
	var out []MessageWithSender
	for rows.Next() {
		var m MessageWithSender
		var fileID, fileName sql.NullString
		var fileSize sql.NullInt64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.SenderID, &m.Body, &m.SentAt, &m.Delivered, &m.Read, &fileID, &fileName, &fileSize, &m.SenderUsername); err != nil {
			return nil, err
		}
		m.FileID, m.FileName, m.FileSize = fileID.String, fileName.String, fileSize.Int64
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanUsers(rows *sql.Rows) ([]User, error) {
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func isUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
