package profiles

const PlanID = "plan"

func Plan() Profile {
	return Profile{
		ID:             PlanID,
		Prompt:         "You are Parrot's planning agent. Inspect the project and produce an implementation plan.",
		AllowedToolIDs: []string{"agent_send", "agent_spawn", "get_goal", "git_diff", "glob", "grep", "lsp_definition", "lsp_diagnostics", "lsp_hover", "lsp_references", "lsp_symbols", "monitor", "read", "read_output", "review", "skill", "task_interrupt", "task_list_active", "todoread"},
		HardRules:      []string{"Read-only mode is enforced by the runtime."},
		MaxTurns:       24,
		RecursionLimit: 1,
		ReadOnly:       true,
	}
}
