// tuios-pip helper: serves the viewer page and relays recorded video slices,
// playback state and player controls between the browser extension and the
// viewer.
package main

import (
	"embed"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"
)

// TUIOS_PIP_DEBUG=1 logs every message type
var debug = os.Getenv("TUIOS_PIP_DEBUG") != ""

//go:embed viewer
var viewerFS embed.FS

const (
	pollWait   = 20 * time.Second
	staleAfter = 45 * time.Second
	// about 30s of 100ms slices
	maxQueue = 300
)

type peer struct {
	id     string
	title  string
	host   string
	paused bool
	w, h   int
	queue  []json.RawMessage
	wake   chan struct{}
	seen   time.Time
}

func newPeer(id string) *peer {
	return &peer{id: id, wake: make(chan struct{}, 1), seen: time.Now()}
}

func (p *peer) push(msg json.RawMessage) {
	p.queue = append(p.queue, msg)
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

type hub struct {
	mu      sync.Mutex
	sources map[string]*peer
	active  string
	// set when the viewer picked a tab by hand: a video starting elsewhere
	// doesn't take over until this tab goes away or another is picked
	pinned string
	viewer *peer
}

// removeSource forgets a tab and moves on if it was the one being shown.
// Caller holds h.mu.
func (h *hub) removeSource(id string) {
	delete(h.sources, id)
	if id == h.pinned {
		h.pinned = ""
	}
	if id == h.active {
		h.setActive(h.pickOther())
	}
	h.notify()
}

// notify sends the viewer the current list of tabs for its picker
func (h *hub) notify() {
	if h.viewer == nil {
		return
	}
	type item struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Host   string `json:"host"`
		Paused bool   `json:"paused"`
	}
	list := []item{}
	for _, p := range h.sources {
		if p.title == "" && p.host == "" {
			continue // polled but never said hello
		}
		list = append(list, item{p.id, p.title, p.host, p.paused})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Title < list[j].Title })
	b, _ := json.Marshal(map[string]any{
		"type":   "sources",
		"active": h.active,
		"pinned": h.pinned != "",
		"list":   list,
	})
	h.viewer.push(b)
}

func (h *hub) prune() {
	for id, p := range h.sources {
		if time.Since(p.seen) > staleAfter {
			h.removeSource(id)
		}
	}
	if h.viewer != nil && time.Since(h.viewer.seen) > staleAfter {
		h.dropViewer()
	}
}

// nobody is watching, so let the tab stop encoding
func (h *hub) dropViewer() {
	h.viewer = nil
	if src := h.sources[h.active]; src != nil {
		src.push(raw(`{"type":"stop"}`))
	}
}

// setActive switches which tab is shared and restarts the stream if someone
// is watching. Caller holds h.mu.
func (h *hub) setActive(id string) {
	if id == h.active {
		return
	}
	if old := h.sources[h.active]; old != nil {
		old.push(raw(`{"type":"stop"}`))
	}
	h.active = id
	if h.viewer == nil {
		return
	}
	h.notify()
	if src := h.sources[id]; src != nil {
		h.viewer.push(raw(`{"type":"reset"}`))
		src.push(raw(`{"type":"start"}`))
	} else {
		h.viewer.push(raw(`{"type":"idle"}`))
	}
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

type envelope struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Host    string `json:"host"`
	Playing bool   `json:"playing"`
	Paused  bool   `json:"paused"`
	Source  string `json:"source"`
	W       int    `json:"w"`
	H       int    `json:"h"`
}

