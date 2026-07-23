package profiles

import "github.com/amirulashraf/parrot-coder/internal/status"

const PlanID = "plan"

func Plan() Profile {
	return Profile{
		ID:             PlanID,
		Prompt:         "You are Parrot's planning agent. Inspect the project and produce an implementation plan.",
		HardRules:      []string{"Read-only mode is enforced by the runtime."},
		MaxTurns:       24,
		RecursionLimit: 1,
		ReadOnly:       true,
		Status:         status.Static{ProviderKey: "profile:plan-agent", Text: "Planning profile: inspect the project and produce an implementation plan. Read-only mode is enforced by the runtime."},
	}
}
