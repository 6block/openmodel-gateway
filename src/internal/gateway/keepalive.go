package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// keepalive.go — keep a streaming request's connection alive while it waits.
//
// A streaming chat request can wait a long time before the first byte: a queue window
// while workers are busy with a Filecoin proof, or a model switch (30–150 s for a large
// model). Many paths between a client and the gateway close a connection that carries
// no bytes for a while — corporate proxies, browser extensions, reverse proxies (nginx's
// default proxy_read_timeout is 60 s). The client then sees a bare network error
// ("Failed to fetch") although the request would have succeeded.
//
// waitKeepalive wraps the response writer of a streaming request. If nothing has been
// written after keepaliveAfter, it commits the response as a 200 event stream and sends
// an SSE comment line (": waiting for a worker") every keepaliveEvery until the worker's
// stream starts. SSE comments are ignored by every SSE parser, so the stream the client
// reads is unchanged. Once committed, a later error status (a 503 after the queue
// window, a worker's 400 refusal) can no longer be an HTTP status: it is delivered as an
// SSE error event, data: {"error": {..., "status": N}}, followed by data: [DONE].
//
// A request answered before keepaliveAfter (the usual case) is not touched at all.

const (
	defaultKeepaliveAfter = 5 * time.Second
	defaultKeepaliveEvery = 5 * time.Second
)

var keepaliveComment = []byte(": waiting for a worker\n\n")

type waitKeepalive struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	hdr      http.Header // headers set through this writer before the upstream starts
	started  bool        // committed by a keepalive (200, text/event-stream)
	upstream bool        // the handler itself has written (header or body)
	stopped  bool
	convert  int          // error status received after the commit; body is buffered
	errBuf   bytes.Buffer // buffered error body while converting
	done     chan struct{}
}

func newWaitKeepalive(w http.ResponseWriter, after, every time.Duration) *waitKeepalive {
	k := &waitKeepalive{w: w, hdr: w.Header().Clone(), done: make(chan struct{})}
	if after <= 0 {
		after = defaultKeepaliveAfter
	}
	if every <= 0 {
		every = defaultKeepaliveEvery
	}
	go k.loop(after, every)
	return k
}

func (k *waitKeepalive) loop(after, every time.Duration) {
	t := time.NewTimer(after)
	defer t.Stop()
	for {
		select {
		case <-k.done:
			return
		case <-t.C:
		}
		k.mu.Lock()
		if k.stopped || k.upstream {
			k.mu.Unlock()
			return
		}
		if !k.started {
			h := k.w.Header()
			for key, v := range k.hdr {
				h[key] = v
			}
			h.Set("Content-Type", "text/event-stream")
			h.Set("Cache-Control", "no-cache")
			h.Del("Content-Length")
			h.Del("Retry-After")
			k.w.WriteHeader(http.StatusOK)
			k.started = true
		}
		_, err := k.w.Write(keepaliveComment)
		if f, ok := k.w.(http.Flusher); ok {
			f.Flush()
		}
		k.mu.Unlock()
		if err != nil {
			return // client gone
		}
		t.Reset(every)
	}
}

// Started reports whether the response was already committed by a keepalive.
func (k *waitKeepalive) Started() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.started
}

func (k *waitKeepalive) Header() http.Header { return k.hdr }

func (k *waitKeepalive) WriteHeader(code int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.upstream && !k.started {
		return // a second WriteHeader is a no-op, as on http.ResponseWriter
	}
	k.upstream = true
	if k.started {
		if code >= 400 {
			k.convert = code
		}
		return
	}
	h := k.w.Header()
	for key := range h {
		if _, ok := k.hdr[key]; !ok {
			delete(h, key)
		}
	}
	for key, v := range k.hdr {
		h[key] = v
	}
	k.w.WriteHeader(code)
}

func (k *waitKeepalive) Write(b []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.upstream {
		// Implicit 200, as on http.ResponseWriter.
		k.upstream = true
		if !k.started {
			h := k.w.Header()
			for key, v := range k.hdr {
				h[key] = v
			}
		}
	}
	if k.convert != 0 {
		return k.errBuf.Write(b)
	}
	return k.w.Write(b)
}

func (k *waitKeepalive) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.convert != 0 {
		return
	}
	if f, ok := k.w.(http.Flusher); ok {
		f.Flush()
	}
}

// stop ends the keepalive and, if an error status arrived after the commit, delivers it
// as an SSE error event.
func (k *waitKeepalive) stop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	k.stopped = true
	close(k.done)
	if k.convert == 0 {
		return
	}
	errObj := map[string]any{"message": http.StatusText(k.convert), "type": "gateway_error"}
	var parsed struct {
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(k.errBuf.Bytes()), &parsed) == nil && parsed.Error != nil {
		errObj = parsed.Error
	}
	errObj["status"] = k.convert
	if k.convert == http.StatusServiceUnavailable {
		errObj["type"] = "service_unavailable"
	}
	ev, _ := json.Marshal(map[string]any{"error": errObj})
	k.w.Write([]byte("data: "))
	k.w.Write(ev)
	k.w.Write([]byte("\n\ndata: [DONE]\n\n"))
	if f, ok := k.w.(http.Flusher); ok {
		f.Flush()
	}
}
