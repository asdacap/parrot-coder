package tool

import (
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/formatter"
	"github.com/amirulashraf/parrot-coder/internal/mcp"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
	"sort"
)

type Phase9Services struct {
	Skills     *skill.Registry
	MCP        MCPCaller
	MCPTools   []mcp.ToolDefinition
	WebFetch   *webfetch.Service
	LSP        LSPToolConfig
	Formatters *formatter.Registry
	Changes    *change.Service
	Snapshots  *snapshot.Service
	Subagents  *subagent.Manager
	Agents     AgentLookup
}

func RegisterPhase9(registry *Registry, services Phase9Services) error {
	if registry == nil || services.Skills == nil || services.WebFetch == nil || services.Subagents == nil || services.Agents == nil {
		return errors.New("tool: phase 9 core services are required")
	}
	items := []Tool{NewSkillTool(services.Skills), NewWebFetchTool(services.WebFetch), NewGitDiffTool()}
	if services.LSP.Client != nil {
		items = append(items, NewLSPTools(services.LSP)...)
	}
	if services.Formatters != nil {
		items = append(items, NewFormatTool(services.Formatters, services.Changes, services.Snapshots))
	}
	items = append(items, NewTaskTools(services.Subagents, services.Agents)...)
	items = append(items, NewReviewTool(services.Subagents, services.Agents))
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
