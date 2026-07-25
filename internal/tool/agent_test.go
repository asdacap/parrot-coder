package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/subagent"
)

type toolTestTurnPermit struct{}

func (toolTestTurnPermit) Release() {}

type reusableAgentExecutor struct {
	mu       sync.Mutex
	runs     chan subagent.Execution
	releases map[string]chan string
	sends    chan string
}

func newReusableAgentExecutor() *reusableAgentExecutor {
	return &reusableAgentExecutor{runs: make(chan subagent.Execution, 4), releases: make(map[string]chan string), sends: make(chan string, 4)}
}

func (e *reusableAgentExecutor) Prepare(context.Context, subagent.Preparation) (string, error) {
	return "session-child", nil
}

func (*reusableAgentExecutor) TryAdmitTurn(string) (subagent.TurnPermit, bool, error) {
	return toolTestTurnPermit{}, true, nil
}

func (e *reusableAgentExecutor) Execute(ctx context.Context, execution subagent.Execution) (string, error) {
	release := make(chan string, 1)
	e.mu.Lock()
	e.releases[execution.SessionID] = release
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

type managerAgentChildren struct{ manager *subagent.Manager }

type managerChildAgent struct {
	manager       *subagent.Manager
	parentSession string
	sessionID     string
}

func (c managerAgentChildren) Create(ctx context.Context, parentSession, callerAgent, prompt, agent, model, name, toolCallID string) (ChildAgent, error) {
	sessionID, err := c.manager.Spawn(ctx, parentSession, callerAgent, subagent.Request{
		Prompt: prompt, Agent: agent, Model: model, Name: name, ToolCallID: toolCallID,
	})
	if err != nil {
		return nil, err
	}
	return managerChildAgent{manager: c.manager, parentSession: parentSession, sessionID: sessionID}, nil
}

func (c managerAgentChildren) Resolve(parentSession, identifier string) (ChildAgent, error) {
	task, err := c.manager.Resolve(parentSession, identifier)
	if err != nil {
		return nil, err
	}
	return managerChildAgent{manager: c.manager, parentSession: parentSession, sessionID: task.SessionID}, nil
}

func (c managerChildAgent) Task() (AgentTask, bool) {
	task, err := c.manager.Status(c.parentSession, c.sessionID)
	return testAgentTask(task), err == nil
}

func (c managerChildAgent) Send(ctx context.Context, message, toolCallID string) (AgentTask, string, error) {
	task, err := c.manager.Status(c.parentSession, c.sessionID)
	if err != nil {
		return AgentTask{}, "", err
	}
	if task.Status == subagent.StatusRunning || task.Status == subagent.StatusPending {
		messageID, sendErr := c.manager.Send(ctx, c.parentSession, c.sessionID, message)
		if sendErr == nil {
			task, err = c.manager.Status(c.parentSession, c.sessionID)
			return testAgentTask(task), messageID, err
		}
		if !errors.Is(sendErr, subagent.ErrNotRunning) {
			return AgentTask{}, "", sendErr
		}
	}
	task, err = c.manager.FollowUp(c.parentSession, c.sessionID, subagent.Request{Prompt: message, ToolCallID: toolCallID})
	return testAgentTask(task), "", err
}

func testAgentTask(task subagent.Task) AgentTask {
	return AgentTask{
		SessionID: task.SessionID, Agent: task.Agent, Name: task.Name, Status: string(task.Status),
		Turn: task.Turn, Depth: task.Depth, Output: task.Output, Error: task.Error,
	}
}

func TestAgentToolsReusableLifecycle(t *testing.T) {
	executor := newReusableAgentExecutor()
	manager := subagent.NewManager(executor, subagent.Config{})
	lookup := func(id string) (bool, error) { return id != "build", nil }
	tools := make(map[string]Tool)
	for _, item := range NewAgentTools(managerAgentChildren{manager: manager}, lookup) {
		tools[item.ID()] = item
	}
	call := CallContext{SessionID: "root", Agent: "build", ToolCallID: "call-1"}
	spawn := tools[agentSpawnID]
	if _, err := spawn.Plan(context.Background(), json.RawMessage(`{"prompt":"write","agent":"build"}`), CallContext{SessionID: "root", Agent: "plan"}); err == nil {
		t.Fatal("read-only caller delegated to writable agent")
	}
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

	spawned := execute(agentSpawnID, `{"prompt":"inspect","agent":"explorer","name":"code-review"}`)
	sessionID, ok := spawned.Metadata["session_id"].(string)
	if !ok || sessionID == "" || spawned.Metadata["name"] != "code-review" || spawned.Metadata["task_id"] != nil || spawned.Metadata["status"] != string(subagent.StatusRunning) {
		t.Fatalf("spawned = %#v", spawned)
	}
	first := <-executor.runs
	if first.Request.Prompt != "inspect" || first.Turn != 1 || sessionID != first.SessionID {
		t.Fatalf("first execution = %#v, session ID = %q", first, sessionID)
	}
	id := first.SessionID
	sent := execute(agentSendID, `{"session_id":"code-review","message":"focus"}`)
	if sent.Metadata["message_id"] != "message-1" || sent.Metadata["session_id"] != sessionID || <-executor.sends != sessionID+":focus" {
		t.Fatalf("sent = %#v", sent)
	}
	executor.release(id, "first output")
	if _, err := manager.Await(context.Background(), "root", id); err != nil {
		t.Fatal(err)
	}
	followed := execute(agentSendID, `{"session_id":"code-review","message":"continue"}`)
	if followed.Metadata["turn"] != 2 || followed.Metadata["status"] != string(subagent.StatusRunning) {
		t.Fatalf("followed = %#v", followed)
	}
	second := <-executor.runs
	if second.SessionID != sessionID || second.Request.Prompt != "continue" || second.Turn != 2 {
		t.Fatalf("second execution = %#v", second)
	}
	interrupted, err := manager.Interrupt(context.Background(), "root", id)
	if err != nil || interrupted.Status != subagent.StatusCanceled {
		t.Fatalf("interrupted = %#v, %v", interrupted, err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}

	send := tools[agentSendID]
	if _, err := send.Plan(context.Background(), json.RawMessage(`{"task_id":"`+id+`","message":"legacy"}`), call); err == nil {
		t.Fatal("agent_send accepted task_id")
	}
	presentation := send.Presentation()
	if len(presentation.Label.Fields) != 2 || presentation.Label.Fields[0].Names[0] != "session_id" || !presentation.Label.Fields[0].TaskName {
		t.Fatalf("agent_send presentation = %#v", presentation)
	}
	if schema := string(send.JSONSchema()); !strings.Contains(schema, `"session_id"`) || !strings.Contains(schema, "friendly name") || strings.Contains(schema, `"task_id"`) {
		t.Fatalf("agent_send schema = %s", schema)
	}
	if description := send.Description(); !strings.Contains(description, "friendly name") {
		t.Fatalf("agent_send description = %q", description)
	}
}
