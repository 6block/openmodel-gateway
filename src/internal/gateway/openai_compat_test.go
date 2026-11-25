package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestImageGeneration_URLFormatIsHostedByTheGateway: response_format "url" reaches the
// worker as b64_json, is billed and hashed on the delivered bytes like any other
// request, and comes back as a link that serves exactly those bytes.
func TestImageGeneration_URLFormatIsHostedByTheGateway(t *testing.T) {
	var gotFormat string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotFormat, _ = req["response_format"].(string)
		body, _ := json.Marshal(map[string]any{"created": 1,
			"data":  []map[string]string{{"b64_json": pngB64(t, 0)}, {"b64_json": pngB64(t, 1)}},
			"usage": map[string]int{"images": 2}})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	defer up.Close()
	gw, bc := newImageGateway(t, up, imageCfg())
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()

	resp, body := postImages(t, srv.URL, `{"model":"`+imageModel+`","prompt":"x","n":2,"response_format":"url"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %v", resp.StatusCode, body)
	}
	if gotFormat != "b64_json" {
		t.Errorf("worker must be asked for b64_json, got %q", gotFormat)
	}
	if ps := bc.GetPendingSpend(billWallet); ps.Text('f', 2) != "4.00" {
		t.Errorf("billing must be unchanged by the url format, got %s", ps.Text('f', 6))
	}
	data := body["data"].([]any)
	for i, d := range data {
		item := d.(map[string]any)
		if _, has := item["b64_json"]; has {
			t.Errorf("item %d still carries b64_json", i)
		}
		u, _ := item["url"].(string)
		if !strings.HasPrefix(u, srv.URL+imageURLPrefix) || !strings.HasSuffix(u, ".png") {
			t.Fatalf("item %d: url %q", i, u)
		}
		get, err := http.Get(u) // no Authorization: the link is the capability
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(get.Body)
		get.Body.Close()
		if get.StatusCode != 200 || get.Header.Get("Content-Type") != "image/png" || !bytes.Equal(got, mockPNG(i)) {
			t.Errorf("item %d: served %d %s, bytes equal=%v", i, get.StatusCode, get.Header.Get("Content-Type"), bytes.Equal(got, mockPNG(i)))
		}
	}
	if r, _ := http.Get(srv.URL + imageURLPrefix + "0123456789abcdef0123456789abcdef.png"); r.StatusCode != 404 {
		t.Errorf("an unknown image must be 404, got %d", r.StatusCode)
	}
	if resp, _ := postImages(t, srv.URL, `{"model":"`+imageModel+`","prompt":"x","response_format":"gif"}`); resp.StatusCode != 400 {
		t.Errorf("unknown response_format must be 400, got %d", resp.StatusCode)
	}
}

func TestImageStore_ExpiresAndEvicts(t *testing.T) {
	s := newImageStore()
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	a := s.put([]byte("aaaa"), "image/png", "png")
	if _, ok := s.get(a); !ok {
		t.Fatal("fresh image missing")
	}
	now = now.Add(imageURLTTL + time.Second)
	if _, ok := s.get(a); ok {
		t.Error("expired image still served")
	}
	now = time.Unix(5000, 0)
	big := make([]byte, imageStoreMaxBytes/2+1)
	first := s.put(big, "image/png", "png")
	second := s.put(big, "image/png", "png")
	if _, ok := s.get(first); ok {
		t.Error("oldest image must be evicted when the store is full")
	}
	if _, ok := s.get(second); !ok {
		t.Error("newest image must stay")
	}
	if s.bytes > imageStoreMaxBytes {
		t.Errorf("store holds %d bytes, cap %d", s.bytes, imageStoreMaxBytes)
	}
}

func TestImageExt(t *testing.T) {
	for _, c := range []struct {
		b   []byte
		ext string
	}{{[]byte{0xFF, 0xD8, 0xFF, 0xE0}, "jpg"}, {[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "webp"}, {[]byte("\x89PNG\r\n\x1a\n"), "png"}} {
		if e, _ := imageExt(c.b); e != c.ext {
			t.Errorf("%x: %s, want %s", c.b, e, c.ext)
		}
	}
}

// TestTranscription_CompressedUploadsAreMeasuredAndBilled: MP3/Ogg/WebM/M4A uploads go
// through the same pipeline as WAV — the gateway measures the declared length, bills
// ceil seconds and hashes the upload — and /v1/audio/translations bills the same way.
func TestTranscription_CompressedUploadsAreMeasuredAndBilled(t *testing.T) {
	up := newSpeechUpstream(t)
	defer up.srv.Close()
	gw, _, logged := newSpeechGateway(t, up)
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	for _, name := range []string{"cbr_3_3.mp3", "opus_3_3.ogg", "opus_3_3.webm", "aac_3_3.m4a"} {
		audio, err := os.ReadFile(filepath.Join("testdata", "audiofmt", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/v1/audio/transcriptions", "/v1/audio/translations"} {
			ct, body := multipartAudio(t, asrModel, audio)
			resp, raw := do(t, srv.URL+path, ct, body)
			if resp.StatusCode != 200 {
				t.Fatalf("%s %s: %d %s", name, path, resp.StatusCode, raw)
			}
			if !bytes.Equal(up.gotBody.Load().([]byte), body) {
				t.Errorf("%s: multipart body must reach the worker byte-for-byte", name)
			}
		}
	}
	readLoggedNow(t, logged)
	for _, rec := range *logged {
		if rec.Quantity != 4 {
			t.Errorf("%s: billed %d seconds, want 4 (3.3 s rounded up)", rec.Path, rec.Quantity)
		}
	}
	if len(*logged) != 8 {
		t.Errorf("want 8 ledger rows, got %d", len(*logged))
	}
	last := (*logged)[len(*logged)-1]
	m4a, _ := os.ReadFile(filepath.Join("testdata", "audiofmt", "aac_3_3.m4a"))
	sum := sha256.Sum256(m4a)
	if last.AssetHash != hex.EncodeToString(sum[:]) || last.Path != "/v1/audio/translations" {
		t.Errorf("last row: %s %s", last.Path, last.AssetHash)
	}
}

// TestSpeech_EveryOpenAIFormatIsForwardedAndHashedAsDelivered.
func TestSpeech_EveryOpenAIFormatIsForwardedAndHashedAsDelivered(t *testing.T) {
	up := newSpeechUpstream(t)
	defer up.srv.Close()
	gw, _, logged := newSpeechGateway(t, up)
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler())
	defer srv.Close()
	for _, f := range []string{"mp3", "opus", "aac", "flac", "pcm", "wav", "MP3"} {
		req, _ := json.Marshal(map[string]any{"model": ttsModel, "input": "hello", "voice": "alloy", "response_format": f, "speed": 1.2})
		resp, raw := do(t, srv.URL+"/v1/audio/speech", "application/json", req)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", f, resp.StatusCode, raw)
		}
		if !bytes.Equal(up.gotBody.Load().([]byte), req) {
			t.Errorf("%s: body must be forwarded verbatim", f)
		}
		sum := sha256.Sum256(raw)
		readLoggedNow(t, logged)
		if rec := (*logged)[len(*logged)-1]; rec.AssetHash != hex.EncodeToString(sum[:]) || rec.Quantity != 5 {
			t.Errorf("%s: ledger %s/%d", f, rec.AssetHash, rec.Quantity)
		}
	}
}

// TestChat_MaxCompletionTokensSizesTheRequest: the newer OpenAI spelling reaches the
// worker as max_tokens (the reservation and the worker both read that field).
func TestChat_MaxCompletionTokensFoldsOntoMaxTokens(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"model":"m","max_completion_tokens":7}`, `7`},
		{`{"model":"m","max_completion_tokens":7,"max_tokens":9}`, `9`},
		{`{"model":"m","max_completion_tokens":null}`, ``},
		{`{"model":"m"}`, ``},
	}
	for _, c := range cases {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(foldMaxCompletionTokens([]byte(c.in)), &m)
		if string(m["max_tokens"]) != c.want {
			t.Errorf("%s → max_tokens %s, want %q", c.in, m["max_tokens"], c.want)
		}
	}
}

var _ = multipart.NewWriter
var _ = base64.StdEncoding
