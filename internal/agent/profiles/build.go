package profiles

import "github.com/amirulashraf/parrot-coder/internal/status"

const BuildID = "build"

func Build() Profile {
	return Profile{
		ID:             BuildID,
		Prompt:         "You are Parrot's build agent. Implement and verify the requested changes.",
		HardRules:      []string{"Keep tool side effects within the authorized workspace."},
		MaxTurns:       64,
		RecursionLimit: 3,
		Status:         status.Static{ProviderKey: "profile:build", Text: "Build profile: implement and verify requested changes. Workspace writes are permitted through the active security policy."},
	}
}
