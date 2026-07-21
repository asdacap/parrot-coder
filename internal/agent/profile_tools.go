package agent

import (
	"github.com/amirulashraf/parrot-coder/internal/agent/profiles"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

// ProfileAllows reports whether a profile may call a tool. It combines the
// profile's own lists with the tool's declared read-onlyness.
func ProfileAllows(profile profiles.Profile, definition tool.Definition) bool {
	return profile.AllowsTool(definition.ID, definition.ReadOnly)
}
