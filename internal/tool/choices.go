package tool

import "github.com/amirulashraf/parrot-coder/internal/permission"

// PermissionChoicer is implemented by a tool which labels its own
// authorization differently from the standard yes/no answers. It is optional:
// a tool which does not implement it offers the standard choices.
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
// Nothing is remembered, so every answer settles exactly the request which
// raised it.
func DefaultPermissionChoices() []permission.Choice {
	// Values are the strings a user types, so they match the established
	// interface rather than the internal decision names.
	return []permission.Choice{
		{Value: "yes", Decision: "allow", Label: "yes", Description: "Allow this request"},
		{Value: "no", Decision: "deny", Label: "no", Description: "Deny this request"},
		{Value: "reject with reason", Decision: "deny", Label: "reject with reason", Description: "Deny and provide feedback to the agent", RequiresReason: true},
	}
}
