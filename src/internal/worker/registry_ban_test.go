package worker

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

// The punishment fields must survive a gateway restart: a reboot lifting a ban
// (or dropping a miner-signed payout address) would defeat both levers.
func TestBanAndPayoutSurviveRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "workers.json")

	r1 := NewRegistry(logger, path)
	if _, err := r1.Register(WorkerRegistration{
		ID:             "sp-1",
		Endpoint:       "http://x:8000",
		SchedulerURL:   "http://x:9090",
		GPUCount:       1,
		MinerAddress:   "t01000",
		AuthToken:      "wk-secret",
		PayoutAddress:  "0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc",
		SelfRegistered: true,
	}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if !r1.SetBan("sp-1", until, "substandard output") {
		t.Fatal("SetBan failed")
	}

	r2 := NewRegistry(logger, path) // simulated restart
	w, ok := r2.Get("sp-1")
	if !ok {
		t.Fatal("worker lost across restart")
	}
	if !w.SelfRegistered || w.PayoutAddress != "0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc" {
		t.Fatalf("payout/self-registered lost: %+v", w)
	}
	if !w.IsBanned() || !w.BannedUntil.Equal(until) || w.BanReason != "substandard output" {
		t.Fatalf("ban lost across restart: %+v", w)
	}
	if m := r2.ListMinerPayoutMap(); m["t01000"] != "0x9965507D1a55bcC2695C58ba16FB37d819B0A4dc" {
		t.Fatalf("payout map after restart: %v", m)
	}

	// Admin re-register WITHOUT payout must not wipe the signed value.
	if _, err := r2.Register(WorkerRegistration{
		ID: "sp-1", Endpoint: "http://y:8000", SchedulerURL: "http://y:9090", GPUCount: 1,
		MinerAddress: "t01000", AuthToken: "wk-new",
	}); err != nil {
		t.Fatal(err)
	}
	w2, _ := r2.Get("sp-1")
	if w2.PayoutAddress == "" || !w2.SelfRegistered {
		t.Fatalf("admin update wiped signed payout: %+v", w2)
	}
}

func TestFindByMinerAndCount(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRegistry(logger, "")
	if _, err := r.Register(WorkerRegistration{
		ID: "a", Endpoint: "http://x:8000", SchedulerURL: "http://x:9090", MinerAddress: "t01000",
	}); err != nil {
		t.Fatal(err)
	}
	if w, ok := r.FindByMiner("t01000"); !ok || w.ID != "a" {
		t.Fatalf("FindByMiner = %v, %v", w, ok)
	}
	if _, ok := r.FindByMiner("t09999"); ok {
		t.Fatal("unknown miner found")
	}
	if r.Count() != 1 {
		t.Fatalf("Count = %d", r.Count())
	}
}
