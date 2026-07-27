// Package agent owns deterministic agent profiles and session execution.
package agent

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/agent/profiles"
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/status"
)

const (
	ExploreID  = "explore"
	ExplorerID = profiles.ExplorerID
	ReviewID   = profiles.ReviewID
	WorkerID   = profiles.WorkerID
)

type Profile = profiles.Profile

func NewProfile(id, prompt, usage string, hardRules []string, maxTurns, recursionLimit int, readOnly bool, sandboxRules []security.Rule, statusProvider status.Provider) Profile {
	return profiles.New(id, prompt, usage, hardRules, maxTurns, recursionLimit, readOnly, sandboxRules, statusProvider)
}

// TurnProfile combines a reusable profile with capabilities granted while
// preparing one session's turn.
type TurnProfile struct {
	profile      Profile
	capabilities []security.Rule
}

func NewTurnProfile(profile Profile, capabilities ...security.Rule) TurnProfile {
	return TurnProfile{profile: profile, capabilities: append([]security.Rule(nil), capabilities...)}
}

func (p TurnProfile) Profile() Profile { return p.profile }
func (p TurnProfile) IsReadOnly() bool { return p.profile.IsReadOnly() }
func (p TurnProfile) Rules() []security.Rule {
	return append(p.BaseRules(), p.capabilities...)
}
func (p TurnProfile) BaseRules() []security.Rule { return p.profile.Rules() }
func (p TurnProfile) CapabilityRules() []security.Rule {
	return append([]security.Rule(nil), p.capabilities...)
}

type Registry struct {
	mu       sync.RWMutex
	profiles map[string]Profile
	def      string
}

func NewRegistry(profiles ...Profile) (*Registry, error) {
	if len(profiles) == 0 || nilProfile(profiles[0]) {
		return nil, errors.New("agent: at least one profile is required")
	}
	r := &Registry{profiles: make(map[string]Profile), def: profiles[0].ID()}
	for _, profile := range profiles {
		if err := r.Register(profile); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) GetProfile(id string) (Profile, error) { return r.Get(id) }

func (r *Registry) Register(profile Profile) error {
	if nilProfile(profile) {
		return errors.New("agent: ID, prompt, and positive max turns are required")
	}
	profile = NewProfile(profile.ID(), profile.Prompt(), profile.Usage(), profile.HardRules(), profile.MaxTurns(), profile.RecursionLimit(), profile.IsReadOnly(), profile.Rules(), profile.Status())
	if profile.ID() == "" || profile.Prompt() == "" || profile.MaxTurns() <= 0 {
		return errors.New("agent: ID, prompt, and positive max turns are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.profiles[profile.ID()]; exists {
		return fmt.Errorf("agent: duplicate profile %q", profile.ID())
	}
	r.profiles[profile.ID()] = profile
	return nil
}

func nilProfile(profile Profile) bool {
	if profile == nil {
		return true
	}
	value := reflect.ValueOf(profile)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
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
		return nil, fmt.Errorf("agent: unknown profile %q", id)
	}
	return profile, nil
}

func (r *Registry) List() []Profile {
	r.mu.RLock()
	result := make([]Profile, 0, len(r.profiles))
	for _, profile := range r.profiles {
		result = append(result, profile)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID() < result[j].ID() })
	return result
}
