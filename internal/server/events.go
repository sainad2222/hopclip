package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

type event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
	// From is the session that caused the event, so the originating device
	// can tell its own changes apart from other devices'.
	From string `json:"from,omitempty"`
}

type subscriber struct {
	userID    int64
	sessionID string
	ch        chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func (s *subscriber) close() { s.closeOnce.Do(func() { close(s.done) }) }

// hub fans events out to every open event stream of a user.
type hub struct {
	mu   sync.Mutex
	subs map[int64]map[*subscriber]struct{}
}

func newHub() *hub { return &hub{subs: map[int64]map[*subscriber]struct{}{}} }

func (h *hub) subscribe(userID int64, sessionID string) *subscriber {
	s := &subscriber{userID: userID, sessionID: sessionID, ch: make(chan []byte, 32), done: make(chan struct{})}
	h.mu.Lock()
	if h.subs[userID] == nil {
		h.subs[userID] = map[*subscriber]struct{}{}
	}
	h.subs[userID][s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	h.removeLocked(s)
	h.mu.Unlock()
}

func (h *hub) removeLocked(s *subscriber) {
	if m := h.subs[s.userID]; m != nil {
		delete(m, s)
		if len(m) == 0 {
			delete(h.subs, s.userID)
		}
	}
	s.close()
}

func (h *hub) publish(userID int64, ev event) {
	msg, err := json.Marshal(ev)
	if err != nil {
		slog.Error("marshal event", "err", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[userID] {
		select {
		case s.ch <- msg:
		default:
			// A stalled stream is dropped rather than blocking everyone else;
			// the client reconnects and resyncs from the REST endpoints.
			h.removeLocked(s)
		}
	}
}

func (h *hub) publishAll(ev event) {
	h.mu.Lock()
	ids := make([]int64, 0, len(h.subs))
	for id := range h.subs {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		h.publish(id, ev)
	}
}

func (h *hub) kickSessions(userID int64, sessionIDs ...string) {
	set := map[string]bool{}
	for _, id := range sessionIDs {
		set[id] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[userID] {
		if set[s.sessionID] {
			h.removeLocked(s)
		}
	}
}

func (h *hub) kickUser(userID int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[userID] {
		h.removeLocked(s)
	}
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.subs {
		for s := range m {
			h.removeLocked(s)
		}
	}
}

func (h *hub) onlineSessions(userID int64) map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]bool{}
	for s := range h.subs[userID] {
		out[s.sessionID] = true
	}
	return out
}

// Cloudflare drops idle connections after 100s; a comment line every 25s
// keeps the stream open through any proxy.
var heartbeatInterval = 25 * time.Second

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r)
	rc := http.NewResponseController(w)

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache, no-transform")
	// Disables response buffering in nginx so events are not held back.
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sub := s.hub.subscribe(a.user.ID, a.session.ID)
	defer s.hub.unsubscribe(sub)

	hello, _ := json.Marshal(event{Type: "hello", Data: map[string]string{"session_id": a.session.ID}})
	if _, err := fmt.Fprintf(w, "retry: 3000\n\ndata: %s\n\n", hello); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}

	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-sub.done:
			return
		case msg := <-sub.ch:
			_, err = fmt.Fprintf(w, "data: %s\n\n", msg)
		case <-tick.C:
			// Sessions revoked from the CLI or expired in place are not known
			// to the hub, so the stream re-checks its own session here.
			if ok, _ := s.store.SessionActive(r.Context(), a.session.ID); !ok {
				return
			}
			_, err = fmt.Fprint(w, ": ping\n\n")
		}
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}
