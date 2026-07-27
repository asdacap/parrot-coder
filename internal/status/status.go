// Package status provides runtime information which agents can query without
// placing it in the system prompt.
package status

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/queue"
	"github.com/amirulashraf/parrot-coder/internal/task"
)

type Query struct {
	SessionID         string
	ParentSessionID   string
	ParentSessionName string
	Agent             string
	Model             string
}

type Observation struct {
	Available bool
	Text      string
}

type Provider interface {
	Key() string
	Observe(context.Context, Query) (Observation, error)
}

type Static struct {
	ProviderKey string
	Text        string
}

func (s Static) Key() string { return s.ProviderKey }
func (s Static) Observe(context.Context, Query) (Observation, error) {
	return Observation{Available: strings.TrimSpace(s.Text) != "", Text: s.Text}, nil
}

type activeTaskLister interface {
	ListActive(sessionID string) []task.Active
}

type ActiveTasks struct {
	tasks activeTaskLister
}

func NewActiveTasks(tasks activeTaskLister) ActiveTasks { return ActiveTasks{tasks: tasks} }

func (ActiveTasks) Key() string { return "runtime:tasks" }
func (p ActiveTasks) Observe(_ context.Context, query Query) (Observation, error) {
	var active []task.Active
	if p.tasks != nil {
		active = p.tasks.ListActive(query.SessionID)
	}
	agents, processes := partitionActiveTasks(active)
	return Observation{Available: true, Text: strings.Join([]string{
		formatActiveSubagents(agents),
		formatActiveProcesses(processes),
	}, "\n\n")}, nil
}

func partitionActiveTasks(active []task.Active) (agents, processes []task.Active) {
	for _, item := range active {
		if item.Kind == task.KindAgent {
			agents = append(agents, item)
		} else if item.Kind == task.KindShell {
			processes = append(processes, item)
		}
	}
	sort.Slice(agents, func(i, j int) bool { return activeIdentifier(agents[i]) < activeIdentifier(agents[j]) })
	sort.Slice(processes, func(i, j int) bool { return activeIdentifier(processes[i]) < activeIdentifier(processes[j]) })
	return agents, processes
}

func formatActiveSubagents(agents []task.Active) string {
	if len(agents) == 0 {
		return "Active subagents: none"
	}
	lines := make([]string, 1, len(agents)+1)
	lines[0] = "Active subagents:"
	for _, item := range agents {
		details := []string{item.Status}
		if item.Agent != "" {
			details = append(details, "agent: "+item.Agent)
		}
		details = append(details, fmt.Sprintf("turn: %d", item.Turn), fmt.Sprintf("depth: %d", item.Depth))
		lines = append(lines, fmt.Sprintf("- %s (%s)", activeIdentifier(item), strings.Join(details, ", ")))
	}
	return strings.Join(lines, "\n")
}

func formatActiveProcesses(processes []task.Active) string {
	if len(processes) == 0 {
		return "Active processes: none"
	}
	lines := make([]string, 1, len(processes)+1)
	lines[0] = "Active processes:"
	for _, item := range processes {
		lines = append(lines, fmt.Sprintf("- %s (%s)", activeIdentifier(item), item.Status))
	}
	return strings.Join(lines, "\n")
}

func activeIdentifier(item task.Active) string {
	if item.Kind == task.KindShell {
		return item.ProcessID
	}
	return item.SessionID
}

type queueLister interface {
	List() ([]queue.Info, error)
}

type Queues struct{ queues queueLister }

func NewQueues(queues queueLister) Queues { return Queues{queues: queues} }

func (Queues) Key() string { return "runtime:queues" }
func (p Queues) Observe(_ context.Context, _ Query) (Observation, error) {
	if p.queues == nil {
		return Observation{Available: true, Text: "Queues: none"}, nil
	}
	items, err := p.queues.List()
	if err != nil {
		return Observation{}, err
	}
	if len(items) == 0 {
		return Observation{Available: true, Text: "Queues: none"}, nil
	}
	lines := make([]string, 1, len(items)+1)
	lines[0] = "Queues:"
	for _, item := range items {
		line := fmt.Sprintf("- %s (%d items", item.Name, item.Size)
		if item.Description != "" {
			line += ", description: " + strconv.Quote(item.Description)
		}
		lines = append(lines, line+")")
	}
	return Observation{Available: true, Text: strings.Join(lines, "\n")}, nil
}

type Selection struct{}

func (Selection) Key() string { return "runtime:selection" }
func (Selection) Observe(_ context.Context, query Query) (Observation, error) {
	lines := []string{"Active profile: " + query.Agent, "Model: " + query.Model}
	if query.ParentSessionID != "" {
		parent := query.ParentSessionID
		if name := strings.TrimSpace(query.ParentSessionName); name != "" {
			parent += " (" + name + ")"
		}
		lines = append(lines, "Parent session: "+parent)
	}
	return Observation{Available: true, Text: strings.Join(lines, "\n")}, nil
}

type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, provider := range providers {
		if err := r.Register(provider); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// With returns an independent registry containing the current providers plus
// the supplied session-bound providers.
func (r *Registry) With(providers ...Provider) (*Registry, error) {
	if r == nil {
		return nil, errors.New("status: registry is unavailable")
	}
	r.mu.RLock()
	items := make([]Provider, 0, len(r.providers)+len(providers))
	for _, provider := range r.providers {
		items = append(items, provider)
	}
	r.mu.RUnlock()
	items = append(items, providers...)
	return NewRegistry(items...)
}

func (r *Registry) Register(provider Provider) error {
	if provider == nil || !validKey(provider.Key()) {
		return errors.New("status: provider requires a stable namespaced key")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[provider.Key()]; exists {
		return fmt.Errorf("status: duplicate provider %q", provider.Key())
	}
	r.providers[provider.Key()] = provider
	return nil
}

func validKey(key string) bool {
	namespace, name, ok := strings.Cut(key, ":")
	return ok && namespace != "" && name != "" && !strings.ContainsAny(key, "\r\n\t ")
}

// Observe evaluates all providers on every call and renders them by key.
func (r *Registry) Observe(ctx context.Context, query Query, profile Provider) (string, error) {
	if r == nil {
		return "", errors.New("status: registry is unavailable")
	}
	r.mu.RLock()
	providers := make(map[string]Provider, len(r.providers)+1)
	for key, provider := range r.providers {
		providers[key] = provider
	}
	r.mu.RUnlock()
	if profile != nil {
		if !validKey(profile.Key()) {
			return "", errors.New("status: profile provider requires a stable namespaced key")
		}
		if _, exists := providers[profile.Key()]; exists {
			return "", fmt.Errorf("status: duplicate provider %q", profile.Key())
		}
		providers[profile.Key()] = profile
	}
	keys := make([]string, 0, len(providers))
	for key := range providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sections := make([]string, 0, len(keys))
	for _, key := range keys {
		observation, err := providers[key].Observe(ctx, query)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		if observation.Available && strings.TrimSpace(observation.Text) != "" {
			sections = append(sections, observation.Text)
		}
	}
	return strings.Join(sections, "\n\n"), nil
}
