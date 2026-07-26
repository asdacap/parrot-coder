// Package systemcontext observes typed, durable provider context.
package systemcontext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/session"
)

type Source interface {
	Key() string
	Observe(context.Context) (Observation, error)
}

type Observation struct {
	Available bool            `json:"available"`
	Value     json.RawMessage `json:"value,omitempty"`
	Path      string          `json:"path,omitempty"`
	Baseline  string          `json:"-"`
	Update    string          `json:"-"`
	Removal   string          `json:"removal,omitempty"`
}

type Snapshot map[string]Observation

type Registry struct {
	mu      sync.RWMutex
	sources map[string]Source
}

func NewRegistry(sources ...Source) (*Registry, error) {
	r := &Registry{sources: make(map[string]Source)}
	for _, source := range sources {
		if err := r.Register(source); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) Register(source Source) error {
	if source == nil || !validKey(source.Key()) {
		return errors.New("systemcontext: source requires a stable namespaced key")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sources[source.Key()]; exists {
		return fmt.Errorf("systemcontext: duplicate source %q", source.Key())
	}
	r.sources[source.Key()] = source
	return nil
}

func validKey(key string) bool {
	namespace, name, ok := strings.Cut(key, ":")
	return ok && namespace != "" && name != "" && !strings.ContainsAny(key, "\r\n\t ")
}

func (r *Registry) Observe(ctx context.Context) (Snapshot, error) {
	r.mu.RLock()
	keys := make([]string, 0, len(r.sources))
	sources := make(map[string]Source, len(r.sources))
	for key, source := range r.sources {
		keys = append(keys, key)
		sources[key] = source
	}
	r.mu.RUnlock()
	sort.Strings(keys)

	type result struct {
		key         string
		observation Observation
		err         error
	}
	results := make(chan result, len(keys))
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func(key string, source Source) {
			defer wg.Done()
			observation, err := source.Observe(ctx)
			if err == nil && observation.Available && len(observation.Value) != 0 && !json.Valid(observation.Value) {
				err = errors.New("invalid observation JSON")
			}
			results <- result{key, observation, err}
		}(key, sources[key])
	}
	wg.Wait()
	close(results)
	snapshot := make(Snapshot, len(keys))
	observed := make(map[string]result, len(keys))
	for result := range results {
		observed[result.key] = result
	}
	var failures []error
	for _, key := range keys {
		result := observed[key]
		if result.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", result.key, result.err))
			result.observation.Available = false
		}
		snapshot[result.key] = result.observation
	}
	return snapshot, errors.Join(failures...)
}

type EpochStore interface {
	GetSession(string) session.UserSession
}

type Manager struct {
	Registry *Registry
	Store    EpochStore
}

// FullContext is a freshly observed, complete typed context snapshot. It is
// suitable for a new epoch baseline; unlike reconciliation it never carries
// unavailable values forward from an older observation.
type FullContext struct {
	Baseline string
	Sources  json.RawMessage
}

func (m Manager) ObserveFull(ctx context.Context) (FullContext, error) {
	if m.Registry == nil {
		return FullContext{}, errors.New("systemcontext: registry is unavailable")
	}
	snapshot, observeErr := m.Registry.Observe(ctx)
	if err := completeSnapshot(snapshot, observeErr); err != nil {
		return FullContext{}, err
	}
	raw, err := encodeSnapshot(snapshot)
	if err != nil {
		return FullContext{}, err
	}
	return FullContext{Baseline: renderBaseline(snapshot), Sources: raw}, nil
}

func (m Manager) Initialize(ctx context.Context, sessionID string, cutoff int64) (session.ContextEpoch, error) {
	full, err := m.ObserveFull(ctx)
	if err != nil {
		return session.ContextEpoch{}, err
	}
	return m.Store.GetSession(sessionID).InitializeContext(ctx, full.Baseline, full.Sources, cutoff)
}

func (m Manager) Reconcile(ctx context.Context, sessionID string) (session.ContextEpoch, error) {
	userSession := m.Store.GetSession(sessionID)
	epoch, err := userSession.CurrentContextEpoch(ctx)
	if err != nil {
		return session.ContextEpoch{}, err
	}
	var previous Snapshot
	if err := json.Unmarshal(epoch.Sources, &previous); err != nil {
		return session.ContextEpoch{}, err
	}
	current, observeErr := m.Registry.Observe(ctx)
	next := cloneSnapshot(previous)
	var changes []string
	keys := make([]string, 0, len(current)+len(previous))
	for key := range current {
		keys = append(keys, key)
	}
	for key := range previous {
		if _, registered := current[key]; !registered {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		observed, registered := current[key]
		old, existed := previous[key]
		if !registered {
			if len(old.Value) != 0 {
				changes = appendNonempty(changes, old.Removal)
			}
			delete(next, key)
			continue
		}
		if !observed.Available {
			continue
		}
		if len(observed.Value) == 0 {
			if existed && len(old.Value) != 0 {
				changes = appendNonempty(changes, old.Removal)
			}
			delete(next, key)
			continue
		}
		if !existed {
			changes = appendNonempty(changes, observed.Baseline)
		} else if !bytes.Equal(old.Value, observed.Value) {
			changes = appendNonempty(changes, observed.Update)
		}
		next[key] = observed
	}
	raw, err := encodeSnapshot(next)
	if err != nil {
		return session.ContextEpoch{}, err
	}
	if bytes.Equal(raw, epoch.Sources) {
		return epoch, observeErr
	}
	if err := userSession.ReconcileContext(ctx, strings.Join(changes, "\n\n"), raw); err != nil {
		return session.ContextEpoch{}, err
	}
	epoch.Sources = raw
	return epoch, observeErr
}

func (m Manager) Replace(ctx context.Context, sessionID, baseline string, cutoff int64) (session.ContextEpoch, error) {
	full, err := m.ObserveFull(ctx)
	if err != nil {
		return session.ContextEpoch{}, err
	}
	return m.Store.GetSession(sessionID).ReplaceContext(ctx, baseline, full.Sources, cutoff)
}

func completeSnapshot(snapshot Snapshot, observeErr error) error {
	var unavailable []error
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		observation := snapshot[key]
		if !observation.Available {
			unavailable = append(unavailable, fmt.Errorf("%s: source unavailable", key))
		}
	}
	return errors.Join(append([]error{observeErr}, unavailable...)...)
}

func renderBaseline(snapshot Snapshot) string {
	keys := make([]string, 0, len(snapshot))
	for key := range snapshot {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var sections []string
	for _, key := range keys {
		observation := snapshot[key]
		if observation.Available && len(observation.Value) != 0 {
			sections = appendNonempty(sections, observation.Baseline)
		}
	}
	return strings.Join(sections, "\n\n")
}

func encodeSnapshot(snapshot Snapshot) (json.RawMessage, error) {
	raw, err := json.Marshal(snapshot)
	return json.RawMessage(raw), err
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	result := make(Snapshot, len(snapshot))
	for key, value := range snapshot {
		value.Value = append(json.RawMessage(nil), value.Value...)
		result[key] = value
	}
	return result
}

func appendNonempty(values []string, value string) []string {
	if strings.TrimSpace(value) != "" {
		return append(values, value)
	}
	return values
}
