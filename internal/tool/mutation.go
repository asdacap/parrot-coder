package tool

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
	"time"
)

func mutationToolPlan(toolID string, raw json.RawMessage, planned change.Plan) (Plan, error) {
	resources := make([]permission.Resource, len(planned.Mutations))
	files := make([]map[string]any, len(planned.Mutations))
	for i, mutation := range planned.Mutations {
		operation := "write"
		if !mutation.Before.Exists {
			operation = "create"
		} else if !mutation.After.Exists {
			operation = "delete"
		}
		resources[i] = permission.Resource{Kind: "filesystem", Identifier: mutation.Path, Operation: operation, Attributes: map[string]string{
			"before_sha256": mutation.Before.SHA256,
			"after_sha256":  mutation.After.SHA256,
		}}
		files[i] = map[string]any{"path": mutation.Path, "operation": operation, "before_sha256": mutation.Before.SHA256, "after_sha256": mutation.After.SHA256}
	}
	review, err := json.Marshal(map[string]any{"diff": planned.Diff, "files": files})
	if err != nil {
		return Plan{}, err
	}
	request, err := permission.NewRequest(toolID, raw, resources, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(toolID, raw, []permission.Request{request}, review, mutationPlan{planned})
}

func executeMutation(ctx context.Context, changes *change.Service, snapshots *snapshot.Service, plan Plan, call CallContext) (Result, error) {
	if changes == nil {
		changes = call.Changes
	}
	if snapshots == nil {
		snapshots = call.Snapshots
	}
	if changes == nil || snapshots == nil || call.Workspace == nil || call.SessionID == "" {
		return Result{}, errors.New("mutation tool requires change, snapshot, workspace, and session services")
	}
	planned, ok := plan.Data.(mutationPlan)
	if !ok {
		return Result{}, errors.New("mutation tool received incompatible plan")
	}
	if err := changes.Commit(ctx, call.Workspace, planned.Change); err != nil {
		return Result{}, err
	}
	entries := make([]snapshot.Entry, len(planned.Change.Mutations))
	for i, mutation := range planned.Change.Mutations {
		entries[i] = snapshot.Entry{Path: mutation.Path, Before: snapshotState(mutation.Before), After: snapshotState(mutation.After)}
	}
	transaction, err := snapshots.Record(ctx, call.Workspace, call.SessionID, entries)
	if errors.Is(err, snapshot.ErrQuota) {
		// A full history must not cost the user their edit. Undo is a
		// convenience; the write the model was asked to make is the point. The
		// edit stays and only its undo entry is lost, which the text reports so
		// the model does not offer an undo that cannot happen.
		return Result{
			Text:     planned.Change.Diff + "\n\nNote: undo history is full, so this change was not recorded and cannot be undone.",
			Metadata: map[string]any{"files": len(entries), "snapshot_recorded": false},
		}, nil
	}
	if err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return Result{}, errors.Join(err, changes.Rollback(rollbackCtx, call.Workspace, planned.Change))
	}
	return Result{Text: planned.Change.Diff, Metadata: map[string]any{"transaction_id": transaction.ID, "files": len(entries), "snapshot_recorded": true}}, nil
}

func snapshotState(state change.FileState) snapshot.State {
	return snapshot.State{Path: state.Path, Exists: state.Exists, Mode: state.Mode, SymlinkTarget: state.SymlinkTarget, Data: append([]byte(nil), state.Data...), SHA256: state.SHA256}
}
