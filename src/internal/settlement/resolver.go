package settlement

// WorkerLister is implemented by worker.Registry.
type WorkerLister interface {
	ListWorkerSPMap() map[string]string
}

// minerPayoutResolver is the optional extension a resolver may implement to
// supply the miner → miner-signed EVM payout overlay (self-registered SPs).
type minerPayoutResolver interface {
	GetMinerPayoutMap() map[string]string
}

// RegistryResolver wraps a worker registry to implement WorkerSPResolver.
type RegistryResolver struct {
	lister WorkerLister
}

func NewRegistryResolver(lister WorkerLister) *RegistryResolver {
	return &RegistryResolver{lister: lister}
}

func (r *RegistryResolver) GetWorkerSPMap() map[string]string {
	if r.lister == nil {
		return nil
	}
	return r.lister.ListWorkerSPMap()
}

// GetMinerPayoutMap surfaces the registry's miner → miner-signed payout overlay
// when the underlying lister provides one (worker.Registry does). Optional so
// existing fakes/listers keep compiling.
func (r *RegistryResolver) GetMinerPayoutMap() map[string]string {
	if pl, ok := r.lister.(interface{ ListMinerPayoutMap() map[string]string }); ok {
		return pl.ListMinerPayoutMap()
	}
	return nil
}
