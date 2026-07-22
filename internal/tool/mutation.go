package tool

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/amirulashraf/parrot-coder/internal/change"
)

func mutationToolPlan(toolID string, raw json.RawMessage, planned change.Plan) (Plan, error) {
	files := make([]map[string]any, len(planned.Mutations))
	for i, mutation := range planned.Mutations {
		operation := "write"
		if !mutation.Before.Exists {
			operation = "create"
		} else if !mutation.After.Exists {
			operation = "delete"
		}
		files[i] = map[string]any{"path": mutation.Path, "operation": operation, "before_sha256": mutation.Before.SHA256, "after_sha256": mutation.After.SHA256}
	}
	review, err := json.Marshal(map[string]any{"diff": planned.Diff, "files": files})
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(toolID, raw, nil, review, mutationPlan{planned})
}

func executeMutation(ctx context.Context, changes *change.Service, plan Plan, call CallContext) (Result, error) {
	if changes == nil {
		changes = call.Changes
	}
	if changes == nil || call.Workspace == nil {
		return Result{}, errors.New("mutation tool requires change and workspace services")
	}
	planned, ok := plan.Data.(mutationPlan)
	if !ok {
		return Result{}, errors.New("mutation tool received incompatible plan")
	}
	if err := changes.Commit(ctx, call.Workspace, planned.Change); err != nil {
		return Result{}, err
	}
	return Result{Text: planned.Change.Diff, ModelText: modelText(planned.Change.Diff), Metadata: map[string]any{"files": len(planned.Change.Mutations)}}, nil
}
