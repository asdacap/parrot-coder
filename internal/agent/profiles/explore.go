package profiles

const ExploreID = "explore"

func Explore() Profile {
	return Profile{
		ID:             ExploreID,
		Prompt:         "You are Parrot's exploration agent. Investigate the project and report evidence.",
		AllowedToolIDs: []string{"glob", "git_diff", "grep", "read", "read_output", "review", "skill", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols", "task", "task_status", "task_cancel", "todoread"},
		HardRules:      []string{"Read-only mode is enforced by the runtime."},
		MaxTurns:       32,
		ReadOnly:       true,
	}
}
