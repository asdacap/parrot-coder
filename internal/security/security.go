// Package security defines the security policy for sandboxed processes.
package security

// RuleAction is the action a sandbox rule applies to its path.
type RuleAction string

const (
	// ActionAllowWrite grants read and write access to the path.
	ActionAllowWrite RuleAction = "allow_write"
	// ActionDenyRead removes read access from the path.
	ActionDenyRead RuleAction = "deny_read"
	// ActionAllowRead grants read-only access to the path.
	ActionAllowRead RuleAction = "allow_read"
	// ActionDenyWrite removes write access from the path, leaving it
	// read-only.
	ActionDenyWrite RuleAction = "deny_write"
)

// ValidRuleActions is the set of accepted RuleAction values.
var ValidRuleActions = map[RuleAction]bool{
	ActionAllowWrite: true,
	ActionDenyRead:   true,
	ActionAllowRead:  true,
	ActionDenyWrite:  true,
}

// Rule is a single ordered sandbox rule mapping a path to an action.
type Rule struct {
	Path   string
	Action RuleAction
}

// SecurityProfile defines the security policy for sandboxed processes. It
// controls which filesystem paths a sandboxed process may read or write.
type SecurityProfile interface {
	// IsReadOnly reports whether the profile restricts tool execution to
	// read-only operations.
	IsReadOnly() bool

	// AllowReadPaths returns file-system paths the process may read.
	AllowReadPaths() []string

	// AllowWritePaths returns file-system paths the process may write.
	AllowWritePaths() []string

	// DenyWritePaths returns paths within AllowWritePaths that must remain
	// read-only (e.g. protected metadata files).
	DenyWritePaths() []string

	// Rules returns ordered sandbox rules applied after the base read,
	// write, and deny-write paths. Later rules override earlier ones.
	Rules() []Rule
}
