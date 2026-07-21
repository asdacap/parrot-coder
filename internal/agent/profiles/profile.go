package profiles

import (
	"slices"

	"github.com/amirulashraf/parrot-coder/internal/tool"
)

type Profile struct {
	ID             string
	Prompt         string
	AllowedToolIDs []string
	// DeniedToolIDs removes tools a profile must never use even though they are
	// otherwise available to it. It applies after AllowedToolIDs.
	DeniedToolIDs []string
	// AllowedWritableToolIDs lists tools that are technically writable but are
	// permitted to a read-only profile because the sandbox prevents filesystem
	// mutations. It only has effect when ReadOnly is true.
	AllowedWritableToolIDs []string
	HardRules              []string
	MaxTurns               int
	RecursionLimit         int
	ReadOnly               bool
}

// ListAllowsTool applies only the profile's own allow and deny lists.
//
// Membership is a linear scan rather than a binary search: the lists are short,
// checked once per tool per turn, and a sorted-slice invariant is exactly the
// kind of thing that silently breaks. It did: this list was previously
// binary-searched while Review's was unsorted, which denied that agent git_diff
// and every lsp_* tool.
func (p Profile) ListAllowsTool(id string) bool {
	if slices.Contains(p.DeniedToolIDs, id) {
		return false
	}
	if len(p.AllowedToolIDs) == 0 {
		return true
	}
	return slices.Contains(p.AllowedToolIDs, id)
}

// AllowsAll reports whether every listed tool is available to the profile.
func (p Profile) AllowsAll(ids []string) bool {
	for _, id := range ids {
		if !p.ListAllowsTool(id) {
			return false
		}
	}
	return true
}

// AllowsWritableTool reports whether a read-only profile explicitly permits a
// tool that is marked as writable. Such tools are safe only when the runtime
// sandbox prevents them from mutating the workspace.
func (p Profile) AllowsWritableTool(id string) bool {
	return slices.Contains(p.AllowedWritableToolIDs, id)
}

// AllowsTool implements tool.SecurityProfile by checking the profile's allow
// and deny lists together with the tool's read-only status.
func (p Profile) AllowsTool(id string, readOnly bool) bool {
	if !p.ListAllowsTool(id) {
		return false
	}
	if p.ReadOnly && !readOnly && !p.AllowsWritableTool(id) {
		return false
	}
	return true
}

// GetSecurityProfile returns the profile as a tool.SecurityProfile.
func (p Profile) GetSecurityProfile() tool.SecurityProfile { return p }
