package agent

import (
	"github.com/amirulashraf/parrot-coder/internal/agent/profiles"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

// GoalContinuationTools are the tools an agent needs before it can be asked to
// continue autonomously toward a goal: it is told to read the goal and to mark
// it complete or blocked, and cannot do either without these.
var GoalContinuationTools = []string{"get_goal", "update_goal"}

// ProfileAllows reports whether a profile may call a tool. It combines the
// profile's own lists with the tool's declared read-onlyness, which lives on
// the tool rather than in a list this package would have to maintain.
func ProfileAllows(profile profiles.Profile, definition tool.Definition) bool {
	if profile.ReadOnly && !definition.ReadOnly {
		return false
	}
	return profile.AllowsTool(definition.ID)
}

// ProfileAllowsID is ProfileAllows for a call site holding only a tool name.
func ProfileAllowsID(profile profiles.Profile, snapshot tool.Snapshot, id string) bool {
	if profile.ReadOnly && !snapshot.ReadOnly(id) {
		return false
	}
	return profile.AllowsTool(id)
}
