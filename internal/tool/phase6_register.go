package tool

import (
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
)

type Phase6Services struct {
	Changes   *change.Service
	Snapshots *snapshot.Service
	Processes *process.Runner
	Todos     *session.TodoService
	Questions *question.Broker
}

// RegisterPhase6 registers all Phase 6 tools. Build profiles expose registered
// tools by default; plan and explore remain restricted by agent enforcement.
func RegisterPhase6(registry *Registry, services Phase6Services) error {
	if registry == nil {
		return errors.New("tool: registry is required")
	}
	for _, item := range []Tool{
		NewEditTool(services.Changes, services.Snapshots),
		NewApplyPatchTool(services.Changes, services.Snapshots),
		NewShellTool(services.Processes),
		NewTodoReadTool(services.Todos),
		NewTodoWriteTool(services.Todos),
		NewQuestionTool(services.Questions),
	} {
		if err := registry.Register(item); err != nil {
			return fmt.Errorf("tool: register %s: %w", item.ID(), err)
		}
	}
	return nil
}
