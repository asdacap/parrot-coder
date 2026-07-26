package tool

import (
	"errors"
	"sort"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/mcp"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
)

// BuiltinServices contains the application services used by built-in tools.
// Optional integrations are enabled when their corresponding configuration is
// present.
type BuiltinServices struct {
	Changes   *change.Service
	Processes *process.Runner
	Tasks     TaskController
	Todos     *session.TodoService
	Goals     *session.GoalService
	Questions *question.Broker
	Queues    QueueService

	Skills    *skill.Registry
	MCP       MCPCaller
	MCPTools  []mcp.ToolDefinition
	WebFetch  *webfetch.Service
	Agents    AgentLookup
	ConfigDir string // Parrot config directory for writing global parrot.yaml
	Status    *statusinfo.Registry
}

// BuiltinProviders assembles one provider per enabled built-in tool. Providers
// retain shared services and allocate a new tool for every bound agent session.
func BuiltinProviders(services BuiltinServices) (Providers, error) {
	if services.Skills == nil || services.WebFetch == nil || services.Agents == nil || services.Status == nil || services.Queues == nil {
		return Providers{}, errors.New("tool: built-in services are required")
	}
	constructors := []func() Tool{
		func() Tool { return NewReadTool(ReadConfig{}) },
		func() Tool { return NewShowTool(ReadConfig{}) },
		func() Tool { return NewGlobTool(GlobConfig{}) },
		func() Tool { return NewRgTool(RgConfig{}) },
		func() Tool { return NewApplyPatchTool(services.Changes) },
		func() Tool { return NewExecCommandTool(services.Processes) },
		func() Tool { return NewWritePermissionTool(services.Processes) },
		func() Tool { return NewWriteStdinTool(services.Processes) },
		func() Tool { return NewTodoReadTool(services.Todos) },
		func() Tool { return NewTodoWriteTool(services.Todos) },
		func() Tool { return NewGetGoalTool(services.Goals) },
		func() Tool { return NewCreateGoalTool(services.Goals) },
		func() Tool { return NewUpdateGoalTool(services.Goals) },
		func() Tool { return NewQuestionTool(services.Questions) },
		func() Tool { return NewSkillTool(services.Skills) },
		func() Tool { return NewWebFetchTool(services.WebFetch) },
		func() Tool { return NewGitDiffTool() },
		func() Tool { return NewSetConfigTool(services.ConfigDir) },
		func() Tool { return NewStatusTool(services.Status) },
	}
	for _, kind := range []string{"queue_create", "queue_info", "queue_monitor", "queue_push", "queue_take"} {
		kind := kind
		constructors = append(constructors, func() Tool { return &QueueTool{Kind: kind, Store: services.Queues} })
	}
	for _, kind := range []string{"task_list_active", "task_interrupt"} {
		kind := kind
		constructors = append(constructors, func() Tool { return &TaskTool{Kind: kind, Controller: services.Tasks} })
	}
	for _, kind := range []managedtask.Kind{managedtask.KindShell, managedtask.KindAgent} {
		kind := kind
		constructors = append(constructors, func() Tool { return &WaitTool{Kind: kind, Controller: services.Tasks} })
	}
	items := make([]ToolProvider, 0, len(constructors)+2+len(services.MCPTools))
	for _, constructor := range constructors {
		constructor := constructor
		items = append(items, providerFor(constructor))
	}
	for _, kind := range []string{agentSpawnID, agentSendID} {
		kind := kind
		items = append(items, &ProviderFunc{ToolDescriptor: AgentDescriptor(kind), CreateTool: func(state AgentSession) (Tool, error) {
			if state == nil {
				return nil, errors.New("agent session state is required")
			}
			return &AgentTool{Kind: kind, Session: state, Agents: services.Agents}, nil
		}})
	}
	definitions := append([]mcp.ToolDefinition(nil), services.MCPTools...)
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	for _, definition := range definitions {
		definition := definition
		prototype, err := NewMCPTool(services.MCP, definition)
		if err != nil {
			return Providers{}, err
		}
		items = append(items, &ProviderFunc{ToolDescriptor: DescriptorOf(prototype), CreateTool: func(AgentSession) (Tool, error) { return NewMCPTool(services.MCP, definition) }})
	}
	return NewProviders(items...)
}

type sessionTool struct {
	Tool
	identity byte
}

func (t *sessionTool) UnwrapTool() Tool { return t.Tool }

func providerFor(constructor func() Tool) ToolProvider {
	return &ProviderFunc{ToolDescriptor: DescriptorOf(constructor()), CreateTool: func(AgentSession) (Tool, error) {
		return &sessionTool{Tool: constructor()}, nil
	}}
}
