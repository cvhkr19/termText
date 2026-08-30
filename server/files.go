// File transfer stays off the WebSocket: large files go over plain
// HTTP so they can't stall the single-writer outbox. See PROTOCOL.md.
package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"termtext/server/store"
)

// authenticateHTTP resolves the bearer token to its user, or writes a
// 401 and returns ok=false.
func authenticateHTTP(st *store.Store, w http.ResponseWriter, r *http.Request) (store.User, bool) {
	token := bearerToken(r)
	if token == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return store.User{}, false
	}
	user, err := st.GetUserByToken(token)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("lookup token: %v", err)
		}
		http.Error(w, "invalid or expired token", http.StatusUnauthorized)
		return store.User{}, false
	}
	return user, true
}

// newFileID returns a random v4 UUID, hand-rolled to avoid a UUID dep.
func newFileID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// uploadHandler streams a multipart file to disk under a generated
// file_id (never the original filename — avoids collisions and path
// traversal), capped at maxUploadBytes. See PROTOCOL.md.
func uploadHandler(st *store.Store, uploadsDir string, maxUploadBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := authenticateHTTP(st, w, r)
		if !ok {
			return
		}

		// +1MB slack covers the form fields; the file itself is checked
		// exactly below.
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))

		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, "expected multipart/form-data", http.StatusBadRequest)
			return
		}

		var conversationID int64
		var fileID, originalFilename, mimeType, storagePath string
		var size int64
		var fileWritten bool

		// Every failure below must not leave the streamed file orphaned;
		// deferred cleanup unless committed is set at the end.
		committed := false
		defer func() {
			if storagePath != "" && !committed {
				os.Remove(storagePath)
			}
		}()

		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "malformed multipart body", http.StatusBadRequest)
				return
			}

			switch part.FormName() {
			case "conversation_id":
				buf, _ := io.ReadAll(io.LimitReader(part, 32))
				conversationID, err = strconv.ParseInt(strings.TrimSpace(string(buf)), 10, 64)
				if err != nil {
					http.Error(w, "invalid conversation_id", http.StatusBadRequest)
					return
				}

			case "file":
				if part.FileName() == "" {
					continue
				}
				fileID, err = newFileID()
				if err != nil {
					log.Printf("generate file id: %v", err)
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				storagePath = filepath.Join(uploadsDir, fileID)
				originalFilename = filepath.Base(part.FileName())
				mimeType = part.Header.Get("Content-Type")
				if mimeType == "" {
					mimeType = "application/octet-stream"
				}

				dst, err := os.Create(storagePath)
				if err != nil {
					log.Printf("create upload file: %v", err)
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				// Read one byte past the limit so "exactly at cap" and
				// "over it" are distinguishable below.
				n, copyErr := io.Copy(dst, io.LimitReader(part, maxUploadBytes+1))
				dst.Close()
				if copyErr != nil {
					log.Printf("write upload file: %v", copyErr)
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				if n > maxUploadBytes {
					http.Error(w, fmt.Sprintf("file exceeds the %s limit", humanizeBytes(maxUploadBytes)), http.StatusRequestEntityTooLarge)
					return
				}
				size = n
				fileWritten = true
			}
		}

		if !fileWritten {
			http.Error(w, "missing file part", http.StatusBadRequest)
			return
		}
		if conversationID == 0 {
			http.Error(w, "missing conversation_id", http.StatusBadRequest)
			return
		}

		conv, err := st.GetConversation(conversationID)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such conversation", http.StatusBadRequest)
			return
		}
		if err != nil {
			log.Printf("get conversation %d: %v", conversationID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, ok := conv.OtherParticipant(user.ID); !ok {
			http.Error(w, "not a participant in this conversation", http.StatusForbidden)
			return
		}

		if err := st.CreateFile(fileID, user.ID, originalFilename, size, mimeType, storagePath, conversationID); err != nil {
			log.Printf("create file record: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Row committed — stop the deferred cleanup from deleting it.
		committed = true

		writeJSON(w, http.StatusCreated, struct {
			FileID string `json:"file_id"`
		}{FileID: fileID})
	}
}

// downloadHandler streams a file back, requiring the requester to be a
// participant in the conversation it was shared in.
func downloadHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := authenticateHTTP(st, w, r)
		if !ok {
			return
		}

		fileID := r.PathValue("file_id")
		if fileID == "" {
			http.Error(w, "missing file id", http.StatusBadRequest)
			return
		}

		file, err := st.GetFile(fileID)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "no such file", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("get file %s: %v", fileID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		conv, err := st.GetConversation(file.ConversationID)
		if err != nil {
			log.Printf("get conversation %d: %v", file.ConversationID, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if _, ok := conv.OtherParticipant(user.ID); !ok {
			http.Error(w, "not a participant in this conversation", http.StatusForbidden)
			return
		}

		f, err := os.Open(file.StoragePath)
		if err != nil {
			log.Printf("open stored file %s: %v", file.StoragePath, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer f.Close()

		// Force octet-stream + nosniff regardless of the stored mime_type:
		// that value is uploader-controlled, and honoring it (e.g.
		// text/html) would let one user's upload run as this origin.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.OriginalFilename}))
		w.Header().Set("Content-Length", strconv.FormatInt(file.Size, 10))
		// Streamed, never buffered whole in memory.
		if _, err := io.Copy(w, f); err != nil {
			log.Printf("stream file %s to %s: %v", fileID, user.Username, err)
		}
	}
}

// humanizeBytes renders a byte count as "25.0 MB", not a raw integer.
func humanizeBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// parseSize parses a human byte size ("25MB", "500KB", "26214400",
// "1.5GB") into bytes. Units are base-1024, case-insensitive.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}

	upper := strings.ToUpper(s)
	multiplier := int64(1)
	numPart := upper
	switch {
	case strings.HasSuffix(upper, "GB"):
		multiplier = 1 << 30
		numPart = strings.TrimSuffix(upper, "GB")
	case strings.HasSuffix(upper, "MB"):
		multiplier = 1 << 20
		numPart = strings.TrimSuffix(upper, "MB")
	case strings.HasSuffix(upper, "KB"):
		multiplier = 1 << 10
		numPart = strings.TrimSuffix(upper, "KB")
	case strings.HasSuffix(upper, "G"):
		multiplier = 1 << 30
		numPart = strings.TrimSuffix(upper, "G")
	case strings.HasSuffix(upper, "M"):
		multiplier = 1 << 20
		numPart = strings.TrimSuffix(upper, "M")
	case strings.HasSuffix(upper, "K"):
		multiplier = 1 << 10
		numPart = strings.TrimSuffix(upper, "K")
	case strings.HasSuffix(upper, "B"):
		numPart = strings.TrimSuffix(upper, "B")
	}

	n, err := strconv.ParseFloat(strings.TrimSpace(numPart), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("size must be positive, got %q", s)
	}
	return int64(n * float64(multiplier)), nil
}