func (h *hub) handleMsg(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || env.Type == "" {
		http.Error(w, "bad message", http.StatusBadRequest)
		return
	}
	from, id := r.URL.Query().Get("from"), r.URL.Query().Get("id")
	if debug {
		log.Printf("%s %.8s -> %s (%d bytes)", from, id, env.Type, len(body))
	}
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.prune()

	switch from {
	case "source":
		src := h.sources[id]
		if src == nil {
			src = newPeer(id)
			h.sources[id] = src
		}
		src.seen = time.Now()
		switch env.Type {
		case "hello":
			changed := src.title != env.Title || src.host != env.Host || src.paused != env.Paused
			src.title, src.host, src.paused = env.Title, env.Host, env.Paused
			if env.W > 0 && env.H > 0 {
				src.w, src.h = env.W, env.H
			}
			// the tab that most recently started playing wins, unless the
			// viewer pinned one
			if (env.Playing && h.pinned == "") || h.active == "" {
				h.setActive(id)
			}
			if changed {
				h.notify()
			}
		case "bye":
			h.removeSource(id)
		default:
			if env.Type == "state" && src.paused != env.Paused {
				src.paused = env.Paused
				h.notify()
			}
			if id == h.active && h.viewer != nil {
				// a viewer that stopped reading would make this grow
				// forever; drop the backlog and restart the recording
				if len(h.viewer.queue) > maxQueue {
					h.viewer.queue = nil
					h.viewer.push(raw(`{"type":"reset"}`))
					src.push(raw(`{"type":"start"}`))
					break
				}
				h.viewer.push(body)
			}
		}
	case "viewer":
		if env.Type == "join" {
			h.viewer = newPeer(id)
			h.notify()
			if src := h.sources[h.active]; src != nil {
				src.push(raw(`{"type":"start"}`))
			} else {
				h.viewer.push(raw(`{"type":"idle"}`))
			}
			break
		}
		if h.viewer == nil || h.viewer.id != id {
			http.Error(w, "not the current viewer", http.StatusGone)
			return
		}
		h.viewer.seen = time.Now()
		switch env.Type {
		case "bye":
			h.dropViewer()
		case "pick":
			if h.sources[env.Source] != nil {
				h.pinned = env.Source
				h.setActive(env.Source)
				h.notify()
			}
		case "next":
			go mediaControl("next-track")
		case "prev":
			go mediaControl("previous-track")
		default:
			if src := h.sources[h.active]; src != nil {
				src.push(body)
			}
		}
	default:
		http.Error(w, "from must be source or viewer", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *hub) pickOther() string {
	var best *peer
	for _, p := range h.sources {
		if best == nil || p.seen.After(best.seen) {
			best = p
		}
	}
	if best == nil {
		return ""
	}
	return best.id
}

// long poll: hold the request until something is queued or pollWait passes
func (h *hub) handlePoll(w http.ResponseWriter, r *http.Request) {
	role, id := r.URL.Query().Get("role"), r.URL.Query().Get("id")

	h.mu.Lock()
	var p *peer
	switch role {
	case "source":
		p = h.sources[id]
		if p == nil {
			p = newPeer(id)
			h.sources[id] = p
		}
	case "viewer":
		if h.viewer != nil && h.viewer.id == id {
			p = h.viewer
		}
	}
	if p == nil {
		h.mu.Unlock()
		http.Error(w, "unknown peer", http.StatusGone)
		return
	}
	p.seen = time.Now()
	h.mu.Unlock()

	timer := time.NewTimer(pollWait)
	defer timer.Stop()
	for {
		h.mu.Lock()
		if len(p.queue) > 0 {
			msgs := p.queue
			p.queue = nil
			p.seen = time.Now()
			h.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(msgs)
			return
		}
		h.mu.Unlock()
		select {
		case <-p.wake:
		case <-timer.C:
			h.mu.Lock()
			p.seen = time.Now()
			h.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, "[]")
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (h *hub) handleStatus(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prune()
	title, vw, vh := "", 0, 0
	if src := h.sources[h.active]; src != nil {
		title, vw, vh = src.title, src.w, src.h
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"sources": len(h.sources),
		"active":  title,
		"viewer":  h.viewer != nil,
		"width":   vw,
		"height":  vh,
	})
}

func mediaControl(cmd string) {
	bin, err := exec.LookPath("media-control")
	if err != nil {
		bin = "/opt/homebrew/bin/media-control"
	}
	if err := exec.Command(bin, cmd).Run(); err != nil {
		log.Printf("media-control %s: %v", cmd, err)
	}
}

// guard keeps random web pages out. They can reach 127.0.0.1 too, but they
// can't send a custom header without a CORS preflight, which we never answer,
// and a rebound DNS name won't match the Host check.
func guard(port string, next http.Handler) http.Handler {
	want := map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !want[r.Host] {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/health" && r.Header.Get("X-Pip") != "1" {
			http.Error(w, "missing X-Pip header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	port := os.Getenv("TUIOS_PIP_PORT")
	if port == "" {
		port = "7420"
	}
	if len(os.Args) > 1 && os.Args[1] == "view" {
		runView(port, os.Args[2:])
		return
	}
	h := &hub{sources: map[string]*peer{}}

	page, err := viewerFS.ReadFile("viewer/index.html")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("GET /status", h.handleStatus)
	mux.HandleFunc("POST /msg", h.handleMsg)
	mux.HandleFunc("GET /poll", h.handlePoll)

	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("tuios-pip on http://127.0.0.1:%s", port)
	log.Fatal(http.Serve(ln, guard(port, mux)))
}
