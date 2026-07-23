// Package security defines the security policy for sandboxed processes.
package security

import "path/filepath"

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

// SecurityProfile defines the security policy for sandboxed processes.
type SecurityProfile interface {
	// IsReadOnly reports whether the profile restricts tool execution to
	// read-only operations.
	IsReadOnly() bool

	// Rules returns ordered filesystem rules. Later rules override earlier
	// ones.
	Rules() []Rule
}

// CanWrite reports whether profile permits writing path. A writable profile
// permits writes by default, while a read-only profile requires a matching
// allow_write rule. Matching rules are applied in order.
func CanWrite(profile SecurityProfile, path string) bool {
	if profile == nil {
		return true
	}
	writable := !profile.IsReadOnly()
	for _, rule := range profile.Rules() {
		if !pathWithin(path, rule.Path) {
			continue
		}
		switch rule.Action {
		case ActionAllowWrite:
			writable = true
		case ActionDenyRead, ActionAllowRead, ActionDenyWrite:
			writable = false
		}
	}
	return writable
}

func pathWithin(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	path, root = filepath.Clean(path), filepath.Clean(root)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !filepath.IsAbs(relative) &&
		(relative == "." || len(relative) < 3 || relative[:3] != ".."+string(filepath.Separator))
}
