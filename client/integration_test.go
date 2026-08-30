package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"

	"termtext/internal/protocol"
)

// TestClientServerIntegration exercises the client's real network code
// against a real server process, proving the wire protocol and the
// client_msg_id/message_ack correlation actually work end to end.
func TestClientServerIntegration(t *testing.T) {
	addr := startTestServer(t)

	aliceToken := mustRegister(t, addr, "alice7", "pw")
	bobToken := mustRegister(t, addr, "bob7", "pw")

	aliceConn := mustConnect(t, addr, aliceToken)
	defer aliceConn.Close()
	bobConn := mustConnect(t, addr, bobToken)
	defer bobConn.Close()

	aliceEvents := make(chan tea.Msg, 64)
	bobEvents := make(chan tea.Msg, 64)
	go readLoop(aliceConn, func(msg tea.Msg) { aliceEvents <- msg })
	go readLoop(bobConn, func(msg tea.Msg) { bobEvents <- msg })

	aliceOutbox := make(chan protocol.Envelope, outboxSize)
	bobOutbox := make(chan protocol.Envelope, outboxSize)
	go writePump(aliceConn, aliceOutbox)
	go writePump(bobConn, bobOutbox)

	waitFor[incomingContactListMsg](t, aliceEvents, "alice contact_list")
	waitFor[incomingContactListMsg](t, bobEvents, "bob contact_list")
	t.Log("real WS connect + auth + initial contact_list: ok")

	env, _ := protocol.Encode(protocol.TypeContactRequest, protocol.ContactPayload{Username: "bob7"})
	aliceOutbox <- env
	req := waitFor[incomingContactRequestMsg](t, bobEvents, "bob contact_request")
	if req.Username != "alice7" {
		t.Fatalf("expected request from alice7, got %q", req.Username)
	}

	env, _ = protocol.Encode(protocol.TypeContactAccept, protocol.ContactPayload{Username: "alice7"})
	bobOutbox <- env
	bobAccept := waitFor[incomingContactAcceptMsg](t, bobEvents, "bob contact_accept (self)")
	aliceAccept := waitFor[incomingContactAcceptMsg](t, aliceEvents, "alice contact_accept")
	if bobAccept.ConversationID == 0 || bobAccept.ConversationID != aliceAccept.ConversationID {
		t.Fatalf("mismatched conversation ids: %d vs %d", bobAccept.ConversationID, aliceAccept.ConversationID)
	}
	convID := bobAccept.ConversationID
	t.Log("real contact_request -> contact_accept flow: ok")

	// Drive the real client Update() path for sending, not just the raw
	// socket — this is what actually runs when a user presses enter.
	alice := initialModel(func(tea.Msg) {}, config{}, endpoint{host: addr}, "")
	alice.screen = screenChat
	alice.me = "alice7"
	alice.outbox = aliceOutbox
	alice.contacts = []contact{{username: "bob7", conversationID: convID, online: true}}
	alice.messages = map[string][]message{}
	// handleConnected normally calls Focus(); this test skips that path
	// by setting screenChat directly, so it needs its own call.
	alice.input.Focus()

	// updateChat drops keystrokes until ready — needs an initial
	// WindowSizeMsg first, as a real launch always gets.
	newM, _ := alice.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	alice = newM.(model)

	newM, _ = alice.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello bob")})
	alice = newM.(model)
	newM, _ = alice.Update(tea.KeyMsg{Type: tea.KeyEnter})
	alice = newM.(model)

	if len(alice.messages["bob7"]) != 1 || !alice.messages["bob7"][0].mine || alice.messages["bob7"][0].body != "hello bob" {
		t.Fatalf("expected optimistic local append after Update(enter), got %+v", alice.messages["bob7"])
	}
	if alice.messages["bob7"][0].id != 0 {
		t.Fatalf("expected the optimistic message to have no server id yet, got %d", alice.messages["bob7"][0].id)
	}
	sentClientMsgID := alice.messages["bob7"][0].clientMsgID
	if sentClientMsgID == 0 {
		t.Fatal("expected handleSubmit to assign a non-zero client_msg_id")
	}

	incoming := waitFor[incomingMessageMsg](t, bobEvents, "bob incoming message")
	if incoming.From != "alice7" || incoming.Body != "hello bob" || incoming.ID == 0 {
		t.Fatalf("unexpected incoming message over the real socket: %+v", incoming)
	}
	t.Log("real message send through model.Update(enter) -> real socket -> bob: ok")

	// Feed that real server-relayed envelope through bob's own
	// model.Update, confirming the full receive path end to end.
	bob := initialModel(func(tea.Msg) {}, config{}, endpoint{host: addr}, "")
	bob.screen = screenChat
	bob.me = "bob7"
	bob.contacts = []contact{{username: "alice7", conversationID: convID}}
	bob.messages = map[string][]message{}
	newBobM, _ := bob.Update(incomingMessageMsg(incoming))
	bob = newBobM.(model)
	if len(bob.messages["alice7"]) != 1 || bob.messages["alice7"][0].body != "hello bob" {
		t.Fatalf("expected bob's model to render the incoming message, got %+v", bob.messages["alice7"])
	}
	t.Log("real incoming message applied through model.Update: ok")

	// message_ack is what lets alice's model learn the real server id
	// for its own message, independent of ack_delivered's timing.
	ack := waitFor[incomingMessageAckMsg](t, aliceEvents, "alice message_ack")
	if ack.ServerID == 0 {
		t.Fatalf("expected a non-zero server id in message_ack, got %+v", ack)
	}
	if ack.ClientMsgID != sentClientMsgID {
		t.Fatalf("message_ack client_msg_id = %d, want %d (the one handleSubmit generated)", ack.ClientMsgID, sentClientMsgID)
	}

	newAliceM, _ := alice.Update(incomingMessageAckMsg(ack))
	alice = newAliceM.(model)
	if alice.messages["bob7"][0].id != ack.ServerID {
		t.Fatalf("expected alice's local message to learn server id %d after message_ack, got %d", ack.ServerID, alice.messages["bob7"][0].id)
	}
	t.Log("real message_ack correlates client_msg_id -> server id: ok")

	// markTick can only find this message now that it has a real id.
	delivered := waitFor[incomingAckDeliveredMsg](t, aliceEvents, "alice ack_delivered")
	newAliceM, _ = alice.Update(incomingAckDeliveredMsg(delivered))
	alice = newAliceM.(model)
	if alice.messages["bob7"][0].tick != tickDelivered {
		t.Fatalf("expected tick=delivered after ack_delivered, got %v", alice.messages["bob7"][0].tick)
	}
	t.Log("real ack_delivered updates the correct message's tick via the learned server id: ok")

	bobConn.Close()
	waitFor[wsDisconnectedMsg](t, bobEvents, "bob wsDisconnectedMsg after conn.Close()")
	t.Log("disconnect detection: ok")
}

