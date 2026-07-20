package tool

import "github.com/amirulashraf/parrot-coder/internal/permission"

// PermissionChoicer is implemented by a tool which constrains how its own
// authorization may be answered. It is optional: a tool which does not
// implement it offers the standard choices, which is the safe default because
// every non-standard set is narrower.
//
// A tool implements it when approving the operation would grant more than the
// operation itself — a tool which hands out a lasting capability must not be
// approvable for a scope broader than the capability it hands out.
type PermissionChoicer interface {
	PermissionChoices() []permission.Choice
}

// ChoicesFor returns the answers a tool offers for its authorization.
func ChoicesFor(t Tool) []permission.Choice {
	if chooser, ok := t.(PermissionChoicer); ok {
		if choices := chooser.PermissionChoices(); len(choices) > 0 {
			return choices
		}
	}
	return DefaultPermissionChoices()
}

// DefaultPermissionChoices are the answers offered for an ordinary operation.
func DefaultPermissionChoices() []permission.Choice {
	// Values are the strings a user types, so they match the established
	// interface rather than the internal decision and scope names.
	return []permission.Choice{
		{Value: "yes", Decision: "allow", Label: "yes", Description: "Allow this request once"},
		{Value: "no", Decision: "deny", Label: "no", Description: "Deny this request"},
		{Value: "allow all for session", Decision: "allow", Scope: "session", Label: "allow all for session", Description: "Allow matching requests for this session"},
		{Value: "allow all for workspace", Decision: "allow", Scope: "workspace", Label: "allow all for workspace", Description: "Allow matching requests for this workspace"},
		{Value: "allow all for process", Decision: "allow", Scope: "process", Label: "allow all for process", Description: "Allow matching requests until Parrot exits"},
		{Value: "enable yolo", Decision: "allow", Scope: "yolo", Label: "enable yolo", Description: "Disable all permission checks for this session"},
	}
}
