package gateway

import (
	"errors"
	"math/rand"
	"path/filepath"
	"strings"

	"openmodel/sp-state-agent/internal/worker"
)

// ErrNoWorkerAvailable is returned when no Worker is idle or busy.
var ErrNoWorkerAvailable = errors.New("no available worker")

type candidate struct {
	worker worker.Worker
	weight float64
}

// selectWorkerForModel picks a Worker using model-aware weighted random selection.
//
// Priority:
//  1. Worker already has the requested model loaded (no switch needed)
//  2. Idle worker that lists the model in supported_models (will trigger model switch)
//  3. If model is "default" or empty, any available worker
//
// overloadFactor: when > 0, if ALL loaded workers have active_requests > gpu_count * overloadFactor,
// fall through to Priority 2 (trigger model switch on an idle worker) instead of piling onto
// the overloaded worker. Set to 0 to disable.
//
// Returns ErrNoWorkerAvailable if no suitable Worker is found.
func selectWorkerForModel(registry *worker.Registry, model string, exclude map[string]bool, overloadFactor ...float64) (*worker.Worker, error) {
	workers := registry.List()

	var loaded []candidate    // Priority 1: model already loaded
	var supported []candidate // Priority 2: model in supported_models, worker idle
	var fallback []candidate  // Priority 3: any available worker (for "default")

	isDefault := model == "" || model == "default"

	for _, w := range workers {
		if w.State != worker.StateIdle && w.State != worker.StateBusy {
			continue
		}
		if exclude != nil && exclude[w.ID] {
			continue
		}

		wt := computeWeight(&w)

		if isDefault {
			fallback = append(fallback, candidate{worker: w, weight: wt})
			continue
		}

		// Check if this worker already has the model loaded
		if modelMatches(w.LoadedModel, model) {
			loaded = append(loaded, candidate{worker: w, weight: wt})
			continue
		}

		// Check if this worker supports the model (can load it)
		if workerSupportsModel(&w, model) {
			// Prefer idle workers for model switch (busy workers would interrupt in-flight requests)
			if w.State == worker.StateIdle {
				supported = append(supported, candidate{worker: w, weight: wt})
			}
			continue
		}

		// Worker doesn't know this model — skip
	}

	// Check if loaded workers are overloaded — if so, prefer model switch on idle worker
	factor := 0.0
	if len(overloadFactor) > 0 {
		factor = overloadFactor[0]
	}
	if factor > 0 && len(loaded) > 0 && len(supported) > 0 {
		allOverloaded := true
		for _, c := range loaded {
			gpus := c.worker.GPUCount
			if gpus <= 0 {
				gpus = 1
			}
			threshold := int(float64(gpus) * factor)
			if c.worker.ActiveRequests <= threshold {
				allOverloaded = false
				break
			}
		}
		if allOverloaded {
			// All loaded workers are overloaded — use idle worker with model switch
			return weightedPick(supported)
		}
	}

	// Pick from highest priority non-empty group
	if len(loaded) > 0 {
		return weightedPick(loaded)
	}
	if len(supported) > 0 {
		return weightedPick(supported)
	}
	if isDefault && len(fallback) > 0 {
		return weightedPick(fallback)
	}

	return nil, ErrNoWorkerAvailable
}

// selectWorker picks any available Worker (model-agnostic, for backward compat).
func selectWorker(registry *worker.Registry) (*worker.Worker, error) {
	return selectWorkerForModel(registry, "default", nil)
}

// selectWorkerExcluding picks a Worker excluding given IDs (for retry).
func selectWorkerExcluding(registry *worker.Registry, exclude map[string]bool) (*worker.Worker, error) {
	return selectWorkerForModel(registry, "default", exclude)
}

// modelMatches checks if a worker's loaded model matches the requested model.
// Handles path variations: "/models/Qwen--Qwen2.5-1.5B" matches "Qwen/Qwen2.5-1.5B".
func modelMatches(loadedModel, requestedModel string) bool {
	if loadedModel == "" || requestedModel == "" {
		return false
	}
	if loadedModel == requestedModel {
		return true
	}
	// Compare basenames: "Qwen--Qwen2.5-1.5B-Instruct" vs "Qwen/Qwen2.5-1.5B-Instruct"
	loadedBase := filepath.Base(loadedModel)
	requestedBase := filepath.Base(requestedModel)
	if loadedBase == requestedBase {
		return true
	}
	// Normalize both to HuggingFace ID format for comparison
	// "Qwen--Qwen2.5-1.5B-Instruct" → "Qwen/Qwen2.5-1.5B-Instruct"
	loadedNorm := strings.Replace(loadedBase, "--", "/", 1)
	requestedNorm := strings.Replace(requestedBase, "--", "/", 1)
	if loadedNorm == requestedNorm {
		return true
	}
	// Also compare normalized against full paths
	if loadedNorm == requestedModel || requestedNorm == loadedModel {
		return true
	}
	// Suffix match (either direction)
	if strings.HasSuffix(loadedModel, requestedModel) || strings.HasSuffix(requestedModel, loadedModel) {
		return true
	}
	return false
}

// workerSupportsModel checks if the requested model is in the worker's supported_models list.
func workerSupportsModel(w *worker.Worker, model string) bool {
	for _, m := range w.SupportedModels {
		if m == model || modelMatches(m, model) {
			return true
		}
	}
	return false
}

// weightedPick selects one candidate using weighted random.
func weightedPick(candidates []candidate) (*worker.Worker, error) {
	if len(candidates) == 0 {
		return nil, ErrNoWorkerAvailable
	}
	if len(candidates) == 1 {
		w := candidates[0].worker
		return &w, nil
	}

	var totalWeight float64
	for _, c := range candidates {
		totalWeight += c.weight
	}

	r := rand.Float64() * totalWeight
	for _, c := range candidates {
		r -= c.weight
		if r <= 0 {
			w := c.worker
			return &w, nil
		}
	}

	w := candidates[len(candidates)-1].worker
	return &w, nil
}

// computeWeight returns a load-balancing weight for a worker.
//
//	idle (0 requests):  weight = GPUCount
//	busy (N requests):  weight = max(1, GPUCount / (1 + N/GPUCount))
func computeWeight(w *worker.Worker) float64 {
	gpus := w.GPUCount
	if gpus <= 0 {
		gpus = 1
	}
	if w.ActiveRequests == 0 {
		return float64(gpus)
	}
	loadFactor := float64(w.ActiveRequests) / float64(gpus)
	weight := float64(gpus) / (1.0 + loadFactor)
	if weight < 1 {
		weight = 1
	}
	return weight
}
