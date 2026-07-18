// Package mode defines the foreground operating modes exposed by Parrot.
package mode

import (
	"errors"
	"fmt"
	"sort"

	"github.com/amirulashraf/parrot-coder/internal/agent"
)

const (
	BuildID = "build"
	PlanID  = "plan"
)

// Mode is a foreground execution policy. Agents are deliberately separate:
// they are workers which a mode may invoke through the task tool.
type Mode interface {
	ID() string
	Profile() agent.Profile
}

type builtin struct {
	profile agent.Profile
}

func (m builtin) ID() string             { return m.profile.ID }
func (m builtin) Profile() agent.Profile { return m.profile }

func Builtins() []Mode {
	readTools := []string{"agent_interrupt", "agent_list", "agent_send", "agent_spawn", "agent_wait", "get_goal", "glob", "git_diff", "grep", "read", "read_output", "review", "skill", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols", "task", "task_status", "task_cancel", "todoread"}
	return []Mode{
		builtin{profile: agent.Profile{ID: BuildID, Prompt: "You are Parrot's build mode. Implement and verify the requested changes.", HardRules: []string{"Keep tool side effects within the authorized workspace."}, MaxTurns: 64}},
		builtin{profile: agent.Profile{ID: PlanID, Prompt: "You are Parrot's plan mode. Inspect the project and produce an implementation plan.", AllowedToolIDs: readTools, HardRules: []string{"Read-only mode is enforced by the runtime."}, MaxTurns: 24, ReadOnly: true}},
	}
}

type Registry struct{ items map[string]Mode }

func NewRegistry(modes ...Mode) (*Registry, error) {
	if len(modes) == 0 {
		modes = Builtins()
	}
	r := &Registry{items: make(map[string]Mode, len(modes))}
	for _, item := range modes {
		if item == nil || item.ID() == "" || item.Profile().ID != item.ID() {
			return nil, errors.New("mode: valid ID and matching profile are required")
		}
		if _, exists := r.items[item.ID()]; exists {
			return nil, fmt.Errorf("mode: duplicate mode %q", item.ID())
		}
		r.items[item.ID()] = item
	}
	return r, nil
}

func (r *Registry) Get(id string) (Mode, error) {
	if id == "" {
		id = BuildID
	}
	item, ok := r.items[id]
	if !ok {
		return nil, fmt.Errorf("mode: unknown mode %q", id)
	}
	return item, nil
}

func (r *Registry) List() []Mode {
	result := make([]Mode, 0, len(r.items))
	for _, item := range r.items {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID() < result[j].ID() })
	return result
}

func (r *Registry) GetProfile(id string) (agent.Profile, error) {
	item, err := r.Get(id)
	if err != nil {
		return agent.Profile{}, err
	}
	return item.Profile(), nil
}
