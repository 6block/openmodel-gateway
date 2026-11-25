package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// imagehost.go — response_format "url" for image generation.
//
// Workers return images inline (b64_json): they have no public file store, and the
// gateway must see the bytes anyway to count and hash what it bills. For a client that
// asks for URLs (the OpenAI default for DALL·E models), the gateway keeps the delivered
// bytes for a limited time and answers with links to them:
//
//	GET /v1/files/images/<id>.<ext>    no authentication; <id> is 128 random bits
//
// Links expire after imageURLTTL, as OpenAI's do. Storage is in memory and bounded by
// imageStoreMaxBytes (oldest evicted first); a gateway restart drops all links. Billing
// is unaffected: the asset hash and quantity are computed from the worker's reply
// before the reply is rewritten.

const (
	imageURLTTL        = time.Hour
	imageStoreMaxBytes = 512 << 20
	imageURLPrefix     = "/v1/files/images/"
)

type storedImage struct {
	data    []byte
	ctype   string
	expires time.Time
}

type imageStore struct {
	mu    sync.Mutex
	items map[string]*storedImage
	order []string // insertion order, for eviction
	bytes int
	now   func() time.Time
}

func newImageStore() *imageStore {
	return &imageStore{items: map[string]*storedImage{}, now: time.Now}
}

// put stores data and returns its file name (<id>.<ext>).
func (s *imageStore) put(data []byte, ctype, ext string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	name := hex.EncodeToString(b[:]) + "." + ext
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(len(data))
	s.items[name] = &storedImage{data: data, ctype: ctype, expires: s.now().Add(imageURLTTL)}
	s.order = append(s.order, name)
	s.bytes += len(data)
	return name
}

func (s *imageStore) evictLocked(incoming int) {
	now := s.now()
	keep := s.order[:0]
	for _, n := range s.order {
		it, ok := s.items[n]
		if !ok {
			continue
		}
		if now.After(it.expires) || s.bytes+incoming > imageStoreMaxBytes {
			s.bytes -= len(it.data)
			delete(s.items, n)
			continue
		}
		keep = append(keep, n)
	}
	s.order = keep
}

func (s *imageStore) get(name string) (*storedImage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[name]
	if !ok || s.now().After(it.expires) {
		return nil, false
	}
	return it, true
}

func (g *Gateway) handleImageFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		jsonError(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, imageURLPrefix)
	it, ok := g.images.get(name)
	if !ok {
		jsonError(w, "image not found or expired", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", it.ctype)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(it.data)
}

// imageExt names the file type of image bytes by their signature.
func imageExt(b []byte) (ext, ctype string) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpg", "image/jpeg"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp", "image/webp"
	}
	return "png", "image/png"
}

// publicBaseURL is the scheme://host clients reach this gateway at: the configured
// public_base_url, else the request's own Host (and X-Forwarded-Proto from a trusted
// proxy).
func (g *Gateway) publicBaseURL(r *http.Request) string {
	if g.publicBase != "" {
		return strings.TrimRight(g.publicBase, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" && isTrustedProxy(remoteHost(r)) {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	return scheme + "://" + r.Host
}

// imagesToURLs rewrites a worker reply {data:[{b64_json}],...} into URL form, storing
// each image. A reply that is not of that shape is returned unchanged.
func (g *Gateway) imagesToURLs(r *http.Request, respBody []byte) []byte {
	var parsed map[string]json.RawMessage
	if json.Unmarshal(respBody, &parsed) != nil {
		return respBody
	}
	var data []map[string]json.RawMessage
	if json.Unmarshal(parsed["data"], &data) != nil {
		return respBody
	}
	base := g.publicBaseURL(r)
	for _, item := range data {
		var b64 string
		if json.Unmarshal(item["b64_json"], &b64) != nil || b64 == "" {
			continue
		}
		raw, err := base64Decode(b64)
		if err != nil {
			continue
		}
		ext, ctype := imageExt(raw)
		name := g.images.put(raw, ctype, ext)
		url, _ := json.Marshal(fmt.Sprintf("%s%s%s", base, imageURLPrefix, name))
		delete(item, "b64_json")
		item["url"] = url
	}
	parsed["data"], _ = json.Marshal(data)
	out, err := json.Marshal(parsed)
	if err != nil {
		return respBody
	}
	return out
}

func remoteHost(r *http.Request) string {
	h := r.RemoteAddr
	if i := strings.LastIndex(h, ":"); i > 0 {
		h = h[:i]
	}
	return strings.Trim(h, "[]")
}
