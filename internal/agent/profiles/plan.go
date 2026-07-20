package profiles

const PlanID = "plan"

func Plan() Profile {
	return Profile{
		ID:             PlanID,
		Prompt:         "You are Parrot's planning agent. Inspect the project and produce an implementation plan.",
		HardRules:      []string{"Read-only mode is enforced by the runtime."},
		MaxTurns:       24,
		RecursionLimit: 1,
		ReadOnly:       true,
	}
}
