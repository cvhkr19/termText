package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"termtext/server/store"
)

// uploadFixture is one prepared conversation plus the tokens needed to
// post to it: alice and bob are participants, eve is not.
type uploadFixture struct {
	st         *store.Store
	uploadsDir string
	convID     int64
	aliceToken string
	eveToken   string
}

func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	st := openTestStore(t)

	aliceID, err := st.CreateUser("alice", "hash")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bobID, err := st.CreateUser("bob", "hash")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	eveID, err := st.CreateUser("eve", "hash")
	if err != nil {
		t.Fatalf("create eve: %v", err)
	}

	conv, err := st.GetOrCreateConversation(aliceID, bobID)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}

	f := &uploadFixture{
		st:         st,
		uploadsDir: t.TempDir(),
		convID:     conv.ID,
		aliceToken: "alice-token",
		eveToken:   "eve-token",
	}
	for id, token := range map[int64]string{aliceID: f.aliceToken, eveID: f.eveToken} {
		if err := st.CreateSession(id, token, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}
	return f
}

// storedFiles is how many files are actually sitting in the uploads
// directory — the thing an orphaned upload would show up in.
func (f *uploadFixture) storedFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.uploadsDir)
	if err != nil {
		t.Fatalf("read uploads dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// uploadPart's field order matters: a file part sent before
// conversation_id is what exercises the orphan-cleanup path (see
// uploadHandler in server/files.go).
type uploadPart struct {
	field    string
	filename string // non-empty makes this the file part
	value    string
}

func (f *uploadFixture) post(t *testing.T, token string, parts []uploadPart) *httptest.ResponseRecorder {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range parts {
		if p.filename == "" {
			if err := mw.WriteField(p.field, p.value); err != nil {
				t.Fatalf("write field %s: %v", p.field, err)
			}
			continue
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+p.field+`"; filename="`+p.filename+`"`)
		h.Set("Content-Type", "text/plain")
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatalf("create file part: %v", err)
		}
		if _, err := w.Write([]byte(p.value)); err != nil {
			t.Fatalf("write file part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	uploadHandler(f.st, f.uploadsDir, 1<<20)(rec, req)
	return rec
}

func TestUploadSucceedsForParticipant(t *testing.T) {
	f := newUploadFixture(t)

	rec := f.post(t, f.aliceToken, []uploadPart{
		{field: "conversation_id", value: itoa(f.convID)},
		{field: "file", filename: "notes.txt", value: "hello"},
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (%s)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	var out struct {
		FileID string `json:"file_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.FileID == "" {
		t.Fatal("response carried no file_id")
	}

	stored := f.storedFiles(t)
	if len(stored) != 1 || stored[0] != out.FileID {
		t.Fatalf("uploads dir holds %v, want exactly [%s]", stored, out.FileID)
	}

	// The file must be recorded as well as written, or the download
	// endpoint has nothing to authorize against.
	rec2, err := f.st.GetFile(out.FileID)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if rec2.OriginalFilename != "notes.txt" || rec2.Size != 5 {
		t.Errorf("stored record = %+v, want notes.txt at 5 bytes", rec2)
	}
}

// Each of these rejects the upload *after* the file bytes have already
// been streamed to disk, because the file part is sent first. Every one of
// them has to leave the uploads directory empty.
func TestUploadFailuresLeaveNoOrphanedFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token func(*uploadFixture) string
		parts func(*uploadFixture) []uploadPart
		want  int
	}{
		{
			name:  "unparseable conversation_id after the file part",
			token: func(f *uploadFixture) string { return f.aliceToken },
			parts: func(f *uploadFixture) []uploadPart {
				return []uploadPart{
					{field: "file", filename: "notes.txt", value: "hello"},
					{field: "conversation_id", value: "not-a-number"},
				}
			},
			want: http.StatusBadRequest,
		},
		{
			name:  "conversation_id missing entirely",
			token: func(f *uploadFixture) string { return f.aliceToken },
			parts: func(f *uploadFixture) []uploadPart {
				return []uploadPart{{field: "file", filename: "notes.txt", value: "hello"}}
			},
			want: http.StatusBadRequest,
		},
		{
			name:  "conversation does not exist",
			token: func(f *uploadFixture) string { return f.aliceToken },
			parts: func(f *uploadFixture) []uploadPart {
				return []uploadPart{
					{field: "file", filename: "notes.txt", value: "hello"},
					{field: "conversation_id", value: "999999"},
				}
			},
			want: http.StatusBadRequest,
		},
		{
			name:  "uploader is not a participant",
			token: func(f *uploadFixture) string { return f.eveToken },
			parts: func(f *uploadFixture) []uploadPart {
				return []uploadPart{
					{field: "file", filename: "notes.txt", value: "hello"},
					{field: "conversation_id", value: itoa(f.convID)},
				}
			},
			want: http.StatusForbidden,
		},
		{
			name:  "file exceeds the size cap",
			token: func(f *uploadFixture) string { return f.aliceToken },
			parts: func(f *uploadFixture) []uploadPart {
				return []uploadPart{
					{field: "file", filename: "big.bin", value: strings.Repeat("a", (1<<20)+1)},
					{field: "conversation_id", value: itoa(f.convID)},
				}
			},
			want: http.StatusRequestEntityTooLarge,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUploadFixture(t)

			rec := f.post(t, tc.token(f), tc.parts(f))
			if rec.Code != tc.want {
				t.Errorf("got %d, want %d (%s)", rec.Code, tc.want, strings.TrimSpace(rec.Body.String()))
			}
			if stored := f.storedFiles(t); len(stored) != 0 {
				t.Errorf("rejected upload orphaned %d file(s) on disk: %v", len(stored), stored)
			}
		})
	}
}

func TestUploadRequiresAuthentication(t *testing.T) {
	f := newUploadFixture(t)

	rec := f.post(t, "not-a-real-token", []uploadPart{
		{field: "conversation_id", value: itoa(f.convID)},
		{field: "file", filename: "notes.txt", value: "hello"},
	})

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", rec.Code)
	}
	if stored := f.storedFiles(t); len(stored) != 0 {
		t.Errorf("unauthenticated upload wrote %v to disk", stored)
	}
}

// The uploader picks the mime type, so it must not be echoed back as the
// Content-Type a browser would render: text/html here would otherwise turn
// a file share into stored XSS against this origin.
func TestDownloadNeutralizesUploaderSuppliedMimeType(t *testing.T) {
	f := newUploadFixture(t)

	fileID := "test-file-id"
	storagePath := filepath.Join(f.uploadsDir, fileID)
	const content = "<script>alert(1)</script>"
	if err := os.WriteFile(storagePath, []byte(content), 0o600); err != nil {
		t.Fatalf("write stored file: %v", err)
	}

	aliceUser, err := f.st.GetUserByToken(f.aliceToken)
	if err != nil {
		t.Fatalf("resolve alice: %v", err)
	}
	if err := f.st.CreateFile(fileID, aliceUser.ID, "payload.html", int64(len(content)), "text/html", storagePath, f.convID); err != nil {
		t.Fatalf("create file record: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/download/"+fileID, nil)
	req.Header.Set("Authorization", "Bearer "+f.aliceToken)
	req.SetPathValue("file_id", fileID)
	downloadHandler(f.st)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (%s)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream — the stored text/html must not be echoed back", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment disposition", got)
	}
	if rec.Body.String() != content {
		t.Errorf("body = %q, want the stored bytes unchanged", rec.Body.String())
	}
}

func TestDownloadRefusesNonParticipant(t *testing.T) {
	f := newUploadFixture(t)

	fileID := "private-file-id"
	storagePath := filepath.Join(f.uploadsDir, fileID)
	if err := os.WriteFile(storagePath, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write stored file: %v", err)
	}
	aliceUser, err := f.st.GetUserByToken(f.aliceToken)
	if err != nil {
		t.Fatalf("resolve alice: %v", err)
	}
	if err := f.st.CreateFile(fileID, aliceUser.ID, "secret.txt", 6, "text/plain", storagePath, f.convID); err != nil {
		t.Fatalf("create file record: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/download/"+fileID, nil)
	req.Header.Set("Authorization", "Bearer "+f.eveToken)
	req.SetPathValue("file_id", fileID)
	downloadHandler(f.st)(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Error("the response leaked the file contents")
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "25MB", want: 25 << 20},
		{in: "25mb", want: 25 << 20},
		{in: "25M", want: 25 << 20},
		{in: "500KB", want: 500 << 10},
		{in: "1GB", want: 1 << 30},
		{in: "1.5GB", want: 1536 << 20},
		{in: "25 MB", want: 25 << 20},
		{in: "  25MB  ", want: 25 << 20},
		{in: "26214400", want: 26214400},
		{in: "1024B", want: 1024},
		{in: "0", want: 0},

		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "MB", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "-5MB", wantErr: true},
	} {
		got, err := parseSize(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseSize(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSize(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestHumanizeBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1 << 10, "1.0 KB"},
		{25 << 20, "25.0 MB"},
		{1 << 30, "1.0 GB"},
		{3 << 30, "3.0 GB"},
	} {
		if got := humanizeBytes(tc.in); got != tc.want {
			t.Errorf("humanizeBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewFileIDIsAUniqueV4UUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := newFileID()
		if err != nil {
			t.Fatalf("newFileID: %v", err)
		}
		if seen[id] {
			t.Fatalf("newFileID returned a duplicate after %d calls: %s", i, id)
		}
		seen[id] = true

		if len(id) != 36 {
			t.Fatalf("id %q has length %d, want 36", id, len(id))
		}
		// Version nibble and variant bits, per RFC 4122 — a file_id
		// doubles as the on-disk filename, so its shape is load-bearing.
		if id[14] != '4' {
			t.Errorf("id %q is not version 4", id)
		}
		if !strings.ContainsRune("89ab", rune(id[19])) {
			t.Errorf("id %q has the wrong variant bits", id)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
