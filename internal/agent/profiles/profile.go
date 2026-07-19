package profiles

import "sort"

type Profile struct {
	ID             string
	Prompt         string
	AllowedToolIDs []string
	HardRules      []string
	MaxTurns       int
	RecursionLimit int
	ReadOnly       bool
}

func (p Profile) AllowsTool(id string) bool {
	if p.ReadOnly && !readOnlyTool(id) {
		return false
	}
	if len(p.AllowedToolIDs) == 0 {
		return true
	}
	i := sort.SearchStrings(p.AllowedToolIDs, id)
	return i < len(p.AllowedToolIDs) && p.AllowedToolIDs[i] == id
}

func readOnlyTool(id string) bool {
	switch id {
	case "read", "glob", "git_diff", "grep", "monitor", "read_output", "review", "skill", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols", "agent_spawn", "agent_send", "task_interrupt", "task_list_active", "todoread", "get_goal":
		return true
	default:
		return false
	}
}
