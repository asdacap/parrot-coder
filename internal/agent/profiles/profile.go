package profiles

import (
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/status"
)

type Profile struct {
	ID                   string
	Prompt               string
	HardRules            []string
	MaxTurns             int
	RecursionLimit       int
	ReadOnly             bool
	SandboxRules         []security.Rule
	EnforcedSandboxRules []security.Rule
	Status               status.Provider
}

// IsReadOnly reports whether the profile is read-only.
func (p Profile) IsReadOnly() bool { return p.ReadOnly }

// GetSecurityProfile returns the profile as a security.SecurityProfile.
func (p Profile) GetSecurityProfile() security.SecurityProfile { return p }

// Rules returns the profile's ordered sandbox rules.
func (p Profile) Rules() []security.Rule {
	return append(append([]security.Rule(nil), p.SandboxRules...), p.EnforcedSandboxRules...)
}

// EnforcedRules returns capability rules that override ambient configuration.
func (p Profile) EnforcedRules() []security.Rule {
	return append([]security.Rule(nil), p.EnforcedSandboxRules...)
}
