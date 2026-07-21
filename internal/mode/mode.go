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
// they are workers which a mode may invoke through the reusable agent tools.
type Mode interface {
	ID() string
	Profile() agent.Profile
	// OnTurnComplete declares what the runtime should do after a turn in this
	// mode completes. The zero value means "do nothing." A mode may present a
	// Dialog for the user to choose, or directly transition without a dialog.
	// The mode owns this behavior; callers must not branch on the mode's ID.
	OnTurnComplete() TurnCompleteResult
}

// TurnCompleteResult is what a mode wants the runtime to do after a turn
// completes. The zero value means "do nothing."
type TurnCompleteResult struct {
	// Dialog presents choices to the user. When nil, the runtime performs
	// the result's Agent and Prompt fields directly.
	Dialog *TurnCompleteDialog
	// Agent switches the session to this mode. Empty means stay.
	Agent string
	// Prompt is injected as the next user message. Empty means no prompt.
	Prompt string
}

// TurnCompleteDialog describes a choice prompt shown after a turn completes.
// Each choice carries its own action; the runtime performs the selected
// choice's action.
type TurnCompleteDialog struct {
	Prompt            string
	Context           []string
	Choices           []DialogChoice
	CustomChoice      string
	CustomDescription string
	CustomPrompt      string
	// EmptyMessage is the validation error shown when the user submits an
	// empty response.
	EmptyMessage string
}

// DialogChoice is one selectable option in a turn-complete dialog.
type DialogChoice struct {
	Value       string
	Description string
	// Aliases are additional accepted values (case-insensitive) for typed
	// input, e.g. "y" for "yes".
	Aliases []string
	// Action describes what the runtime does when this choice is selected.
	// An empty action stops the run.
	Action ChoiceAction
}

// ChoiceAction describes what the runtime does when a dialog choice is
// selected: switch to Agent (if non-empty), inject Prompt (if non-empty).
// An empty action stops the run.
type ChoiceAction struct {
	Agent  string
	Prompt string
}

type builtin struct {
	profile      agent.Profile
	turnComplete TurnCompleteResult
}

func (m builtin) ID() string                       { return m.profile.ID }
func (m builtin) Profile() agent.Profile           { return m.profile }
func (m builtin) OnTurnComplete() TurnCompleteResult { return m.turnComplete }

func Builtins() []Mode {
	return []Mode{
		builtin{profile: agent.Profile{ID: BuildID, Prompt: "You are Parrot's build mode. Implement and verify the requested changes.", HardRules: []string{"Keep tool side effects within the authorized workspace."}, MaxTurns: 64, RecursionLimit: 3}},
		builtin{
			profile: agent.Profile{ID: PlanID, Prompt: "You are Parrot's plan mode. Inspect the project and produce an implementation plan.", HardRules: []string{"Read-only mode is enforced by the runtime."}, MaxTurns: 24, RecursionLimit: 1, ReadOnly: true},
			turnComplete: TurnCompleteResult{Dialog: &TurnCompleteDialog{
				Prompt:            "Plan complete: ",
				Context:           []string{"Review the plan before implementation."},
				Choices: []DialogChoice{
					{Value: "yes", Description: "Implement the approved plan", Aliases: []string{"y"}, Action: ChoiceAction{Agent: BuildID, Prompt: "Implement the approved plan."}},
					{Value: "no", Description: "Stop after planning", Aliases: []string{"n"}},
				},
				CustomChoice:      "feedback",
				CustomDescription: "Provide feedback and revise the plan",
				CustomPrompt:      "plan feedback: ",
				EmptyMessage:      "enter yes, no, or feedback",
			}},
		},
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