// TestHistoryOfflineMergeDedupsRegardlessOfArrivalOrder is a regression
// test: history_request and flushOfflineMessages can complete in either
// order over a real connection, and appendMessage must dedup against
// mergeInitialHistory regardless of which arrives first. Rather than
// race the two over the network, this fetches one real history_response
// and two real offline-flushed messages, then feeds them through
// model.Update in both orders deterministically.
func TestHistoryOfflineMergeDedupsRegardlessOfArrivalOrder(t *testing.T) {
	addr := startTestServer(t)

	aliceToken := mustRegister(t, addr, "alice11", "pw")
	bobToken := mustRegister(t, addr, "bob11", "pw")
	aliceConn := mustConnect(t, addr, aliceToken)
	defer aliceConn.Close()

	aliceEvents := make(chan tea.Msg, 64)
	go readLoop(aliceConn, func(msg tea.Msg) { aliceEvents <- msg })
	aliceOutbox := make(chan protocol.Envelope, 64)
	go writePump(aliceConn, aliceOutbox)
	waitFor[incomingContactListMsg](t, aliceEvents, "alice contact_list")

	// bob goes offline right after accepting, so alice's messages land
	// in the durable offline queue instead of relaying live.
	bobConn := mustConnect(t, addr, bobToken)
	bobEvents := make(chan tea.Msg, 64)
	go readLoop(bobConn, func(msg tea.Msg) { bobEvents <- msg })
	bobOutbox := make(chan protocol.Envelope, 64)
	go writePump(bobConn, bobOutbox)
	waitFor[incomingContactListMsg](t, bobEvents, "bob contact_list")

	env, _ := protocol.Encode(protocol.TypeContactRequest, protocol.ContactPayload{Username: "bob11"})
	aliceOutbox <- env
	waitFor[incomingContactRequestMsg](t, bobEvents, "bob contact_request")
	env, _ = protocol.Encode(protocol.TypeContactAccept, protocol.ContactPayload{Username: "alice11"})
	bobOutbox <- env
	bobAccept := waitFor[incomingContactAcceptMsg](t, bobEvents, "bob contact_accept (self)")
	waitFor[incomingContactAcceptMsg](t, aliceEvents, "alice contact_accept")
	convID := bobAccept.ConversationID
	bobConn.Close()

	for i := 0; i < 2; i++ {
		env, _ := protocol.Encode(protocol.TypeMessage, protocol.MessagePayload{
			ConversationID: convID,
			ClientMsgID:    int64(i + 1),
			Body:           fmt.Sprintf("offline-%d", i),
		})
		aliceOutbox <- env
		waitFor[incomingMessageAckMsg](t, aliceEvents, "alice message_ack")
	}

	bobConn2 := mustConnect(t, addr, bobToken)
	defer bobConn2.Close()
	bobEvents2 := make(chan tea.Msg, 64)
	go readLoop(bobConn2, func(msg tea.Msg) { bobEvents2 <- msg })
	bobOutbox2 := make(chan protocol.Envelope, 64)
	go writePump(bobConn2, bobOutbox2)

	waitFor[incomingContactListMsg](t, bobEvents2, "bob contact_list on reconnect")

	newBob := func() model {
		b := initialModel(func(tea.Msg) {}, config{}, endpoint{host: addr}, "")
		b.screen = screenChat
		b.me = "bob11"
		b.outbox = bobOutbox2
		b.contacts = []contact{{username: "alice11", conversationID: convID, online: true}}
		b.messages = map[string][]message{}
		newM, _ := b.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		return newM.(model)
	}

	bobForHistoryRequest := newBob()
	bobForHistoryRequest.maybeLoadHistory()
	// Order isn't guaranteed on the wire — collect all 3 and sort by
	// type rather than risk waitFor discarding one while scanning for another.
	raw := collectN(t, bobEvents2, 3, "bob history_response + 2 offline messages")
	var resp incomingHistoryResponseMsg
	var flushedMsgs []incomingMessageMsg
	for _, m := range raw {
		switch v := m.(type) {
		case incomingHistoryResponseMsg:
			resp = v
		case incomingMessageMsg:
			flushedMsgs = append(flushedMsgs, v)
		default:
			t.Fatalf("unexpected message type in collected set: %#v", m)
		}
	}
	if flushedMsgs == nil || len(flushedMsgs) != 2 {
		t.Fatalf("expected 2 flushed live messages, got %d: %+v", len(flushedMsgs), flushedMsgs)
	}
	flushed1, flushed2 := flushedMsgs[0], flushedMsgs[1]

	checkDeduped := func(t *testing.T, b model) {
		t.Helper()
		if len(b.messages["alice11"]) != 2 {
			t.Fatalf("expected exactly 2 messages (no duplicates), got %d: %+v", len(b.messages["alice11"]), b.messages["alice11"])
		}
		if b.messages["alice11"][0].body != "offline-0" || b.messages["alice11"][1].body != "offline-1" {
			t.Fatalf("unexpected content/order: %+v", b.messages["alice11"])
		}
	}

	t.Run("history_response applied first, live flush second", func(t *testing.T) {
		b := newBob()
		newM, _ := b.Update(resp)
		b = newM.(model)
		newM, _ = b.Update(incomingMessageMsg(flushed1))
		b = newM.(model)
		newM, _ = b.Update(incomingMessageMsg(flushed2))
		b = newM.(model)
		checkDeduped(t, b)
	})

	t.Run("live flush applied first, history_response second", func(t *testing.T) {
		b := newBob()
		newM, _ := b.Update(incomingMessageMsg(flushed1))
		b = newM.(model)
		newM, _ = b.Update(incomingMessageMsg(flushed2))
		b = newM.(model)
		newM, _ = b.Update(resp)
		b = newM.(model)
		checkDeduped(t, b)
	})
}

