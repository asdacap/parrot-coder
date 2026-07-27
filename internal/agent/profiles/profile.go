package profiles

import "github.com/amirulashraf/parrot-coder/internal/security"

const (
	ExplorerID = "explorer"
	ReviewID   = "review"
	WorkerID   = "worker"
)

// Profile describes an agent's instructions, limits, and security policy.
type Profile interface {
	security.SecurityProfile

	ID() string
	Prompt() string
	Usage() string
	HardRules() []string
	AllowedTools() []string
	MaxTurns() int
	RecursionLimit() int
}

type profile struct {
	id             string
	prompt         string
	usage          string
	hardRules      []string
	allowedTools   []string
	maxTurns       int
	recursionLimit int
	readOnly       bool
	sandboxRules   []security.Rule
}

// New constructs an immutable Profile.
func New(id, prompt, usage string, hardRules, allowedTools []string, maxTurns, recursionLimit int, readOnly bool, sandboxRules []security.Rule) Profile {
	return profile{
		id:             id,
		prompt:         prompt,
		usage:          usage,
		hardRules:      append([]string(nil), hardRules...),
		allowedTools:   cloneStrings(allowedTools),
		maxTurns:       maxTurns,
		recursionLimit: recursionLimit,
		readOnly:       readOnly,
		sandboxRules:   append([]security.Rule(nil), sandboxRules...),
	}
}

func (p profile) ID() string             { return p.id }
func (p profile) Prompt() string         { return p.prompt }
func (p profile) Usage() string          { return p.usage }
func (p profile) MaxTurns() int          { return p.maxTurns }
func (p profile) RecursionLimit() int    { return p.recursionLimit }
func (p profile) IsReadOnly() bool       { return p.readOnly }
func (p profile) HardRules() []string    { return append([]string(nil), p.hardRules...) }
func (p profile) AllowedTools() []string { return cloneStrings(p.allowedTools) }
func (p profile) Rules() []security.Rule { return append([]security.Rule(nil), p.sandboxRules...) }

func cloneStrings(value []string) []string {
	if value == nil {
		return nil
	}
	return append(make([]string, 0, len(value)), value...)
}
