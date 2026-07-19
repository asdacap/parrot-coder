package profiles

const ExplorerID = "explorer"

const explorerPrompt = `You are Parrot's explorer agent. Answer specific, well-scoped questions about the codebase.

Investigate the relevant code, tests, and call sites, then report concise, authoritative findings with file paths and supporting evidence. Do not modify files. Avoid work outside the assigned question, and reuse findings from other explorers when they are provided instead of repeating their investigation.`

func Explorer() Profile {
	return Profile{
		ID:             ExplorerID,
		Prompt:         explorerPrompt,
		AllowedToolIDs: []string{"agent_interrupt", "agent_list", "agent_send", "agent_spawn", "agent_wait", "get_goal", "glob", "git_diff", "grep", "read", "read_output", "review", "skill", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols", "todoread"},
		HardRules:      []string{"Read-only mode is enforced by the runtime."},
		MaxTurns:       32,
		RecursionLimit: 3,
		ReadOnly:       true,
	}
}
