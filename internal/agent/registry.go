// Package agent owns deterministic agent profiles and session execution.
package agent

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

const (
	BuildID   = "build"
	PlanID    = "plan"
	ExploreID = "explore"
)

type Profile struct {
	ID             string
	Prompt         string
	AllowedToolIDs []string
	HardRules      []string
	MaxTurns       int
	ReadOnly       bool
}

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
	readTools := []string{"glob", "grep", "read", "read_output", "skill", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols", "task", "task_status", "task_cancel"}
	return []Profile{
		{ID: BuildID, Prompt: "You are Parrot's build agent. Implement and verify the requested changes.", HardRules: []string{"Keep tool side effects within the authorized workspace."}, MaxTurns: 64},
		{ID: PlanID, Prompt: "You are Parrot's planning agent. Inspect the project and produce an implementation plan.", AllowedToolIDs: readTools, HardRules: []string{"Read-only mode is enforced by the runtime."}, MaxTurns: 24, ReadOnly: true},
		{ID: ExploreID, Prompt: "You are Parrot's exploration agent. Investigate the project and report evidence.", AllowedToolIDs: readTools, HardRules: []string{"Read-only mode is enforced by the runtime."}, MaxTurns: 32, ReadOnly: true},
	}
}

func (r *Registry) Register(profile Profile) error {
	if profile.ID == "" || profile.Prompt == "" || profile.MaxTurns <= 0 {
		return errors.New("agent: ID, prompt, and positive max turns are required")
	}
	profile.AllowedToolIDs = sortedUnique(profile.AllowedToolIDs)
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
	r.mu.RUnlock()
	if !ok {
		return Profile{}, fmt.Errorf("agent: unknown profile %q", id)
	}
	profile.AllowedToolIDs = append([]string(nil), profile.AllowedToolIDs...)
	profile.HardRules = append([]string(nil), profile.HardRules...)
	return profile, nil
}

func (r *Registry) List() []Profile {
	r.mu.RLock()
	result := make([]Profile, 0, len(r.profiles))
	for _, profile := range r.profiles {
		result = append(result, profile)
	}
	r.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (p Profile) AllowsTool(id string) bool {
	if p.ReadOnly && !readOnlyTool(id) {
		return false
	}
	if len(p.AllowedToolIDs) == 0 {
		return true
	}
	i := sort.SearchStrings(p.AllowedToolIDs, id)
	return i < len(p.AllowedToolIDs) && p.AllowedToolIDs[i] == id
}

func readOnlyTool(id string) bool {
	switch id {
	case "read", "glob", "grep", "read_output", "skill", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols", "task", "task_status", "task_cancel":
		return true
	default:
		return false
	}
}

func sortedUnique(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	out := result[:0]
	for _, value := range result {
		if value != "" && (len(out) == 0 || out[len(out)-1] != value) {
			out = append(out, value)
		}
	}
	return out
}