// pairedUsers is two registered, connected, mutually-accepted users
// ready for a real send/receive exchange.
type pairedUsers struct {
	aliceToken, bobToken   string
	aliceConn, bobConn     *websocket.Conn
	aliceEvents, bobEvents chan tea.Msg
	aliceOutbox, bobOutbox chan protocol.Envelope
	convID                 int64
}

func pairContacts(t *testing.T, addr, aliceName, bobName string) *pairedUsers {
	t.Helper()
	p := &pairedUsers{}
	p.aliceToken = mustRegister(t, addr, aliceName, "pw")
	p.bobToken = mustRegister(t, addr, bobName, "pw")
	p.aliceConn = mustConnect(t, addr, p.aliceToken)
	p.bobConn = mustConnect(t, addr, p.bobToken)

	p.aliceEvents = make(chan tea.Msg, 64)
	p.bobEvents = make(chan tea.Msg, 64)
	go readLoop(p.aliceConn, func(msg tea.Msg) { p.aliceEvents <- msg })
	go readLoop(p.bobConn, func(msg tea.Msg) { p.bobEvents <- msg })

	p.aliceOutbox = make(chan protocol.Envelope, outboxSize)
	p.bobOutbox = make(chan protocol.Envelope, outboxSize)
	go writePump(p.aliceConn, p.aliceOutbox)
	go writePump(p.bobConn, p.bobOutbox)

	waitFor[incomingContactListMsg](t, p.aliceEvents, aliceName+" contact_list")
	waitFor[incomingContactListMsg](t, p.bobEvents, bobName+" contact_list")

	env, _ := protocol.Encode(protocol.TypeContactRequest, protocol.ContactPayload{Username: bobName})
	p.aliceOutbox <- env
	waitFor[incomingContactRequestMsg](t, p.bobEvents, bobName+" contact_request")

	env, _ = protocol.Encode(protocol.TypeContactAccept, protocol.ContactPayload{Username: aliceName})
	p.bobOutbox <- env
	bobAccept := waitFor[incomingContactAcceptMsg](t, p.bobEvents, bobName+" contact_accept (self)")
	waitFor[incomingContactAcceptMsg](t, p.aliceEvents, aliceName+" contact_accept")
	p.convID = bobAccept.ConversationID

	return p
}

