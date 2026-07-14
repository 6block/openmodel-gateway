package main

import (
	"strings"
	"testing"
)

func onChain(processed bool) map[string]interface{} {
	return map[string]interface{}{"processed": processed}
}
func localAudit(found bool) map[string]interface{} {
	return map[string]interface{}{"found": found}
}

func TestSettlementVerdict(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]interface{}
		want string // substring
	}{
		{
			"consistent",
			map[string]interface{}{"on_chain": onChain(true), "local_audit": localAudit(true)},
			"OK — on-chain record matches",
		},
		{
			"chain-processed-no-local-record",
			map[string]interface{}{"on_chain": onChain(true), "local_audit": localAudit(false)},
			"audit-log gap",
		},
		{
			"local-record-not-on-chain",
			map[string]interface{}{"on_chain": onChain(false), "local_audit": localAudit(true)},
			"NOT marked processed on-chain",
		},
		{
			"neither",
			map[string]interface{}{"on_chain": onChain(false), "local_audit": localAudit(false)},
			"not processed on-chain and no local audit record",
		},
		{
			"no-local-section",
			map[string]interface{}{"on_chain": onChain(true)},
			"no local audit log available",
		},
	}
	for _, c := range cases {
		got := settlementVerdict(c.resp)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want substring %q", c.name, got, c.want)
		}
	}
}
