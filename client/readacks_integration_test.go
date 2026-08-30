package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"termtext/internal/protocol"
)

// End-to-end regression test for the read-receipt sync bug: bob receives
// a real message, his connection drops, he views it (locally) while
// disconnected — the exact scenario from the bug report — then
// reconnects for real. The pending ack_read must go out over the new
// connection without bob needing to re-open the conversation, and alice
// must see the tick flip to read.
func TestReadAckResentAfterReconnectWhenReadWhileDisconnected(t *testing.T) {
	addr := startTestServer(t)
	p := pairContacts(t, addr, "readacksalice", "readacksbob")
	defer p.aliceConn.Close()

	bob := initialModel(func(tea.Msg) {}, config{}, endpoint{host: addr}, "")
	bob.screen = screenChat
	bob.me = "readacksbob"
	// carol is a second, unrelated contact so alice isn't bob's active
	// conversation when her message arrives — otherwise
	// handleIncomingMessage's own "already viewing it" auto-ack would
	// mark the message read+acked on arrival, before this test ever gets
	// to simulate reading it while disconnected.
	bob.contacts = []contact{
		{username: "carol", conversationID: 999},
		{username: "readacksalice", conversationID: p.convID},
	}
	bob.messages = map[string][]message{}
	bob.outbox = p.bobOutbox // bob is live, over the connection pairContacts already set up

	env, _ := protocol.Encode(protocol.TypeMessage, protocol.MessagePayload{
		ConversationID: p.convID,
		ClientMsgID:    1,
		Body:           "are you there?",
	})
	p.aliceOutbox <- env
	incoming := waitFor[incomingMessageMsg](t, p.bobEvents, "bob incoming message")

	newM, _ := bob.Update(incomingMessageMsg(incoming))
	bob = newM.(model)
	if bob.messages["readacksalice"][0].readLocally {
		t.Fatal("test setup invalid: message should still be unread (carol, not alice, is the active contact)")
	}

	// bob's connection drops — matches handleDisconnected's own effect.
	p.bobConn.Close()
	bob.outbox = nil

	// bob switches to alice's conversation and reads it *while
	// disconnected* — the exact scenario from the bug report. The read
	// attempt must not be lost, but it also can't succeed yet.
	bob.active = 1
	bob.markConversationRead("readacksalice")
	if !bob.messages["readacksalice"][0].readLocally {
		t.Fatal("test setup invalid: message should be marked read locally")
	}
	if bob.messages["readacksalice"][0].readAckSent {
		t.Fatal("test setup invalid: the ack could not have actually sent with no connection")
	}

	// bob reconnects for real. handleConnected (via Update) starts the
	// real read/write pumps itself and, with the fix, resends the
	// pending ack_read as part of the same call — nothing manual needed.
	bobConn2 := mustConnect(t, addr, p.bobToken)
	defer bobConn2.Close()

	newM, _ = bob.Update(wsConnectedMsg{conn: bobConn2})
	bob = newM.(model)

	if !bob.messages["readacksalice"][0].readAckSent {
		t.Error("expected the pending ack_read to be marked sent after reconnecting")
	}

	// The real proof: alice, still on her original connection, must
	// actually receive ack_read for the message she sent.
	ack := waitFor[incomingAckReadMsg](t, p.aliceEvents, "alice ack_read after bob's reconnect")
	if ack.MessageID != incoming.ID {
		t.Errorf("ack_read MessageID = %d, want %d", ack.MessageID, incoming.ID)
	}
}
