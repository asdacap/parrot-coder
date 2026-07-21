// Package agent owns deterministic agent profiles and session execution.
package agent

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/agent/profiles"
)

const (
	BuildID    = profiles.BuildID
	PlanID     = profiles.PlanID
	ExploreID  = "explore"
	ExplorerID = profiles.ExplorerID
	ReviewID   = profiles.ReviewID
	WorkerID   = profiles.WorkerID
)

type Profile = profiles.Profile

type Registry struct {
	mu       sync.RWMutex
	profiles map[string]Profile
	def      string
}

func NewRegistry(profiles ...Profile) (*Registry, error) {
	r := &Registry{profiles: make(map[string]Profile), def: BuildID}
	if len(profiles) == 0 {
		profiles = Builtins()
	}
	for _, profile := range profiles {
		if err := r.Register(profile); err != nil {
			return nil, err
		}
	}
	if _, ok := r.profiles[r.def]; !ok {
		r.def = r.List()[0].ID
	}
	return r, nil
}

func Builtins() []Profile {
	return []Profile{
		profiles.Build(),
		profiles.Plan(),
		profiles.Explorer(),
		profiles.Review(),
		profiles.Worker(),
	}
}

// Subagents returns child-agent-only profiles, not foreground modes.
func Subagents() []Profile {
	return []Profile{
		profiles.Explorer(),
		profiles.Review(),
		profiles.Worker(),
	}
}

func (r *Registry) GetProfile(id string) (Profile, error) { return r.Get(id) }

func (r *Registry) Register(profile Profile) error {
	if profile.ID == "" || profile.Prompt == "" || profile.MaxTurns <= 0 {
		return errors.New("agent: ID, prompt, and positive max turns are required")
	}
	profile.HardRules = append([]string(nil), profile.HardRules...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.profiles[profile.ID]; exists {
		return fmt.Errorf("agent: duplicate profile %q", profile.ID)
	}
	r.profiles[profile.ID] = profile
	return nil
}

func (r *Registry) Get(id string) (Profile, error) {
	if id == "" {
		id = r.def
	}
	r.mu.RLock()
	profile, ok := r.profiles[id]
	if !ok && id == ExploreID {
		profile, ok = r.profiles[ExplorerID]
	}
	r.mu.RUnlock()
	if !ok {
		return Profile{}, fmt.Errorf("agent: unknown profile %q", id)
	}
	profile.HardRules = append([]string(nil), profile.HardRules...)
	return profile, nil
}

func (r *Registry) List() []Profile {
	r.mu.RLock()
	result := make([]Profile, 0, len(r.profiles))
	for _, profile := range r.profiles {
		profile.HardRules = append([]string(nil), profile.HardRules...)
		result = append(result, profile)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