// TestFileTransferSendReceiveDownload uploads a text and a binary file,
// confirming the relayed envelope, the client's rendering, and a
// byte-for-byte download match — binary content specifically, since
// multipart/form-data handling is a common place for encoding bugs.
func TestFileTransferSendReceiveDownload(t *testing.T) {
	addr := startTestServer(t)
	p := pairContacts(t, addr, "filealice", "filebob")
	defer p.aliceConn.Close()
	defer p.bobConn.Close()

	dir := t.TempDir()
	textPath := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(textPath, []byte("hello from alice, this is a text file"), 0o644); err != nil {
		t.Fatalf("write text file: %v", err)
	}
	imgPath := filepath.Join(dir, "photo.png")
	// Real PNG header + arbitrary bytes (including a null byte) — not
	// a decodable image, just realistic binary content.
	imgData := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xde, 0xad, 0xbe, 0xef, 0x01, 0x02, 0x03}
	if err := os.WriteFile(imgPath, imgData, 0o644); err != nil {
		t.Fatalf("write image file: %v", err)
	}

	downloadDir := t.TempDir()

	for _, localPath := range []string{textPath, imgPath} {
		wantData, err := os.ReadFile(localPath)
		if err != nil {
			t.Fatalf("read local file %s: %v", localPath, err)
		}

		fileID, fileName, size, err := uploadFile(endpoint{host: addr}, p.aliceToken, p.convID, localPath)
		if err != nil {
			t.Fatalf("upload %s: %v", localPath, err)
		}
		if fileID == "" {
			t.Fatalf("upload %s: got empty file_id", localPath)
		}
		if size != int64(len(wantData)) {
			t.Fatalf("upload %s: size = %d, want %d", localPath, size, len(wantData))
		}

		env, _ := protocol.Encode(protocol.TypeMessage, protocol.MessagePayload{
			ConversationID: p.convID,
			ClientMsgID:    1,
			FileID:         fileID,
			FileName:       fileName,
			FileSize:       size,
		})
		p.aliceOutbox <- env
		waitFor[incomingMessageAckMsg](t, p.aliceEvents, "alice message_ack for "+localPath)

		incoming := waitFor[incomingMessageMsg](t, p.bobEvents, "bob incoming file message for "+localPath)
		if incoming.FileID != fileID || incoming.FileName != fileName || incoming.FileSize != size {
			t.Fatalf("incoming file message mismatch for %s: got %+v", localPath, incoming)
		}
		if incoming.Body != "" {
			t.Fatalf("expected empty body for an uncaptioned file message, got %q", incoming.Body)
		}

		// Confirm the real model renders it, not just the wire payload.
		bob := initialModel(func(tea.Msg) {}, config{}, endpoint{host: addr}, "")
		bob.screen = screenChat
		bob.me = "filebob"
		bob.contacts = []contact{{username: "filealice", conversationID: p.convID}}
		bob.messages = map[string][]message{}
		newM, _ := bob.Update(incomingMessageMsg(incoming))
		bob = newM.(model)
		got := bob.messages["filealice"]
		if len(got) != 1 || !got[0].isFile() || got[0].fileName != fileName || got[0].fileSize != size {
			t.Fatalf("bob's model didn't render the file message correctly for %s: %+v", localPath, got)
		}

		// Same fetchAndSaveFileTo the real "o" keybinding uses, pointed at
		// a test dir instead of ~/Downloads.
		savedPath, err := fetchAndSaveFileTo(endpoint{host: addr}, p.bobToken, incoming.FileID, incoming.FileName, downloadDir)
		if err != nil {
			t.Fatalf("download %s: %v", localPath, err)
		}
		gotData, err := os.ReadFile(savedPath)
		if err != nil {
			t.Fatalf("read downloaded file: %v", err)
		}
		if !bytes.Equal(gotData, wantData) {
			t.Fatalf("downloaded content for %s doesn't match original (got %d bytes, want %d)", localPath, len(gotData), len(wantData))
		}
	}
}

