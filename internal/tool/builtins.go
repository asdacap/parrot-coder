package tool

import (
	"errors"
	"fmt"
	"sort"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/formatter"
	"github.com/amirulashraf/parrot-coder/internal/mcp"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
)

// BuiltinServices contains the application services used by built-in tools.
// Optional integrations are enabled when their corresponding configuration is
// present.
type BuiltinServices struct {
	Changes   *change.Service
	Processes *process.Runner
	Todos     *session.TodoService
	Goals     *session.GoalService
	Questions *question.Broker

	Skills     *skill.Registry
	MCP        MCPCaller
	MCPTools   []mcp.ToolDefinition
	WebFetch   *webfetch.Service
	LSP        LSPToolConfig
	Formatters *formatter.Registry
	Subagents  *subagent.Manager
	Agents     AgentLookup
}

// RegisterBuiltins registers the complete built-in tool set. LSP, formatter,
// and MCP tools are added only when those integrations are configured.
func RegisterBuiltins(registry *Registry, services BuiltinServices) error {
	if registry == nil {
		return errors.New("tool: registry is required")
	}
	if services.Skills == nil || services.WebFetch == nil || services.Subagents == nil || services.Agents == nil {
		return errors.New("tool: built-in services are required")
	}

	items := []Tool{
		NewReadTool(ReadConfig{}),
		NewGlobTool(GlobConfig{}),
		NewGrepTool(GrepConfig{}),
		NewReadOutputTool(1 << 20),
		NewEditTool(services.Changes),
		NewApplyPatchTool(services.Changes),
		NewExecCommandTool(services.Processes),
		NewShellTool(services.Processes),
		NewUnrestrictedShellTool(services.Processes),
		NewWritePermissionTool(services.Processes),
		NewWriteStdinTool(services.Processes),
		NewTodoReadTool(services.Todos),
		NewTodoWriteTool(services.Todos),
		NewGetGoalTool(services.Goals),
		NewCreateGoalTool(services.Goals),
		NewUpdateGoalTool(services.Goals),
		NewQuestionTool(services.Questions),
		NewSkillTool(services.Skills),
		NewWebFetchTool(services.WebFetch),
		NewGitDiffTool(),
	}
	if services.LSP.Client != nil {
		items = append(items, NewLSPTools(services.LSP)...)
	}
	if services.Formatters != nil {
		items = append(items, NewFormatTool(services.Formatters, services.Changes))
	}
	items = append(items, NewTaskTools(services.Subagents, services.Agents)...)
	items = append(items, NewReviewTool(services.Subagents, services.Agents))
	items = append(items, NewAgentTools(services.Subagents, services.Agents)...)

	definitions := append([]mcp.ToolDefinition(nil), services.MCPTools...)
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	for _, definition := range definitions {
		item, err := NewMCPTool(services.MCP, definition)
		if err != nil {
			return err
		}
		items = append(items, item)
	}

	for _, item := range items {
		if err := registry.Register(item); err != nil {
			return fmt.Errorf("tool: register %s: %w", item.ID(), err)
		}
	}
	return nil
}
