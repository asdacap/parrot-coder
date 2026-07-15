// Package permission authorizes canonical, hash-bound operations.
package permission

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

type Scope string

const (
	ScopeProcess   Scope = "process"
	ScopeSession   Scope = "session"
	ScopeWorkspace Scope = "workspace"
)

// Resource identifies an already-canonicalized object being accessed.
type Resource struct {
	Kind       string            `json:"kind"`
	Identifier string            `json:"identifier"`
	Operation  string            `json:"operation"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Request contains authorization data and a human-facing description. Callers
// must not put credentials or file contents in Description, Review, or resource
// attributes.
type Request struct {
	ToolID         string          `json:"tool_id"`
	Description    string          `json:"description,omitempty"`
	CanonicalInput json.RawMessage `json:"canonical_input"`
	Resources      []Resource      `json:"resources"`
	Review         json.RawMessage `json:"review,omitempty"`
	OperationHash  string          `json:"operation_hash"`
	SessionID      string          `json:"-"`
	Workspace      string          `json:"-"`
}

func NewRequest(toolID string, input json.RawMessage, resources []Resource, review json.RawMessage) (Request, error) {
	if toolID == "" {
		return Request{}, errors.New("tool ID is required")
	}
	canonicalInput, err := CanonicalJSON(input)
	if err != nil {
		return Request{}, fmt.Errorf("canonical input: %w", err)
	}
	canonicalReview, err := optionalCanonicalJSON(review)
	if err != nil {
		return Request{}, fmt.Errorf("review: %w", err)
	}
	r := Request{ToolID: toolID, CanonicalInput: canonicalInput, Resources: cloneResources(resources), Review: canonicalReview}
	r.OperationHash, err = Hash(r)
	return r, err
}

func (r Request) Verify() error {
	hash, err := Hash(r)
	if err != nil {
		return err
	}
	if r.OperationHash == "" || hash != r.OperationHash {
		return errors.New("operation hash mismatch")
	}
	return nil
}

// Hash computes SHA-256 over stable canonical JSON, excluding contextual scope
// IDs and the hash field itself.
func Hash(r Request) (string, error) {
	type operation struct {
		ToolID         string          `json:"tool_id"`
		Description    string          `json:"description,omitempty"`
		CanonicalInput json.RawMessage `json:"canonical_input"`
		Resources      []Resource      `json:"resources"`
		Review         json.RawMessage `json:"review,omitempty"`
	}
	resources := cloneResources(r.Resources)
	sort.Slice(resources, func(i, j int) bool {
		a, _ := json.Marshal(resources[i])
		b, _ := json.Marshal(resources[j])
		return bytes.Compare(a, b) < 0
	})
	b, err := json.Marshal(operation{r.ToolID, r.Description, r.CanonicalInput, resources, r.Review})
	if err != nil {
		return "", err
	}
	canonical, err := CanonicalJSON(b)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func CanonicalJSON(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return json.Marshal(value)
}

func optionalCanonicalJSON(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	return CanonicalJSON(raw)
}

func cloneResources(in []Resource) []Resource {
	out := make([]Resource, len(in))
	for i, resource := range in {
		out[i] = resource
		if resource.Attributes != nil {
			out[i].Attributes = make(map[string]string, len(resource.Attributes))
			for k, v := range resource.Attributes {
				out[i].Attributes[k] = v
			}
		}
	}
	return out
}

type MatchFunc func(Request) bool

type Rule struct {
	Match    MatchFunc
	Decision Decision
	HardDeny bool
	Reason   string
}

type Policy struct {
	Rules   []Rule
	Default Decision
}

func (p Policy) Evaluate(request Request) (Decision, string, bool) {
	decision := p.Default
	if decision == "" {
		decision = Ask
	}
	reason := "default policy"
	chosen := false
	for _, rule := range p.Rules {
		matches := rule.Match == nil || rule.Match(request)
		if !matches {
			continue
		}
		if rule.HardDeny || rule.Decision == Deny && rule.HardDeny {
			return Deny, rule.Reason, true
		}
		if !chosen {
			decision, reason, chosen = rule.Decision, rule.Reason, true
		}
	}
	return decision, reason, false
}

type Pending struct {
	ID      string
	Request Request
	Reason  string
}

type Reply struct {
	Decision Decision
	Scope    Scope
}

type Prompter interface {
	Prompt(context.Context, Pending) (Reply, error)
}

type grantKey struct{ hash, context string }
type pendingState struct {
	pending Pending
	result  chan Reply
}

// Broker keeps pending requests and remembered grants in memory. It contains no
// tool input or review data after a request settles.
type Broker struct {
	mu             sync.Mutex
	policy         Policy
	noninteractive bool
	prompter       Prompter
	pending        map[string]*pendingState
	grants         map[Scope]map[grantKey]struct{}
	yoloSessions   map[string]struct{}
}

func NewBroker(policy Policy, noninteractive bool, prompter Prompter) *Broker {
	return &Broker{policy: policy, noninteractive: noninteractive, prompter: prompter, pending: make(map[string]*pendingState), grants: map[Scope]map[grantKey]struct{}{ScopeProcess: {}, ScopeSession: {}, ScopeWorkspace: {}}, yoloSessions: make(map[string]struct{})}
}

// Authorize returns the effective decision. Ask is never returned: it is either
// resolved by a reply or converted to deny in noninteractive mode.
func (b *Broker) Authorize(ctx context.Context, request Request) (Decision, error) {
	if err := request.Verify(); err != nil {
		return Deny, err
	}
	b.mu.Lock()
	_, yolo := b.yoloSessions[request.SessionID]
	b.mu.Unlock()
	if yolo && request.SessionID != "" {
		return Allow, nil
	}
	decision, reason, hard := b.policy.Evaluate(request)
	b.mu.Lock()
	_, yolo = b.yoloSessions[request.SessionID]
	if yolo && request.SessionID != "" {
		b.mu.Unlock()
		return Allow, nil
	}
	if hard {
		b.mu.Unlock()
		return Deny, nil
	}
	if b.grantedLocked(request) {
		decision = Allow
	}
	b.mu.Unlock()
	if decision != Ask {
		return decision, nil
	}
	if b.noninteractive {
		return Deny, nil
	}
	state, err := b.addPending(request, reason)
	if err != nil {
		return Deny, err
	}
	if state == nil { // YOLO was enabled between evaluation and enqueueing.
		return Allow, nil
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

func (b *Broker) ReplyOnce(id string) error      { return b.reply(id, Reply{Allow, ""}) }
func (b *Broker) ReplyProcess(id string) error   { return b.reply(id, Reply{Allow, ScopeProcess}) }
func (b *Broker) ReplySession(id string) error   { return b.reply(id, Reply{Allow, ScopeSession}) }
func (b *Broker) ReplyWorkspace(id string) error { return b.reply(id, Reply{Allow, ScopeWorkspace}) }
func (b *Broker) Reject(id string) error         { return b.reply(id, Reply{Deny, ""}) }

// EnableYolo allows every permission request in the pending request's session
// for the lifetime of this broker. It also releases requests already pending
// for that session. The setting is deliberately in-memory and session-scoped.
func (b *Broker) EnableYolo(id string) error {
	b.mu.Lock()
	state, ok := b.pending[id]
	if !ok || state.pending.Request.SessionID == "" {
		b.mu.Unlock()
		return errors.New("permission request is unknown, already settled, or has no session")
	}
	sessionID := state.pending.Request.SessionID
	b.yoloSessions[sessionID] = struct{}{}
	settled := make([]*pendingState, 0, 1)
	for pendingID, candidate := range b.pending {
		if candidate.pending.Request.SessionID == sessionID {
			delete(b.pending, pendingID)
			settled = append(settled, candidate)
		}
	}
	b.mu.Unlock()
	for _, candidate := range settled {
		candidate.result <- Reply{Decision: Allow}
	}
	return nil
}

func (b *Broker) addPending(request Request, reason string) (*pendingState, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return nil, err
	}
	state := &pendingState{Pending{hex.EncodeToString(idBytes), request, reason}, make(chan Reply, 1)}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.yoloSessions[request.SessionID]; ok && request.SessionID != "" {
		return nil, nil
	}
	b.pending[state.pending.ID] = state
	return state, nil
}

func (b *Broker) reply(id string, reply Reply) error {
	if reply.Decision != Allow && reply.Decision != Deny {
		return errors.New("invalid permission reply decision")
	}
	if reply.Decision == Allow && reply.Scope != "" && reply.Scope != ScopeProcess && reply.Scope != ScopeSession && reply.Scope != ScopeWorkspace {
		return errors.New("invalid permission reply scope")
	}
	b.mu.Lock()
	state, ok := b.pending[id]
	if !ok {
		b.mu.Unlock()
		return errors.New("permission request is unknown or already settled")
	}
	delete(b.pending, id)
	if reply.Decision == Allow && reply.Scope != "" {
		contextID := scopeContext(reply.Scope, state.pending.Request)
		b.grants[reply.Scope][grantKey{state.pending.Request.OperationHash, contextID}] = struct{}{}
	}
	b.mu.Unlock()
	state.result <- reply
	return nil
}

func (b *Broker) grantedLocked(r Request) bool {
	for _, scope := range []Scope{ScopeProcess, ScopeSession, ScopeWorkspace} {
		_, ok := b.grants[scope][grantKey{r.OperationHash, scopeContext(scope, r)}]
		if ok {
			return true
		}
	}
	return false
}

func scopeContext(scope Scope, r Request) string {
	switch scope {
	case ScopeSession:
		return r.SessionID
	case ScopeWorkspace:
		return r.Workspace
	default:
		return "process"
	}
}
