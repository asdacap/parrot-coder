package profiles

const BuildID = "build"

func Build() Profile {
	return Profile{
		ID:        BuildID,
		Prompt:    "You are Parrot's build agent. Implement and verify the requested changes.",
		HardRules: []string{"Keep tool side effects within the authorized workspace."},
		MaxTurns:  64,
	}
}
