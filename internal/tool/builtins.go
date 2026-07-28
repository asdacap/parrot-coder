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
	if services.Skills == nil || services.WebFetch == nil || services.Agents == nil || services.Status == nil {
		return Providers{}, errors.New("tool: built-in services are required")
	}
	constructors := []struct {
		prototype Tool
		create    func(AgentSession) Tool
	}{
		{NewReadTool(ReadConfig{}, nil), func(s AgentSession) Tool { return NewReadTool(ReadConfig{}, s.Workspace()) }},
		{NewShowToUserTool(ReadConfig{}, nil), func(s AgentSession) Tool { return NewShowToUserTool(ReadConfig{}, s.Workspace()) }},
		{NewGlobTool(GlobConfig{}, nil), func(s AgentSession) Tool { return NewGlobTool(GlobConfig{}, s.Workspace()) }},
		{NewRgTool(RgConfig{}, nil), func(s AgentSession) Tool { return NewRgTool(RgConfig{}, s.Workspace()) }},
		{NewApplyPatchTool(services.Changes, nil), func(s AgentSession) Tool { return NewApplyPatchTool(services.Changes, s.Workspace()) }},
		{NewExecCommandTool(services.Processes, nil, ""), func(s AgentSession) Tool { return NewExecCommandTool(services.Processes, s.Workspace(), s.SessionID()) }},
		{NewWritePermissionTool(services.Processes, nil, ""), func(s AgentSession) Tool {
			return NewWritePermissionTool(services.Processes, s.Workspace(), s.SessionID())
		}},
		{NewWriteStdinTool(services.Processes, ""), func(s AgentSession) Tool { return NewWriteStdinTool(services.Processes, s.SessionID()) }},
		{NewTodoReadTool(services.Todos, ""), func(s AgentSession) Tool { return NewTodoReadTool(services.Todos, s.SessionID()) }},
		{NewTodoWriteTool(services.Todos, ""), func(s AgentSession) Tool { return NewTodoWriteTool(services.Todos, s.SessionID()) }},
		{NewGetGoalTool(services.Goals, ""), func(s AgentSession) Tool { return NewGetGoalTool(services.Goals, s.SessionID()) }},
		{NewCreateGoalTool(services.Goals, ""), func(s AgentSession) Tool { return NewCreateGoalTool(services.Goals, s.SessionID()) }},
		{NewUpdateGoalTool(services.Goals, ""), func(s AgentSession) Tool { return NewUpdateGoalTool(services.Goals, s.SessionID()) }},
		{NewQuestionTool(services.Questions, ""), func(s AgentSession) Tool { return NewQuestionTool(services.Questions, s.SessionID()) }},
		{NewSkillTool(services.Skills), func(AgentSession) Tool { return NewSkillTool(services.Skills) }},
		{NewWebFetchTool(services.WebFetch), func(AgentSession) Tool { return NewWebFetchTool(services.WebFetch) }},
		{NewGitDiffTool(nil), func(s AgentSession) Tool { return NewGitDiffTool(s.Workspace()) }},
		{NewSetConfigTool(services.ConfigDir), func(AgentSession) Tool { return NewSetConfigTool(services.ConfigDir) }},
	}
	for _, kind := range []string{"task_list_active", "task_interrupt"} {
		kind := kind
		constructors = append(constructors, struct {
			prototype Tool
			create    func(AgentSession) Tool
		}{&TaskTool{Kind: kind, Controller: services.Tasks}, func(s AgentSession) Tool {
			return &TaskTool{Kind: kind, Controller: services.Tasks, SessionID: s.SessionID()}
		}})
	}
	for _, kind := range []managedtask.Kind{managedtask.KindShell, managedtask.KindAgent} {
		kind := kind
		constructors = append(constructors, struct {
			prototype Tool
			create    func(AgentSession) Tool
		}{&WaitTool{Kind: kind, Controller: services.Tasks}, func(s AgentSession) Tool {
			return &WaitTool{Kind: kind, Controller: services.Tasks, SessionID: s.SessionID()}
		}})
	}
	items := make([]ToolProvider, 0, len(constructors)+8+len(services.MCPTools))
	for _, constructor := range constructors {
		constructor := constructor
		items = append(items, providerForState(constructor.prototype, constructor.create))
	}
	for _, kind := range []string{"queue_create", "queue_info", "queue_listen", "queue_push", "queue_take"} {
		kind := kind
		prototype := &QueueTool{Kind: kind}
		items = append(items, &ProviderFunc{ToolDescriptor: DescriptorOf(prototype), CreateTool: func(state AgentSession) (Tool, error) {
			if state == nil || state.Queues() == nil {
				return nil, errors.New("user session queue manager is required")
			}
			return &QueueTool{Kind: kind, Store: state.Queues()}, nil
		}})
	}
	items = append(items, &ProviderFunc{ToolDescriptor: DescriptorOf(NewStatusTool(services.Status)), CreateTool: func(state AgentSession) (Tool, error) {
		if state == nil || state.Queues() == nil {
			return nil, errors.New("user session queue manager is required")
		}
		registry, err := services.Status.With(statusinfo.NewQueues(state.Queues()))
		if err != nil {
			return nil, err
		}
		return NewStatusTool(registry), nil
	}})
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

func providerForState(prototype Tool, constructor func(AgentSession) Tool) ToolProvider {
	return &ProviderFunc{ToolDescriptor: DescriptorOf(prototype), CreateTool: func(state AgentSession) (Tool, error) {
		if state == nil || state.SessionID() == "" || state.Workspace() == nil {
			return nil, errors.New("agent session ID and workspace are required")
		}
		return &sessionTool{Tool: constructor(state)}, nil
	}}
}
