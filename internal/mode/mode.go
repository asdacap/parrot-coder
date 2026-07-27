// Package mode defines the foreground operating modes exposed by Parrot.
package mode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	"github.com/amirulashraf/parrot-coder/internal/security"
)

const (
	BuildID = "build"
	PlanID  = "plan"
	QueryID = "query"
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
	PrepareTurn(string) (agent.TurnProfile, error)
	CompleteTurn(string, string) (TurnCompleteResult, error)
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
	Markdown          string
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
	profile agent.Profile
}

type profileWithPrompt struct {
	agent.Profile
	prompt string
}

func (p profileWithPrompt) Prompt() string { return p.prompt }

func (m builtin) ID() string                       { return m.profile.ID() }
func (m builtin) Profile() agent.Profile           { return m.profile }
func (builtin) OnTurnComplete() TurnCompleteResult { return TurnCompleteResult{} }
func (m builtin) PrepareTurn(string) (agent.TurnProfile, error) {
	return agent.NewTurnProfile(m.profile), nil
}
func (builtin) CompleteTurn(string, string) (TurnCompleteResult, error) {
	return TurnCompleteResult{}, nil
}

// planMode extends builtin with a turn-complete dialog that lets the user
// approve, decline, or provide feedback on the plan.
type planMode struct {
	builtin
	directory string
	mu        sync.Mutex
	files     map[string]string
}

func planCompletion(markdown string) TurnCompleteResult {
	return TurnCompleteResult{Dialog: &TurnCompleteDialog{
		Markdown: markdown,
		Prompt:   "Plan complete: ",
		Context:  []string{"Review the plan before implementation."},
		Choices: []DialogChoice{
			{Value: "yes", Description: "Implement the approved plan", Aliases: []string{"y"}, Action: ChoiceAction{Agent: BuildID, Prompt: "Implement the approved plan."}},
			{Value: "no", Description: "Stop after planning", Aliases: []string{"n"}},
		},
		CustomChoice:      "feedback",
		CustomDescription: "Provide feedback and revise the plan",
		CustomPrompt:      "plan feedback: ",
		EmptyMessage:      "enter yes, no, or feedback",
	}}
}

func (m *planMode) OnTurnComplete() TurnCompleteResult { return planCompletion("") }

func (m *planMode) PrepareTurn(sessionID string) (agent.TurnProfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.directory, 0o700); err != nil {
		return agent.TurnProfile{}, fmt.Errorf("mode: create plan directory: %w", err)
	}
	path := m.files[sessionID]
	if path == "" {
		file, err := os.CreateTemp(m.directory, "plan-*.md")
		if err != nil {
			return agent.TurnProfile{}, fmt.Errorf("mode: create plan file: %w", err)
		}
		path = file.Name()
		if err = file.Close(); err != nil {
			return agent.TurnProfile{}, err
		}
		m.files[sessionID] = path
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return agent.TurnProfile{}, fmt.Errorf("mode: make plan file writable: %w", err)
	}
	profile := profileWithPrompt{
		Profile: m.profile,
		prompt:  m.profile.Prompt() + "\n\nWrite the complete implementation plan as Markdown to this exact canonical file: " + path + ". You may write optional supporting artifacts under this plan directory and reference them from the canonical plan: " + m.directory + ". Do not include the plan in your assistant response. Finish only after writing the canonical file.",
	}
	return agent.NewTurnProfile(profile, security.Rule{Path: m.directory, Action: security.ActionAllowWrite}), nil
}

func (m *planMode) CompleteTurn(sessionID, _ string) (TurnCompleteResult, error) {
	m.mu.Lock()
	path := m.files[sessionID]
	m.mu.Unlock()
	if path == "" {
		return TurnCompleteResult{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return TurnCompleteResult{}, fmt.Errorf("mode: read plan: %w", err)
	}
	plan := strings.TrimSpace(string(data))
	if plan == "" {
		return TurnCompleteResult{}, nil
	}
	return planCompletion(plan), nil
}

func Builtins(profiles ...agent.Profile) []Mode {
	return BuiltinsWithPlanDirectory(filepath.Join(os.TempDir(), "parrot-plans"), profiles...)
}

func BuiltinsWithPlanDirectory(directory string, profiles ...agent.Profile) []Mode {
	items := make([]Mode, 0, len(profiles))
	for _, profile := range profiles {
		if profile != nil && profile.ID() == PlanID {
			items = append(items, &planMode{builtin: builtin{profile: profile}, directory: directory, files: make(map[string]string)})
			continue
		}
		items = append(items, builtin{profile: profile})
	}
	return items
}

type Registry struct{ items map[string]Mode }

func NewRegistry(modes ...Mode) (*Registry, error) {
	r := &Registry{items: make(map[string]Mode, len(modes))}
	for _, item := range modes {
		if item == nil {
			return nil, errors.New("mode: valid ID and matching profile are required")
		}
		profile := item.Profile()
		if nilProfile(profile) || item.ID() == "" || profile.ID() != item.ID() {
			return nil, errors.New("mode: valid ID and matching profile are required")
		}
		if _, exists := r.items[item.ID()]; exists {
			return nil, fmt.Errorf("mode: duplicate mode %q", item.ID())
		}
		r.items[item.ID()] = item
	}
	return r, nil
}

func nilProfile(profile agent.Profile) bool {
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

func NewRegistryWithPlanDirectory(directory string, profiles ...agent.Profile) (*Registry, error) {
	return NewRegistry(BuiltinsWithPlanDirectory(directory, profiles...)...)
}

func (r *Registry) Get(id string) (Mode, error) {
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

func (r *Registry) PrepareTurn(id, sessionID string) (agent.TurnProfile, error) {
	item, err := r.Get(id)
	if err != nil {
		return agent.TurnProfile{}, err
	}
	return item.PrepareTurn(sessionID)
}

func (r *Registry) CompleteTurn(id, sessionID, messageID string) (TurnCompleteResult, error) {
	item, err := r.Get(id)
	if err != nil {
		return TurnCompleteResult{}, err
	}
	return item.CompleteTurn(sessionID, messageID)
}

func (r *Registry) GetProfile(id string) (agent.Profile, error) {
	item, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	return item.Profile(), nil
}
