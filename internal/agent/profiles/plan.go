package profiles

const PlanID = "plan"

func Plan() Profile {
	return Profile{
		ID:             PlanID,
		Prompt:         "You are Parrot's planning agent. Inspect the project and produce an implementation plan.",
		AllowedToolIDs: []string{"agent_interrupt", "agent_list", "agent_send", "agent_spawn", "agent_wait", "get_goal", "glob", "git_diff", "grep", "read", "read_output", "review", "skill", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols", "task", "task_status", "task_cancel", "todoread"},
		HardRules:      []string{"Read-only mode is enforced by the runtime."},
		MaxTurns:       24,
		ReadOnly:       true,
	}
}
