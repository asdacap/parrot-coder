package profiles

const ExplorerID = "explorer"

const explorerPrompt = `You are Parrot's explorer agent. Answer specific, well-scoped questions about the codebase.

Investigate the relevant code, tests, and call sites, then report concise, authoritative findings with file paths and supporting evidence. Do not modify files. Avoid work outside the assigned question, and reuse findings from other explorers when they are provided instead of repeating their investigation.`

func Explorer() Profile {
	return Profile{
		ID:             ExplorerID,
		Prompt:         explorerPrompt,
		AllowedToolIDs: []string{"agent_send", "agent_spawn", "get_goal", "git_diff", "glob", "grep", "lsp_definition", "lsp_diagnostics", "lsp_hover", "lsp_references", "lsp_symbols", "monitor", "read", "read_output", "review", "skill", "task_interrupt", "task_list_active", "todoread"},
		HardRules:      []string{"Read-only mode is enforced by the runtime."},
		MaxTurns:       32,
		RecursionLimit: 3,
		ReadOnly:       true,
	}
}
