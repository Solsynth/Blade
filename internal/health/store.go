package health

import (
	"sync"
	"time"
)

type ServiceState struct {
	ServiceName string
	IsHealthy   bool
	LastChecked time.Time
}

type ReadinessStore struct {
	mu           sync.RWMutex
	services     map[string]ServiceState
	coreServices map[string]bool
}

func NewReadinessStore(coreServiceNames []string) *ReadinessStore {
	coreServices := make(map[string]bool)
	for _, name := range coreServiceNames {
		coreServices[name] = false
	}
	return &ReadinessStore{
		services:     make(map[string]ServiceState),
		coreServices: coreServices,
	}
}

func (s *ReadinessStore) UpdateService(state ServiceState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.services[state.ServiceName] = state
}

func (s *ReadinessStore) GetServiceState(serviceName string) (ServiceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.services[serviceName]
	return state, ok
}

func (s *ReadinessStore) IsCoreServiceHealthy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for name, healthy := range s.coreServices {
		if !healthy {
			state, ok := s.services[name]
			if !ok || !state.IsHealthy {
				return false
			}
		}
	}
	return true
}

func (s *ReadinessStore) GetAllStates() map[string]ServiceState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	states := make(map[string]ServiceState, len(s.services))
	for k, v := range s.services {
		states[k] = v
	}
	return states
}
