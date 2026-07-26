package profiles

import "github.com/amirulashraf/parrot-coder/internal/status"

const WorkerID = "worker"

const workerPrompt = `You are Parrot's worker agent. Execute the assigned implementation or production task and verify your work.

Typical tasks include implementing part of a feature, fixing tests or bugs, and completing an independently assigned portion of a refactor. Respect the ownership boundaries in the task. You are not alone in the codebase: do not revert other agents' edits, and accommodate concurrent changes made by others.`

func Worker() Profile {
	return New(
		WorkerID,
		workerPrompt,
		[]string{"Keep tool side effects within the authorized workspace."},
		64,
		3,
		false,
		nil,
		status.Static{ProviderKey: "profile:worker", Text: "Worker profile: execute delegated work within the assigned ownership boundaries and verify it."},
	)
}
