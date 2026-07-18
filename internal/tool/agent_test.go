package tool

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/subagent"
)

type reusableAgentExecutor struct {
	mu       sync.Mutex
	runs     chan subagent.Execution
	releases map[string]chan string
	sends    chan string
}

func newReusableAgentExecutor() *reusableAgentExecutor {
	return &reusableAgentExecutor{runs: make(chan subagent.Execution, 4), releases: make(map[string]chan string), sends: make(chan string, 4)}
}

func (e *reusableAgentExecutor) Execute(ctx context.Context, execution subagent.Execution) (string, error) {
	if execution.SessionID == "" {
		execution.RegisterSession("session-" + execution.TaskID)
	}
	release := make(chan string, 1)
	e.mu.Lock()
	e.releases[execution.TaskID] = release
	e.mu.Unlock()
	e.runs <- execution
	select {
	case output := <-release:
		return output, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (e *reusableAgentExecutor) Send(_ context.Context, execution subagent.Execution, message string) (string, error) {
	e.sends <- execution.SessionID + ":" + message
	return "message-1", nil
}

func (e *reusableAgentExecutor) release(id, output string) {
	e.mu.Lock()
	release := e.releases[id]
	e.mu.Unlock()
	release <- output
}

func TestAgentToolsReusableLifecycle(t *testing.T) {
	executor := newReusableAgentExecutor()
	manager := subagent.NewManager(executor, subagent.Config{})
	lookup := func(id string) (bool, error) { return id != "build", nil }
	tools := make(map[string]Tool)
	for _, item := range NewAgentTools(manager, lookup) {
		tools[item.ID()] = item
	}
	call := CallContext{SessionID: "root", Agent: "build", ToolCallID: "call-1"}
	execute := func(kind, input string) Result {
		t.Helper()
		item := tools[kind]
		plan, err := item.Plan(context.Background(), json.RawMessage(input), call)
		if err != nil {
			t.Fatalf("plan %s: %v", kind, err)
		}
		result, err := item.Execute(context.Background(), plan, call)
		if err != nil {
			t.Fatalf("execute %s: %v", kind, err)
		}
		return result
	}

	spawned := execute(agentSpawnID, `{"prompt":"inspect","agent":"explorer"}`)
	id, ok := spawned.Metadata["agent_id"].(string)
	if !ok || id == "" || spawned.Metadata["status"] != subagent.StatusRunning {
		t.Fatalf("spawned = %#v", spawned)
	}
	first := <-executor.runs
	if first.Request.Prompt != "inspect" || first.Turn != 1 {
		t.Fatalf("first execution = %#v", first)
	}
	listed := execute(agentListID, `{}`)
	if agents := listed.Metadata["agents"].([]map[string]any); len(agents) != 1 || agents[0]["agent_id"] != id {
		t.Fatalf("listed = %#v", listed)
	}
	sent := execute(agentSendID, `{"agent_id":"`+id+`","message":"focus"}`)
	if sent.Metadata["message_id"] != "message-1" || <-executor.sends != "session-"+id+":focus" {
		t.Fatalf("sent = %#v", sent)
	}
	waited := execute(agentWaitID, `{"ids":["`+id+`"],"timeout_ms":5}`)
	if waited.Metadata["timed_out"] != true {
		t.Fatalf("waited = %#v", waited)
	}

	executor.release(id, "first output")
	if _, err := manager.Await(context.Background(), "root", id); err != nil {
		t.Fatal(err)
	}
	followed := execute(agentSendID, `{"agent_id":"`+id+`","message":"continue"}`)
	if followed.Metadata["turn"] != 2 || followed.Metadata["status"] != subagent.StatusRunning {
		t.Fatalf("followed = %#v", followed)
	}
	second := <-executor.runs
	if second.SessionID != "session-"+id || second.Request.Prompt != "continue" || second.Turn != 2 {
		t.Fatalf("second execution = %#v", second)
	}
	interrupted := execute(agentInterruptID, `{"agent_id":"`+id+`"}`)
	if interrupted.Metadata["status"] != subagent.StatusCanceled {
		t.Fatalf("interrupted = %#v", interrupted)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
}
