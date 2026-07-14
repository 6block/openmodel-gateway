package settlement

// WorkerLister is implemented by worker.Registry.
type WorkerLister interface {
	ListWorkerSPMap() map[string]string
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
