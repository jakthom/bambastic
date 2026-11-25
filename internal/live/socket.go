package live

import (
	"bambastic/internal/access"
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Event = map[string]any
type Hub struct {
	mu          sync.Mutex
	subscribers map[chan Event]struct{}
	Auth        *access.Service
	Initial     func(context.Context, access.Principal) ([]map[string]any, error)
}

func New(auth *access.Service) *Hub { return &Hub{Auth: auth, subscribers: map[chan Event]struct{}{}} }
func (h *Hub) Publish(event Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subscribers {
		select {
		case ch <- event:
		default:
			delete(h.subscribers, ch)
			close(ch)
		}
	}
}
func number(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), true
	case json.Number:
		i, e := n.Int64()
		return i, e == nil
	}
	return 0, false
}
func allowed(event Event, p access.Principal) bool {
	if id, ok := number(event["user_id"]); ok && id != p.ID {
		return false
	}
	if id, ok := number(event["printer_id"]); ok {
		return p.Has("printers:read") && p.AllowsPrinter(id)
	}
	if d, ok := event["data"].(map[string]any); ok {
		if id, ok := number(d["printer_id"]); ok {
			return p.Has("printers:read") && p.AllowsPrinter(id)
		}
	}
	eventType, _ := event["type"].(string)
	if strings.HasPrefix(eventType, "spoolbuddy_") || eventType == "inventory_changed" {
		return p.Has("inventory:read") || p.Has("inventory:update")
	}
	return p.Admin
}
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, e := websocket.Accept(w, r, nil)
	if e != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8192)
	token := access.Credential(r)
	kind := "session"
	if t := r.URL.Query().Get("token"); t != "" {
		token = t
		kind = "ws"
	}
	identity := func() (access.Principal, bool) {
		p, e := h.Auth.Resolve(r.Context(), token, kind)
		if e != nil || !p.Has("websocket:connect") {
			conn.Close(4401, "Authentication required")
			return p, false
		}
		return p, true
	}
	p, ok := identity()
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.subscribers, ch); h.mu.Unlock() }()
	write := func(v Event) bool {
		c, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		return wsjson.Write(c, conn, v) == nil
	}
	snapshots := func(user access.Principal, printer int64) bool {
		if !user.Has("printers:read") {
			return true
		}
		all, e := h.Initial(ctx, user)
		if e != nil {
			return false
		}
		for _, v := range all {
			id, ok := number(v["printer_id"])
			if !ok {
				id, ok = number(v["id"])
			}
			if ok && (printer == 0 || printer == id) && user.AllowsPrinter(id) {
				if !write(Event{"type": "printer_status", "printer_id": id, "data": v}) {
					return false
				}
			}
		}
		return true
	}
	if !snapshots(p, 0) {
		return
	}
	messages := make(chan Event, 2)
	go func() {
		defer cancel()
		for {
			var m Event
			if wsjson.Read(ctx, conn, &m) != nil {
				return
			}
			select {
			case messages <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, ok := identity(); !ok {
				return
			}
		case m := <-messages:
			p, ok = identity()
			if !ok {
				return
			}
			switch m["type"] {
			case "ping":
				if !write(Event{"type": "pong"}) {
					return
				}
			case "get_status":
				if id, exists := number(m["printer_id"]); exists && id > 0 && !snapshots(p, id) {
					return
				}
			}
		case event, open := <-ch:
			if !open {
				return
			}
			p, ok = identity()
			if !ok {
				return
			}
			if allowed(event, p) && !write(event) {
				return
			}
		}
	}
}
