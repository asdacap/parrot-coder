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

// AllowsTool implements tool.SecurityProfile by checking the read-only status
// of the profile against the tool's declared read-only status.
func (p Profile) AllowsTool(id string, readOnly bool) bool {
	return !p.IsReadOnly() || readOnly
}

// GetSecurityProfile returns the profile as a tool.SecurityProfile.
func (p Profile) GetSecurityProfile() tool.SecurityProfile { return p }
