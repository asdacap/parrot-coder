package profiles

import "slices"

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

// AllowsTool applies only the profile's own allow and deny lists. Whether a
// tool is read-only is the tool's own business and is checked separately by the
// caller, which holds the tool registry; see agent.ProfileAllows.
//
// Membership is a linear scan rather than a binary search: the lists are short,
// checked once per tool per turn, and a sorted-slice invariant is exactly the
// kind of thing that silently breaks. It did: this list was previously
// binary-searched while Review's was unsorted, which denied that agent git_diff
// and every lsp_* tool.
func (p Profile) AllowsTool(id string) bool {
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
		if !p.AllowsTool(id) {
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
