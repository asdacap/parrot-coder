// Package permission prompts for operations the sandbox cannot contain.
package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"sync"
)

type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
)

// Choice is one answer a requesting tool offers for its own authorization. A
// tool which grants a lasting capability can thereby narrow how it may be
// answered.
type Choice struct {
	Value          string `json:"value"`
	Decision       string `json:"decision"`
	Label          string `json:"label"`
	Description    string `json:"description,omitempty"`
	RequiresReason bool   `json:"requires_reason,omitempty"`
}

// Request contains authorization data and a human-facing description. Callers
// must not put credentials or file contents in Description or Review.
//
// Description, Choices, and CanonicalInput are filled by the tool executor from
// the surrounding plan, so a tool constructs only its identity and review data.
type Request struct {
	ToolID         string          `json:"tool_id"`
	Description    string          `json:"description,omitempty"`
	CanonicalInput json.RawMessage `json:"canonical_input"`
	Review         json.RawMessage `json:"review,omitempty"`
	Choices        []Choice        `json:"choices,omitempty"`
	SessionID      string          `json:"-"`
	Workspace      string          `json:"-"`
}

func NewRequest(toolID string, review json.RawMessage) (Request, error) {
	if toolID == "" {
		return Request{}, errors.New("tool ID is required")
	}
	return Request{ToolID: toolID, Review: review}, nil
}

type Pending struct {
	ID      string
	Request Request
}

type Reply struct {
	Decision Decision
	Reason   string
}

type Prompter interface {
	Prompt(context.Context, Pending) (Reply, error)
}

type pendingState struct {
	pending Pending
	result  chan Reply
}

// Broker keeps pending requests in memory. Nothing is remembered once a request
// settles: every prompt authorizes exactly the call which raised it.
type Broker struct {
	mu             sync.Mutex
	noninteractive bool
	prompter       Prompter
	pending        map[string]*pendingState
}

func NewBroker(noninteractive bool, prompter Prompter) *Broker {
	return &Broker{noninteractive: noninteractive, prompter: prompter, pending: make(map[string]*pendingState)}
}

// Authorize returns the effective decision, which is deny when no one can be
// asked.
func (b *Broker) Authorize(ctx context.Context, request Request) (Decision, error) {
	if b.noninteractive {
		return Deny, nil
	}
	state, err := b.addPending(request)
	if err != nil {
		return Deny, err
	}
	if b.prompter != nil {
		go func() {
			reply, promptErr := b.prompter.Prompt(ctx, state.pending)
			if promptErr != nil {
				_ = b.Reject(state.pending.ID)
				return
			}
			_ = b.reply(state.pending.ID, reply)
		}()
	}
	select {
	case reply := <-state.result:
		if reply.Decision == Deny && reply.Reason != "" {
			return Deny, errors.New(reply.Reason)
		}
		return reply.Decision, nil
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, state.pending.ID)
		b.mu.Unlock()
		return Deny, ctx.Err()
	}
}

func (b *Broker) Pending() []Pending {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Pending, 0, len(b.pending))
	for _, state := range b.pending {
		out = append(out, state.pending)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (b *Broker) ReplyOnce(id string) error { return b.reply(id, Reply{Decision: Allow}) }
func (b *Broker) Reject(id string) error    { return b.reply(id, Reply{Decision: Deny}) }
func (b *Broker) RejectWithReason(id, reason string) error {
	return b.reply(id, Reply{Decision: Deny, Reason: reason})
}

func (b *Broker) addPending(request Request) (*pendingState, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	state := &pendingState{Pending{hex.EncodeToString(idBytes), request}, make(chan Reply, 1)}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending[state.pending.ID] = state
	return state, nil
}

func (b *Broker) reply(id string, reply Reply) error {
	if reply.Decision != Allow && reply.Decision != Deny {
		return errors.New("invalid permission reply decision")
	}
	b.mu.Lock()
	state, ok := b.pending[id]
	if !ok {
		b.mu.Unlock()
		return errors.New("permission request is unknown or already settled")
	}
	delete(b.pending, id)
	b.mu.Unlock()
	state.result <- reply
	return nil
}
