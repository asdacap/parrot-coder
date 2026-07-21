package profiles

import (
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

type Profile struct {
	ID             string
	Prompt         string
	HardRules      []string
	MaxTurns       int
	RecursionLimit int
	ReadOnly       bool
}

// IsReadOnly reports whether the profile is read-only.
func (p Profile) IsReadOnly() bool { return p.ReadOnly }

// GetSecurityProfile returns the profile as a tool.SecurityProfile.
func (p Profile) GetSecurityProfile() tool.SecurityProfile { return p }
