// File transfer client side: upload a local file, download one already
// shared. Both go over plain HTTP, never the WebSocket — see
// server/files.go.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// fileUploadedMsg is runSendFile's result. fileName/fileSize are the
// local values, not read back from the server.
type fileUploadedMsg struct {
	fileID   string
	fileName string
	fileSize int64
	err      error
}

// fileDownloadedMsg is downloadFocusedFile's result: path is where the
// file landed on disk (only meaningful if err is nil).
type fileDownloadedMsg struct {
	path string
	err  error
}

func uploadFileCmd(server endpoint, token string, conversationID int64, path string) tea.Cmd {
	return func() tea.Msg {
		fileID, fileName, size, err := uploadFile(server, token, conversationID, path)
		return fileUploadedMsg{fileID: fileID, fileName: fileName, fileSize: size, err: err}
	}
}

// uploadFile reads path into memory (bounded — already size-checked by
// runSendFile) and POSTs it as multipart/form-data.
func uploadFile(server endpoint, token string, conversationID int64, path string) (fileID, fileName string, size int64, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", 0, err
	}
	fileName = filepath.Base(path)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("conversation_id", strconv.FormatInt(conversationID, 10)); err != nil {
		return "", "", 0, err
	}
	part, err := createFilePart(mw, fileName)
	if err != nil {
		return "", "", 0, err
	}
	if _, err := part.Write(data); err != nil {
		return "", "", 0, err
	}
	if err := mw.Close(); err != nil {
		return "", "", 0, err
	}

	req, err := http.NewRequest(http.MethodPost, server.httpURL("/upload"), &body)
	if err != nil {
		return "", "", 0, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", "", 0, fmt.Errorf("%s: %s", resp.Status, readErrBody(resp.Body))
	}

	var out struct {
		FileID string `json:"file_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", 0, err
	}
	return out.FileID, fileName, int64(len(data)), nil
}

// createFilePart sets a real content-derived Content-Type — otherwise
// CreateFormFile would always send octet-stream, which the server
// stores as the file's permanent mime_type.
func createFilePart(mw *multipart.Writer, filename string) (io.Writer, error) {
	mimeType := mime.TypeByExtension(filepath.Ext(filename))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", mimeType)
	return mw.CreatePart(h)
}

// downloadFileCmd fetches and saves a file — deliberately stops there.
// Auto-opening it would run a sender-controlled filename as whatever
// program the OS picks; the user decides, not this client.
func downloadFileCmd(server endpoint, token, fileID, fileName string) tea.Cmd {
	return func() tea.Msg {
		path, err := fetchAndSaveFile(server, token, fileID, fileName)
		return fileDownloadedMsg{path: path, err: err}
	}
}

// fetchAndSaveFile does the GET /download/{file_id} and disk-write half.
func fetchAndSaveFile(server endpoint, token, fileID, fileName string) (string, error) {
	dir, err := downloadsDir()
	if err != nil {
		return "", err
	}
	return fetchAndSaveFileTo(server, token, fileID, fileName, dir)
}

// fetchAndSaveFileTo lets tests point at a t.TempDir() instead of ~/Downloads.
func fetchAndSaveFileTo(server endpoint, token, fileID, fileName, dir string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, server.httpURL("/download/"+fileID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", resp.Status, readErrBody(resp.Body))
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, sanitizeFilename(fileName))

	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	// Streamed, never buffered whole in memory.
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return dest, nil
}

func downloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Downloads", "termtext"), nil
}

// sanitizeFilename strips any path component (e.g. "../../.bashrc") so
// a download can't escape downloadsDir.
func sanitizeFilename(name string) string {
	name = filepath.Base(name)
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "download"
	}
	return name
}

func readErrBody(r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, 4096))
	return strings.TrimSpace(string(body))
}

// humanizeBytes renders a byte count for display, e.g. "240 KB".
func humanizeBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
