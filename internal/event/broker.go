package event

import (
	"context"
	"sync"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

// SessionHierarchy resolves a child session's direct parent.
type SessionHierarchy interface {
	ChildRelation(sessionID string) (parentSessionID string, ok bool)
}

// Broker is the application event boundary. It combines durable replay with
// transient delivery and projects descendant activity onto ancestor streams.
type Broker struct {
	durable   *Repository
	transient *TransientRepository
	hierarchy SessionHierarchy

	mu              sync.RWMutex
	watching        map[string]*Subscription
	observer        map[string]map[uint64]func(v1.Event)
	subtreeObserver map[string]map[uint64]func(v1.Event)
	next            uint64
	handler         func(BrokerEvent) func()
}

func NewBroker(durable *Repository, transient *TransientRepository, hierarchy ...SessionHierarchy) *Broker {
	if transient == nil {
		transient = NewTransientRepository()
	}
	var sessions SessionHierarchy
	if len(hierarchy) != 0 {
		sessions = hierarchy[0]
	}
	return &Broker{
		durable: durable, transient: transient, hierarchy: sessions,
		watching: make(map[string]*Subscription), observer: make(map[string]map[uint64]func(v1.Event)), subtreeObserver: make(map[string]map[uint64]func(v1.Event)),
	}
}

// ObserveSession projects future durable events from sessionID onto its
// ancestors. Observation is idempotent and lasts for the broker's lifetime.
func (b *Broker) ObserveSession(sessionID string) {
	if b == nil || b.durable == nil || sessionID == "" {
		return
	}
	b.mu.Lock()
	if b.watching[sessionID] != nil {
		b.mu.Unlock()
		return
	}
	subscription := b.durable.Subscribe(sessionID, 256)
	b.watching[sessionID] = subscription
	b.mu.Unlock()
	go b.forwardDurable(sessionID, subscription)
}

// ForgetSession stops projecting durable events from sessionID. It complements
// ObserveSession when prepared child-session admission is rolled back.
func (b *Broker) ForgetSession(sessionID string) {
	if b == nil || sessionID == "" {
		return
	}
	b.mu.Lock()
	subscription := b.watching[sessionID]
	delete(b.watching, sessionID)
	b.mu.Unlock()
	if subscription != nil {
		subscription.Close()
	}
}

func (b *Broker) forwardDurable(sessionID string, subscription *Subscription) {
	for item := range subscription.Events {
		event := v1.Event{ID: item.ID, Type: item.Type, SessionID: item.SessionID, Data: item.Data}
		b.project(event)
		b.notify(event.SessionID, event)
		b.notifySubtree(event)
	}
}

func (b *Broker) SetSessionHierarchy(hierarchy SessionHierarchy) {
	b.mu.Lock()
	b.hierarchy = hierarchy
	b.mu.Unlock()
}

// ObserveTransient receives transient events produced directly by sessionID.
// It is used for subagent accounting without coupling the event package to it.
func (b *Broker) ObserveTransient(sessionID string, observer func(v1.Event)) func() {
	if b == nil {
		return func() {}
	}
	return b.observeTransient(b.observer, sessionID, observer)
}

// ObserveTransientSubtree receives transient events produced by sessionID or
// any of its descendants according to the hierarchy at delivery time.
func (b *Broker) ObserveTransientSubtree(sessionID string, observer func(v1.Event)) func() {
	if b == nil {
		return func() {}
	}
	return b.observeTransient(b.subtreeObserver, sessionID, observer)
}

func (b *Broker) observeTransient(observersBySession map[string]map[uint64]func(v1.Event), sessionID string, observer func(v1.Event)) func() {
	if b == nil || observer == nil {
		return func() {}
	}
	b.mu.Lock()
	id := b.next
	b.next++
	if observersBySession[sessionID] == nil {
		observersBySession[sessionID] = make(map[uint64]func(v1.Event))
	}
	observersBySession[sessionID][id] = observer
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(observersBySession[sessionID], id)
			if len(observersBySession[sessionID]) == 0 {
				delete(observersBySession, sessionID)
			}
			b.mu.Unlock()
		})
	}
}

func (b *Broker) notify(sessionID string, item v1.Event) {
	b.mu.RLock()
	observers := snapshotObservers(b.observer[sessionID])
	b.mu.RUnlock()
	notifyObservers(observers, item)
}

func (b *Broker) notifySubtree(item v1.Event) {
	for _, sessionID := range b.ancestry(item.SessionID) {
		b.mu.RLock()
		observers := snapshotObservers(b.subtreeObserver[sessionID])
		b.mu.RUnlock()
		notifyObservers(observers, item)
	}
}

