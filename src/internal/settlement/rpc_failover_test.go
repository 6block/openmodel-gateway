package settlement

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// C2 multi-RPC failover tests: the contract client builds a de-duplicated failover list,
// dials the first HEALTHY endpoint (not merely the first that TCP-connects), and rotates
// to a healthy one when the active endpoint stops answering.

func TestBuildEndpoints_OrderDedupBlank(t *testing.T) {
	got := buildEndpoints("  https://a  ", []string{"https://b", "", "https://a", "https://c", "  "})
	want := []string{"https://a", "https://b", "https://c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildEndpoints: rpc_url must lead, blanks dropped, dups collapsed\n got=%v\nwant=%v", got, want)
	}
	// Empty primary + only extras still works (primary is optional once rpc_urls exist).
	if g := buildEndpoints("", []string{"https://x"}); !reflect.DeepEqual(g, []string{"https://x"}) {
		t.Fatalf("empty primary: got %v", g)
	}
	if g := buildEndpoints("", nil); len(g) != 0 {
		t.Fatalf("no endpoints: got %v", g)
	}
}

// fakeRPC serves the minimal JSON-RPC the failover logic probes (eth_chainId,
// eth_blockNumber). When up is false it returns 503, simulating a dead provider.
func fakeRPC(t *testing.T, chainID int64, up *atomic.Bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up != nil && !up.Load() {
			http.Error(w, "provider down", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		result := "0x0"
		switch req.Method {
		case "eth_chainId":
			result = fmt.Sprintf("0x%x", chainID)
		case "eth_blockNumber":
			result = "0x10"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%q}`, req.ID, result)
	}))
}

func TestDialFirstHealthy_SkipsDeadPrefersHealthy(t *testing.T) {
	up := &atomic.Bool{}
	up.Store(true)
	live := fakeRPC(t, 314159, up)
	defer live.Close()

	dead := "http://127.0.0.1:1" // nothing listening → probe fails

	// dead first, live second → must skip dead and land on live (index 1).
	client, idx, err := dialFirstHealthy([]string{dead, live.URL}, 0)
	if err != nil {
		t.Fatalf("must find the healthy endpoint: %v", err)
	}
	client.Close()
	if idx != 1 {
		t.Fatalf("must select the live endpoint index 1, got %d", idx)
	}

	// start=1 wraps back to the live endpoint even though we asked to start past it.
	if _, idx2, err := dialFirstHealthy([]string{live.URL, dead}, 1); err != nil || idx2 != 0 {
		t.Fatalf("wrap-around must reach live endpoint 0: idx=%d err=%v", idx2, err)
	}

	// All dead → error, no client.
	if c, _, err := dialFirstHealthy([]string{dead, "http://127.0.0.1:2"}, 0); err == nil {
		c.Close()
		t.Fatal("all-dead must return an error")
	}
}

func testKeyHex(t *testing.T) string {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(crypto.FromECDSA(k))
}

// End-to-end rotation: build a client on two live endpoints, kill the active one, run one
// rotation, and confirm the client swapped to the survivor and can still serve calls.
func TestRotateEndpoint_SwapsToHealthySurvivor(t *testing.T) {
	upA, upB := &atomic.Bool{}, &atomic.Bool{}
	upA.Store(true)
	upB.Store(true)
	srvA := fakeRPC(t, 314159, upA)
	defer srvA.Close()
	srvB := fakeRPC(t, 314159, upB)
	defer srvB.Close()

	cfg := &Config{
		Enabled:            true,
		RPCURL:             srvA.URL,
		RPCURLs:            []string{srvB.URL},
		ChainID:            314159,
		ContractAddress:    "0x000000000000000000000000000000000000dEaD",
		OperatorPrivateKey: testKeyHex(t),
	}
	c, err := NewContractClient(cfg, discardLogger())
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	defer c.Close()

	if c.ActiveEndpoint() != srvA.URL {
		t.Fatalf("must start on rpc_url (A): got %s", c.ActiveEndpoint())
	}

	// A goes down; a rotation triggered by the failed probe must move to B.
	upA.Store(false)
	c.rotateEndpoint(fmt.Errorf("simulated probe failure"))
	if c.ActiveEndpoint() != srvB.URL {
		t.Fatalf("must rotate to survivor B: got %s", c.ActiveEndpoint())
	}
	// The swapped-in client actually works (a real BlockNumber round-trip through B).
	if _, err := c.curClient().BlockNumber(t.Context()); err != nil {
		t.Fatalf("rotated client must serve calls: %v", err)
	}

	// Both down now: rotation keeps the existing client (does not nil it out), so calls
	// still have something to attempt and self-heal on recovery.
	upB.Store(false)
	c.rotateEndpoint(fmt.Errorf("both down"))
	if c.curClient() == nil {
		t.Fatal("total outage must not drop the client")
	}
}
