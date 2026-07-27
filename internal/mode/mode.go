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
	"github.com/amirulashraf/parrot-coder/internal/event"
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
	OnTurnStart(string) (agent.TurnProfile, error)
	OnTurnFinish(string, string) ([]event.BrokerEvent, error)
}

type builtin struct {
	profile agent.Profile
}

type profileWithPrompt struct {
	agent.Profile
	prompt string
}

func (p profileWithPrompt) Prompt() string { return p.prompt }

func (m builtin) ID() string             { return m.profile.ID() }
func (m builtin) Profile() agent.Profile { return m.profile }
func (m builtin) OnTurnStart(string) (agent.TurnProfile, error) {
	return agent.NewTurnProfile(m.profile), nil
}
func (builtin) OnTurnFinish(string, string) ([]event.BrokerEvent, error) { return nil, nil }

// planMode extends builtin with a turn-complete dialog that lets the user
// approve, decline, or provide feedback on the plan.
type planMode struct {
	builtin
	directory string
	mu        sync.Mutex
	files     map[string]string
}

func planCompletion() event.TurnCompleteDialog {
	return event.TurnCompleteDialog{
		Prompt:  "Plan complete: ",
		Context: []string{"Review the plan before implementation."},
		Choices: []event.DialogChoice{
			{Value: "yes", Description: "Implement the approved plan", Aliases: []string{"y"}, Action: event.ChoiceAction{Agent: BuildID, Prompt: "Implement the approved plan."}},
			{Value: "no", Description: "Stop after planning", Aliases: []string{"n"}},
		},
		CustomChoice:      "feedback",
		CustomDescription: "Provide feedback and revise the plan",
		CustomPrompt:      "plan feedback: ",
		EmptyMessage:      "enter yes, no, or feedback",
	}
}

func (m *planMode) OnTurnStart(sessionID string) (agent.TurnProfile, error) {
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
		prompt:  m.profile.Prompt() + "\n\nWrite the complete implementation plan as Markdown to this exact file: " + path + ". Do not include the plan in your assistant response. Finish only after writing the file.",
	}
	return agent.NewTurnProfile(profile, security.Rule{Path: path, Action: security.ActionAllowWrite}), nil
}

func (m *planMode) OnTurnFinish(sessionID, messageID string) ([]event.BrokerEvent, error) {
	m.mu.Lock()
	path := m.files[sessionID]
	m.mu.Unlock()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mode: read plan: %w", err)
	}
	plan := strings.TrimSpace(string(data))
	if plan == "" {
		return nil, nil
	}
	return []event.BrokerEvent{{Name: event.PlanCompleted, Payload: event.PlanCompletedPayload{
		SessionID: sessionID, MessageID: messageID, Markdown: plan, Dialog: planCompletion(),
	}}}, nil
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

func (r *Registry) GetProfile(id string) (agent.Profile, error) {
	item, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	return item.Profile(), nil
}