// TestFileMessageQueuesOfflineAndFlushesOnReconnect confirms a file
// message follows the same offline-queue path a text message does.
func TestFileMessageQueuesOfflineAndFlushesOnReconnect(t *testing.T) {
	addr := startTestServer(t)
	p := pairContacts(t, addr, "fileoffalice", "fileoffbob")
	defer p.aliceConn.Close()

	p.bobConn.Close() // bob goes offline right after accepting

	dir := t.TempDir()
	path := filepath.Join(dir, "offline-notes.txt")
	if err := os.WriteFile(path, []byte("waiting for bob to come back"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	fileID, fileName, size, err := uploadFile(endpoint{host: addr}, p.aliceToken, p.convID, path)
	if err != nil {
		t.Fatalf("upload while bob offline: %v", err)
	}

	env, _ := protocol.Encode(protocol.TypeMessage, protocol.MessagePayload{
		ConversationID: p.convID,
		ClientMsgID:    1,
		FileID:         fileID,
		FileName:       fileName,
		FileSize:       size,
	})
	p.aliceOutbox <- env
	waitFor[incomingMessageAckMsg](t, p.aliceEvents, "alice message_ack while bob offline")

	bobConn2 := mustConnect(t, addr, p.bobToken)
	defer bobConn2.Close()
	bobEvents2 := make(chan tea.Msg, 64)
	go readLoop(bobConn2, func(msg tea.Msg) { bobEvents2 <- msg })

	waitFor[incomingContactListMsg](t, bobEvents2, "bob contact_list on reconnect")
	flushed := waitFor[incomingMessageMsg](t, bobEvents2, "bob flushed file message on reconnect")
	if flushed.FileID != fileID || flushed.FileName != fileName || flushed.FileSize != size {
		t.Fatalf("flushed offline file message mismatch: got %+v", flushed)
	}

	// Offline flush also acks delivery to the sender, same as a text message.
	waitFor[incomingAckDeliveredMsg](t, p.aliceEvents, "alice ack_delivered after bob's reconnect flush")
}

// TestUploadOverSizeCapRejected confirms an over-cap upload gets a
// clean 413, not a silent truncation.
func TestUploadOverSizeCapRejected(t *testing.T) {
	addr := startTestServer(t)
	p := pairContacts(t, addr, "sizealice", "sizebob")
	defer p.aliceConn.Close()
	defer p.bobConn.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "toobig.bin")
	if err := os.WriteFile(path, make([]byte, protocol.MaxUploadBytes+1), 0o644); err != nil {
		t.Fatalf("write oversized file: %v", err)
	}

	_, _, _, err := uploadFile(endpoint{host: addr}, p.aliceToken, p.convID, path)
	if err == nil {
		t.Fatal("expected an upload over the size cap to fail, got nil error")
	}
	if !strings.Contains(err.Error(), "413") {
		t.Fatalf("expected a 413 (Request Entity Too Large) error, got: %v", err)
	}
}

// TestDownloadRejectedForNonParticipant confirms a file_id alone isn't
// enough to download — the requester must be a real participant.
func TestDownloadRejectedForNonParticipant(t *testing.T) {
	addr := startTestServer(t)
	p := pairContacts(t, addr, "dlalice", "dlbob")
	defer p.aliceConn.Close()
	defer p.bobConn.Close()

	eveToken := mustRegister(t, addr, "dleve", "pw") // not a participant in alice/bob's conversation

	dir := t.TempDir()
	path := filepath.Join(dir, "private.txt")
	if err := os.WriteFile(path, []byte("just between alice and bob"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	fileID, _, _, err := uploadFile(endpoint{host: addr}, p.aliceToken, p.convID, path)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}

	if _, err := fetchAndSaveFileTo(endpoint{host: addr}, eveToken, fileID, "private.txt", t.TempDir()); err == nil {
		t.Fatal("expected download by a non-participant to be rejected, got nil error")
	} else if !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected a 403 (Forbidden) error, got: %v", err)
	}
}

func startTestServer(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	binPath := filepath.Join(t.TempDir(), "termtext-server-test.exe")
	build := exec.Command("go", "build", "-o", binPath, "../server")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test server: %v\n%s", err, out)
	}

	dbPath := filepath.Join(t.TempDir(), "test.db")
	uploadsPath := filepath.Join(t.TempDir(), "uploads")
	cmd := exec.Command(binPath, "-addr", addr, "-db", dbPath, "-uploads", uploadsPath)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("test server never started listening")
	return ""
}

