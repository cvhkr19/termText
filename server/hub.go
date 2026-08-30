package main

import (
	"log"

	"termtext/internal/protocol"
)

// Client is the hub's per-connection handle. outbox is written only
// here — required so writePump stays gorilla/websocket's single writer.
type Client struct {
	id       int64
	username string
	outbox   chan protocol.Envelope
}

// routedEnvelope addresses an Envelope to a username for hub routing.
// delivered, if set, must be buffered (cap>=1) so the hub never blocks.
type routedEnvelope struct {
	to        string
	env       protocol.Envelope
	delivered chan bool
}

// presenceQuery asks which usernames are online. reply must be
// buffered (cap>=1) so Run never blocks.
type presenceQuery struct {
	usernames []string
	reply     chan map[string]bool
}

// Hub owns the authoritative username->Client map. Only Run reads or
// writes it, so there's no mutex — channels only.
type Hub struct {
	register   chan *Client
	unregister chan *Client
	route      chan routedEnvelope
	presence   chan presenceQuery

	clients map[string]*Client
}

func NewHub() *Hub {
	return &Hub{
		register:   make(chan *Client),
		unregister: make(chan *Client),
		route:      make(chan routedEnvelope),
		presence:   make(chan presenceQuery),
		clients:    make(map[string]*Client),
	}
}

// OnlineStatus reports which of the given usernames are currently connected.
func (h *Hub) OnlineStatus(usernames []string) map[string]bool {
	reply := make(chan map[string]bool, 1)
	h.presence <- presenceQuery{usernames: usernames, reply: reply}
	return <-reply
}

// Run is the hub's event loop. Start it in its own goroutine exactly
// once; it never returns.
func (h *Hub) Run() {
	for {
		select {
		case c := <-h.register:
			// A reconnect replaces the old connection for this username;
			// closing its outbox lets its pumps unwind on their own.
			if old, ok := h.clients[c.username]; ok {
				close(old.outbox)
			}
			h.clients[c.username] = c
			log.Printf("hub: %s connected (%d online)", c.username, len(h.clients))

		case c := <-h.unregister:
			// Only remove if this is still the current *Client — guards
			// against a stale unregister from one already replaced above.
			if cur, ok := h.clients[c.username]; ok && cur == c {
				delete(h.clients, c.username)
				close(c.outbox)
				log.Printf("hub: %s disconnected (%d online)", c.username, len(h.clients))
			}

		case r := <-h.route:
			target, ok := h.clients[r.to]
			if !ok {
				log.Printf("hub: no route to %q, dropping %s", r.to, r.env.Type)
				reportDelivery(r.delivered, false)
				continue
			}
			select {
			case target.outbox <- r.env:
				reportDelivery(r.delivered, true)
			default:
				// Outbox full: treat as dead rather than block routing
				// for everyone else or drop silently forever.
				log.Printf("hub: %s outbox full, disconnecting", r.to)
				delete(h.clients, r.to)
				close(target.outbox)
				reportDelivery(r.delivered, false)
			}

		case q := <-h.presence:
			result := make(map[string]bool, len(q.usernames))
			for _, u := range q.usernames {
				_, online := h.clients[u]
				result[u] = online
			}
			q.reply <- result
		}
	}
}

func reportDelivery(delivered chan bool, ok bool) {
	if delivered != nil {
		delivered <- ok
	}
}
