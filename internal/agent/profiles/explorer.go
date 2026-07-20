package profiles

const ExplorerID = "explorer"

const explorerPrompt = `You are Parrot's explorer agent. Answer specific, well-scoped questions about the codebase.

Investigate the relevant code, tests, and call sites, then report concise, authoritative findings with file paths and supporting evidence. Do not modify files. Avoid work outside the assigned question, and reuse findings from other explorers when they are provided instead of repeating their investigation.`

func Explorer() Profile {
	return Profile{
		ID:             ExplorerID,
		Prompt:         explorerPrompt,
		HardRules:      []string{"Read-only mode is enforced by the runtime."},
		MaxTurns:       32,
		RecursionLimit: 3,
		ReadOnly:       true,
	}
}