func mustRegister(t *testing.T, addr, username, password string) string {
	t.Helper()
	token, err := httpAuth(endpoint{host: addr}, "/register", username, password, "")
	if err != nil {
		t.Fatalf("register %s: %v", username, err)
	}
	return token
}

func mustConnect(t *testing.T, addr, token string) *websocket.Conn {
	t.Helper()
	msg := wsConnect(endpoint{host: addr}, token)()
	connected, ok := msg.(wsConnectedMsg)
	if !ok {
		t.Fatalf("expected wsConnectedMsg, got %#v", msg)
	}
	return connected.conn
}

// waitFor blocks for a message of type T, discarding other types while
// scanning past them (unsafe if T isn't the only thing awaited from ch —
// use collectN for that). Fails after 3s.
func waitFor[T any](t *testing.T, ch chan tea.Msg, label string) T {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg := <-ch:
			if v, ok := msg.(T); ok {
				return v
			}
		case <-deadline:
			t.Fatalf("%s: timed out waiting", label)
			var zero T
			return zero
		}
	}
}

// collectN reads exactly n messages in arrival order, discarding
// nothing — for gathering a mixed batch whose order isn't guaranteed.
func collectN(t *testing.T, ch chan tea.Msg, n int, label string) []tea.Msg {
	t.Helper()
	got := make([]tea.Msg, 0, n)
	deadline := time.After(3 * time.Second)
	for len(got) < n {
		select {
		case msg := <-ch:
			got = append(got, msg)
		case <-deadline:
			t.Fatalf("%s: timed out after collecting %d/%d", label, len(got), n)
		}
	}
	return got
}
