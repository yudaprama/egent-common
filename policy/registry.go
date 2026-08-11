package policy

import "sync"

// PolicyRegistry maps tool names to their XAgenticAccess policies. It is
// built once at startup and read concurrently at request time.
type PolicyRegistry struct {
	mu       sync.RWMutex
	policies map[string]XAgenticAccess
	default_ XAgenticAccess
}

// NewRegistry creates a registry with the given default policy for unknown tools.
func NewRegistry(defaultPolicy XAgenticAccess) *PolicyRegistry {
	return &PolicyRegistry{
		policies: make(map[string]XAgenticAccess),
		default_: defaultPolicy,
	}
}

// Register associates a tool name with a policy. Call at startup only.
func (r *PolicyRegistry) Register(name string, p XAgenticAccess) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.policies[name] = p
}

// RegisterBatch registers multiple tool names with the same policy.
func (r *PolicyRegistry) RegisterBatch(names []string, p XAgenticAccess) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range names {
		r.policies[name] = p
	}
}

// Lookup returns the policy for the given tool name, or the default if not found.
func (r *PolicyRegistry) Lookup(name string) XAgenticAccess {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p, ok := r.policies[name]; ok {
		return p
	}
	return r.default_
}
