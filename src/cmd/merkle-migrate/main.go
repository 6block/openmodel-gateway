// Command merkle-migrate converts a pre-R4 "fat" merkle ledger (records embedded in each
// batch line) into the R4 layout: a thin merkle-batches.jsonl, an external
// receipt-records.jsonl, and a receipt-index.jsonl sidecar.
//
// Run it OFFLINE (gateway stopped) against the settlement data dir, before starting the R4
// build:
//
//	merkle-migrate /data
//
// The original ledger is preserved as merkle-batches.jsonl.pre-r4. Skipping the migration is
// safe — the R4 gateway reads fat lines directly and rebuilds its index on first start — but
// running it makes that first start instant and shrinks the ledger ~5x.
package main

import (
	"fmt"
	"os"

	"openmodel/sp-state-agent/internal/settlement"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] == "-h" || os.Args[1] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: merkle-migrate <settlement-data-dir>")
		os.Exit(2)
	}
	dir := os.Args[1]
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "not a directory: %s\n", dir)
		os.Exit(2)
	}

	stats, err := settlement.MigrateFatLedger(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "migration failed: %v\n", err)
		os.Exit(1)
	}
	if stats == nil {
		fmt.Println("nothing to migrate (no merkle-batches.jsonl)")
		return
	}
	fmt.Printf("migrated %d batches / %d leaves; externalized %d records\n",
		stats.Batches, stats.Leaves, stats.RecordsMoved)
	fmt.Printf("ledger %d B -> %d B (thin) + %d B records\n",
		stats.OldLedgerSize, stats.NewLedgerSize, stats.RecordSize)
	fmt.Println("original preserved as merkle-batches.jsonl.pre-r4")
}
