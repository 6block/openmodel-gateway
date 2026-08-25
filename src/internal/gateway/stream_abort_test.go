package gateway

import (
	"bufio"
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// slowSSEServer streams content deltas with a delay between each (so a client can read a
// few then disconnect BEFORE the final usage chunk), then usage + [DONE]. It stops early
// if the gateway cancels it (which happens when the client aborts).
func slowSSEServer(deltas int, gap time.Duration) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for i := 0; i < deltas; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"t%d\"}}]}\n\n", i)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(gap)
		}
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", deltas, deltas+3)
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestBilling_StreamHardClientAbortBillsDelivered: a client that opens a stream, reads a
// few content chunks, then HARD-aborts the connection (cancels the request context)
// BEFORE the final usage chunk must still be billed for the tokens actually delivered —
// not 0. This is the integration counterpart to the unit metering test: it confirms a
// real mid-stream client disconnect reaches the gateway's metered-billing path (isn't
// swallowed by a proxy error / ErrAbortHandler panic that would skip billing).
func TestBilling_StreamHardClientAbortBillsDelivered(t *testing.T) {
	up := slowSSEServer(20, 40*time.Millisecond)
	defer up.Close()
	gw, bc, cleanup := newBillingGateway(t, usdcWei(100000), up.URL) // ModelPricesUSD = $1/token
	defer cleanup()

	body := `{"model":"test-model","messages":[{"role":"user","content":"x"}],"max_tokens":30,"stream":true}`
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "POST", gw.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	// Read a few delivered content chunks, then hard-abort mid-stream.
	r := bufio.NewReader(resp.Body)
	got := 0
	for got < 3 {
		line, rerr := r.ReadString('\n')
		if rerr != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"content"`) {
			got++
		}
	}
	if got < 1 {
		t.Fatalf("client received no content before abort (test setup issue)")
	}
	reserved := bc.GetPendingSpend(billWallet) // ~estimate (max_tokens), still in-flight
	cancel()
	resp.Body.Close()

	// Wait for the gateway handler to observe the disconnect and finish billing —
	// pendingSpend changes from the up-front reservation to the settled amount.
	var final *big.Float
	for i := 0; i < 60; i++ {
		time.Sleep(100 * time.Millisecond)
		final = bc.GetPendingSpend(billWallet)
		if final.Cmp(reserved) != 0 { // handler adjusted the reservation → it finished
			break
		}
	}
	if final.Cmp(reserved) == 0 {
		t.Fatalf("gateway handler did not finish after client abort (pendingSpend stuck at reservation %s)", reserved.Text('f', 4))
	}
	if final.Sign() <= 0 {
		t.Errorf("hard client abort mid-stream must bill the delivered tokens, got pendingSpend=%s (delivered %d chunks)", final.Text('f', 4), got)
	}
}
