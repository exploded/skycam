package capture

import (
	"sync"
	"time"
)

// Hub hands frames from the capture loop to everything that shows or
// uploads them, and tracks who wants the live view.
type Hub struct {
	mu     sync.Mutex
	latest *Record
	subs   map[chan *Record]struct{}

	live      map[chan []byte]struct{} // viewers: keep live mode on
	taps      map[chan []byte]struct{} // listeners that do not
	liveSeen  time.Time                // last local viewer or remote demand
	liveSince time.Time                // when the current session started; zero if none
	liveOn    bool
}

func NewHub() *Hub {
	return &Hub{subs: map[chan *Record]struct{}{}, live: map[chan []byte]struct{}{}, taps: map[chan []byte]struct{}{}}
}

// Publish makes r the latest frame and offers it to every subscriber. A
// slow subscriber misses frames rather than holding up capture.
func (h *Hub) Publish(r *Record) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = r
	for ch := range h.subs {
		select {
		case ch <- r:
		default:
		}
	}
}

// Latest returns the newest frame, or nil before the first one.
func (h *Hub) Latest() *Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.latest
}

// Subscribe returns a channel of new frames and a function to stop.
func (h *Hub) Subscribe() (<-chan *Record, func()) {
	ch := make(chan *Record, 4)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Watch registers a live viewer and returns its stream of JPEGs. Holding
// the subscription is what keeps live mode running.
func (h *Hub) Watch() (<-chan []byte, func()) {
	ch := make(chan []byte, 2)
	h.mu.Lock()
	h.live[ch] = struct{}{}
	h.liveSeen = time.Now()
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.live, ch)
		h.liveSeen = time.Now()
		h.mu.Unlock()
	}
}

// Demand records a remote viewer (the relay) asking for live frames. It
// must be repeated to keep live mode going.
func (h *Hub) Demand() {
	h.mu.Lock()
	h.liveSeen = time.Now()
	h.mu.Unlock()
}

// WantLive reports whether anyone has wanted live frames within idle.
func (h *Hub) WantLive(now time.Time, idle time.Duration) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.live) > 0 || (!h.liveSeen.IsZero() && now.Sub(h.liveSeen) < idle)
}

// LiveViewers is the number of local streams open.
func (h *Hub) LiveViewers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.live)
}

// setLive records whether the camera is streaming, for the status page.
func (h *Hub) setLive(on bool, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.liveOn = on
	if on {
		h.liveSince = now
	} else {
		h.liveSince = time.Time{}
	}
}

// LiveState reports whether live mode is running and since when.
func (h *Hub) LiveState() (bool, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.liveOn, h.liveSince
}

// PublishLive sends one live JPEG to every viewer, dropping it for any
// viewer still sending the last one.
func (h *Hub) PublishLive(jpeg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.live {
		select {
		case ch <- jpeg:
		default:
		}
	}
	for ch := range h.taps {
		select {
		case ch <- jpeg:
		default:
		}
	}
}

// LiveSubscribe is Watch without counting as a viewer: the relay uploader
// listens this way so it never keeps live mode alive on its own.
func (h *Hub) LiveSubscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 2)
	h.mu.Lock()
	h.taps[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.taps, ch)
		h.mu.Unlock()
	}
}