func snapshotObservers(registered map[uint64]func(v1.Event)) []func(v1.Event) {
	observers := make([]func(v1.Event), 0, len(registered))
	for _, observer := range registered {
		observers = append(observers, observer)
	}
	return observers
}

func notifyObservers(observers []func(v1.Event), item v1.Event) {
	for _, observer := range observers {
		observer(item)
	}
}

// Subscribe subscribes to transient events without durable replay.
func (b *Broker) Subscribe(sessionID string, capacity int) (<-chan v1.Event, func()) {
	return b.transient.Subscribe(sessionID, capacity)
}

// SetEventHandler installs the synchronous handler for predefined domain
// events. It is configured once during application assembly.
func (b *Broker) SetEventHandler(handler func(BrokerEvent) func()) {
	b.mu.Lock()
	b.handler = handler
	b.mu.Unlock()
}

// Publish implements EventBroker.
func (b *Broker) Publish(item BrokerEvent) func() {
	if b == nil || !item.Name.Valid() {
		return func() {}
	}
	b.mu.RLock()
	handler := b.handler
	b.mu.RUnlock()
	if handler == nil {
		return func() {}
	}
	stop := handler(item)
	if stop == nil {
		return func() {}
	}
	return stop
}

// PublishProtocol maps a canonical runner event into the API event stream.
func (b *Broker) PublishProtocol(sessionID string, item protocol.Event) {
	if b == nil {
		return
	}
	event, ok := protocolEvent(sessionID, item)
	if ok {
		b.PublishEvent(event)
	}
}

// PublishEvent publishes to the event's own session and every ancestor stream.
func (b *Broker) PublishEvent(item v1.Event) {
	if b == nil {
		return
	}
	b.transient.PublishEvent(item)
	b.project(item)
	b.notify(item.SessionID, item)
	b.notifySubtree(item)
}

func (b *Broker) project(item v1.Event) {
	for _, parent := range b.ancestry(item.SessionID)[1:] {
		projected := item
		projected.ID = ""
		projected.Sequence = nil
		projected.CreatedAt = nil
		b.transient.publishProjection(parent, projected)
	}
}

func (b *Broker) ancestry(origin string) []string {
	route := []string{origin}
	seen := map[string]bool{origin: true}
	for current := origin; current != ""; {
		b.mu.RLock()
		hierarchy := b.hierarchy
		b.mu.RUnlock()
		if hierarchy == nil {
			break
		}
		parent, ok := hierarchy.ChildRelation(current)
		if !ok || parent == "" || seen[parent] {
			break
		}
		route = append(route, parent)
		seen[parent] = true
		current = parent
	}
	return route
}

// Stream combines the durable replay/continuation and transient subscription.
type Stream struct {
	Replay    []v1.Event
	Durable   <-chan v1.Event
	Transient <-chan v1.Event
	close     func()
	once      sync.Once
}

func (s *Stream) Close() {
	if s != nil {
		s.once.Do(s.close)
	}
}

func (b *Broker) ReplayAndSubscribe(ctx context.Context, sessionID string, after int64, capacity int) (*Stream, error) {
	live, closeLive := b.transient.Subscribe(sessionID, capacity)
	if b.durable == nil {
		return &Stream{Transient: live, close: closeLive}, nil
	}
	replay, subscription, err := b.durable.ReplayAndSubscribe(ctx, sessionID, after, capacity)
	if err != nil {
		closeLive()
		return nil, err
	}
	durable := make(chan v1.Event)
	stop := make(chan struct{})
	go func() {
		defer close(durable)
		for {
			select {
			case item, ok := <-subscription.Events:
				if !ok {
					return
				}
				select {
				case durable <- b.durableEvent(item):
				case <-stop:
					return
				}
			case <-stop:
				return
			}
		}
	}()
	converted := make([]v1.Event, len(replay))
	for i, item := range replay {
		converted[i] = b.durableEvent(item)
	}
	return &Stream{Replay: converted, Durable: durable, Transient: live, close: func() {
		close(stop)
		subscription.Close()
		closeLive()
	}}, nil
}

func (b *Broker) durableEvent(item Event) v1.Event {
	sequence, createdAt := item.Sequence, item.CreatedAt
	return v1.Event{ID: item.ID, Type: item.Type, SessionID: item.SessionID, Sequence: &sequence, Data: item.Data, CreatedAt: &createdAt}
}
