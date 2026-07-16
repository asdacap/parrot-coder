package tool

import (
	"github.com/amirulashraf/parrot-coder/internal/permission"
)

// DefaultWorkspacePolicy allows canonical read/search operations and reviewed
// workspace mutations made through edit or apply_patch. Other operations,
// including process execution, remain at ask.
func DefaultWorkspacePolicy() permission.Policy {
	return permission.Policy{Default: permission.Ask, Rules: []permission.Rule{{Match: func(r permission.Request) bool {
		if len(r.Resources) == 0 {
			return false
		}
		for _, resource := range r.Resources {
			if resource.Operation != "read" && resource.Operation != "search" {
				return false
			}
		}
		return true
	}, Decision: permission.Allow, Reason: "read-only operation"}, {Match: func(r permission.Request) bool {
		if r.ToolID != "edit" && r.ToolID != "apply_patch" || len(r.Resources) == 0 {
			return false
		}
		for _, resource := range r.Resources {
			if resource.Kind != "filesystem" || resource.Operation != "write" && resource.Operation != "create" && resource.Operation != "delete" {
				return false
			}
		}
		return true
	}, Decision: permission.Allow, Reason: "reviewed workspace mutation"}}}
}

// DefaultReadOnlyPolicy is retained for callers that need automatic access
// only to non-mutating workspace operations.
func DefaultReadOnlyPolicy() permission.Policy {
	policy := DefaultWorkspacePolicy()
	policy.Rules = policy.Rules[:1]
	return policy
}
