package tool

import (
	"github.com/amirulashraf/parrot-coder/internal/permission"
)

// DefaultWorkspacePolicy allows canonical read/search operations, bounded web
// fetches, reviewed workspace mutations made through edit or apply_patch, and
// sandboxed shell execution. Other operations remain at ask.
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
		if r.ToolID != "web_fetch" || len(r.Resources) == 0 {
			return false
		}
		for _, resource := range r.Resources {
			if resource.Kind != "network" || resource.Operation != "GET" && resource.Operation != "HEAD" {
				return false
			}
		}
		return true
	}, Decision: permission.Allow, Reason: "bounded web fetch"}, {Match: func(r permission.Request) bool {
		if r.ToolID != "edit" && r.ToolID != "apply_patch" || len(r.Resources) == 0 {
			return false
		}
		for _, resource := range r.Resources {
			if resource.Kind != "filesystem" || resource.Operation != "write" && resource.Operation != "create" && resource.Operation != "delete" {
				return false
			}
		}
		return true
	}, Decision: permission.Allow, Reason: "reviewed workspace mutation"}, {Match: func(r permission.Request) bool {
		if r.ToolID != "shell" || len(r.Resources) == 0 {
			return false
		}
		for _, resource := range r.Resources {
			if resource.Kind != "process" || resource.Operation != "execute" {
				return false
			}
		}
		return true
	}, Decision: permission.Allow, Reason: "sandboxed shell execution"}}}
}

// DefaultReadOnlyPolicy is retained for callers that need automatic access
// only to non-mutating workspace operations.
func DefaultReadOnlyPolicy() permission.Policy {
	policy := DefaultWorkspacePolicy()
	policy.Rules = policy.Rules[:2]
	return policy
}
