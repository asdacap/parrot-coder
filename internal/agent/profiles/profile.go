package profiles

import (
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/status"
)

const (
	ExplorerID = "explorer"
	ReviewID   = "review"
	WorkerID   = "worker"
)

// Profile describes an agent's instructions, limits, status, and security
// policy.
type Profile interface {
	security.SecurityProfile

	ID() string
	Prompt() string
	Usage() string
	HardRules() []string
	MaxTurns() int
	RecursionLimit() int
	Status() status.Provider
}

type profile struct {
	id             string
	prompt         string
	usage          string
	hardRules      []string
	maxTurns       int
	recursionLimit int
	readOnly       bool
	sandboxRules   []security.Rule
	statusProvider status.Provider
}

// New constructs an immutable Profile.
func New(id, prompt, usage string, hardRules []string, maxTurns, recursionLimit int, readOnly bool, sandboxRules []security.Rule, statusProvider status.Provider) Profile {
	return profile{
		id:             id,
		prompt:         prompt,
		usage:          usage,
		hardRules:      append([]string(nil), hardRules...),
		maxTurns:       maxTurns,
		recursionLimit: recursionLimit,
		readOnly:       readOnly,
		sandboxRules:   append([]security.Rule(nil), sandboxRules...),
		statusProvider: statusProvider,
	}
}

func (p profile) ID() string              { return p.id }
func (p profile) Prompt() string          { return p.prompt }
func (p profile) Usage() string           { return p.usage }
func (p profile) MaxTurns() int           { return p.maxTurns }
func (p profile) RecursionLimit() int     { return p.recursionLimit }
func (p profile) IsReadOnly() bool        { return p.readOnly }
func (p profile) Status() status.Provider { return p.statusProvider }
func (p profile) HardRules() []string     { return append([]string(nil), p.hardRules...) }
func (p profile) Rules() []security.Rule  { return append([]security.Rule(nil), p.sandboxRules...) }
