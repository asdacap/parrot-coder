package profiles

import (
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/status"
)

type Profile struct {
	ID             string
	Prompt         string
	HardRules      []string
	MaxTurns       int
	RecursionLimit int
	ReadOnly       bool
	SandboxRules   []security.Rule
	Status         status.Provider
}

// IsReadOnly reports whether the profile is read-only.
func (p Profile) IsReadOnly() bool { return p.ReadOnly }

// Rules returns the profile's ordered sandbox rules.
func (p Profile) Rules() []security.Rule { return append([]security.Rule(nil), p.SandboxRules...) }
