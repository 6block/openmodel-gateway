package gateway

import "testing"

// The floor lookup must recognise the model name as the worker actually reports
// it. A miss silently falls back to the permissive global MinScore, which is the
// difference between "32B held to a 32B bar" and "32B held to a 0.5 bar".
func TestFloorFor_MatchesRealWorkerModelNames(t *testing.T) {
	a := &auditor{cfg: ProbeConfig{MinScore: 0.5}}
	for _, name := range []string{
		"/models/Qwen--Qwen3-32B-AWQ",
		"/models/Qwen--Qwen2.5-3B-Instruct",
		"/models/Qwen--Qwen2.5-1.5B-Instruct",
	} {
		if f := a.floorFor(name); f == 0.5 {
			t.Errorf("%s fell back to MinScore — no per-model floor matched", name)
		} else {
			t.Logf("%s → floor %.4f", name, f)
		}
	}
}
