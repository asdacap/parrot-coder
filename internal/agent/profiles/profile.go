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
	WritePaths     []string
	Status         status.Provider
}

// IsReadOnly reports whether the profile is read-only.
func (p Profile) IsReadOnly() bool { return p.ReadOnly }

// GetSecurityProfile returns the profile as a security.SecurityProfile.
func (p Profile) GetSecurityProfile() security.SecurityProfile { return p }

// AllowReadPaths returns the file-system paths the process may read.
func (p Profile) AllowReadPaths() []string { return []string{"/"} }

// AllowWritePaths returns the file-system paths the process may write.
func (p Profile) AllowWritePaths() []string { return append([]string(nil), p.WritePaths...) }

// DenyWritePaths returns paths within AllowWritePaths that must remain read-only.
func (p Profile) DenyWritePaths() []string { return nil }

// Rules returns ordered sandbox rules. Profiles do not carry rules; the
// runner applies configured rules from process.Config.
func (p Profile) Rules() []security.Rule { return nil }
