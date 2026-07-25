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
	SecurityCapabilities []security.Rule
	Status               status.Provider
}

// IsReadOnly reports whether the profile is read-only.
func (p Profile) IsReadOnly() bool { return p.ReadOnly }

// GetSecurityProfile returns the profile as a security.SecurityProfile.
func (p Profile) GetSecurityProfile() security.SecurityProfile { return p }

// Rules returns the profile's complete ordered security policy.
func (p Profile) Rules() []security.Rule {
	return append(p.BaseRules(), p.SecurityCapabilities...)
}

// BaseRules returns reusable profile policy rules.
func (p Profile) BaseRules() []security.Rule {
	return append([]security.Rule(nil), p.SandboxRules...)
}

// CapabilityRules returns capabilities added while preparing this session's turn.
func (p Profile) CapabilityRules() []security.Rule {
	return append([]security.Rule(nil), p.SecurityCapabilities...)
}
