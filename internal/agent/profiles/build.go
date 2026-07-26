package profiles

import "github.com/amirulashraf/parrot-coder/internal/status"

const BuildID = "build"

func Build() Profile {
	return New(
		BuildID,
		"You are Parrot's build agent. Implement and verify the requested changes.",
		[]string{"Keep tool side effects within the authorized workspace."},
		64,
		3,
		false,
		nil,
		status.Static{ProviderKey: "profile:build", Text: "Build profile: implement and verify requested changes. Workspace writes are permitted through the active security policy."},
	)
}
