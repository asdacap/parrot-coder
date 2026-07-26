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

func (c managerAgentChildren) Create(ctx context.Context, parentSession, callerAgent, prompt, agent, model, name string) (ChildAgent, error) {
	sessionID, err := c.manager.Spawn(ctx, parentSession, callerAgent, subagent.Request{
		Prompt: prompt, Agent: agent, Model: model, Name: name,
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

func (c managerChildAgent) Status() AgentTask {
	task, _ := c.manager.Status(c.parentSession, c.sessionID)
	return testAgentTask(task)
}

func (c managerChildAgent) Send(ctx context.Context, message AgentMessage) (AgentTask, string, error) {
	task, err := c.manager.Status(c.parentSession, c.sessionID)
	if err != nil {
		return AgentTask{}, "", err
	}
	if task.Status == subagent.StatusRunning || task.Status == subagent.StatusPending {
		messageID, sendErr := c.manager.SendAttributed(ctx, c.parentSession, c.sessionID, message.String(), message.Content)
		if sendErr == nil {
			task, err = c.manager.Status(c.parentSession, c.sessionID)
			return testAgentTask(task), messageID, err
		}
		if !errors.Is(sendErr, subagent.ErrNotRunning) {
			return AgentTask{}, "", sendErr
		}
	}
	task, err = c.manager.FollowUpAttributed(c.parentSession, c.sessionID, subagent.Request{Prompt: message.String()}, message.Content)
	return testAgentTask(task), "", err
}

func testAgentTask(task subagent.Task) AgentTask {
	return AgentTask{
		SessionID: task.SessionID, Agent: task.Agent, Name: task.Name, Status: string(task.Status),
		Turn: task.Turn, Depth: task.Depth, Output: task.Output, Error: task.Error,
	}
}

type agentSendTestChild struct {
	task     AgentTask
	messages []string
}

func (c *agentSendTestChild) Status() AgentTask { return c.task }
func (c *agentSendTestChild) Send(_ context.Context, message AgentMessage) (AgentTask, string, error) {
	c.messages = append(c.messages, message.String())
	return c.task, "message-parent", nil
}

type agentSendTestSession struct {
	name    string
	targets map[string]ResolvedAgent
}

func (s *agentSendTestSession) SessionID() string   { return "caller-session" }
func (s *agentSendTestSession) SessionName() string { return s.name }
func (*agentSendTestSession) IsSubagent() bool      { return true }
func (*agentSendTestSession) CreateAgent(context.Context, string, string, string, string, string) (ChildAgent, error) {
	return nil, errors.New("not implemented")
}
func (s *agentSendTestSession) ResolveAgent(identifier string) (ResolvedAgent, error) {
	target, ok := s.targets[identifier]
	if !ok {
		return ResolvedAgent{}, errors.New("not found")
	}
	return target, nil
}

func TestAgentSendTrustRelationshipMatrix(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		caller       string
		target       string
		senderName   string
		relationship AgentRelationship
		wantError    bool
	}{
		{name: "writable caller to writable descendant", caller: "build", target: "build", senderName: "trusted-sender", relationship: AgentRelationshipDescendant},
		{name: "read-only caller to read-only descendant", caller: "explorer", target: "explorer", senderName: "trusted-sender", relationship: AgentRelationshipDescendant},
		{name: "read-only caller to writable descendant", caller: "explorer", target: "build", senderName: "trusted-sender", relationship: AgentRelationshipDescendant, wantError: true},
		{name: "read-only child to writable direct parent", caller: "explorer", target: "build", senderName: "trusted-sender", relationship: AgentRelationshipParent},
		{name: "empty friendly name falls back to canonical caller", caller: "build", target: "build", relationship: AgentRelationshipDescendant},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			child := &agentSendTestChild{task: AgentTask{SessionID: "canonical-target", Agent: testCase.target, Status: "running"}}
			session := &agentSendTestSession{name: testCase.senderName, targets: map[string]ResolvedAgent{
				"friendly-target": {Agent: child, Relationship: testCase.relationship},
			}}
			send := &AgentTool{Kind: agentSendID, Session: session, Agents: func(agent string) (bool, error) {
				return agent == "explorer", nil
			}}
			call := CallContext{SessionID: session.SessionID(), Agent: testCase.caller}
			plan, err := send.Plan(context.Background(), json.RawMessage(`{"session_id":"friendly-target","message":"exact\nmessage"}`), call)
			if testCase.wantError {
				if err == nil || !strings.Contains(err.Error(), "cannot message writable agent") {
					t.Fatalf("Plan() error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			input := plan.Data.(agentInput)
			if input.SessionID != "canonical-target" || input.Target.Relationship != testCase.relationship {
				t.Fatalf("planned input = %#v", input)
			}
			delete(session.targets, "friendly-target")
			result, err := send.Execute(context.Background(), plan, call)
			if err != nil {
				t.Fatal(err)
			}
			wantSender := testCase.senderName
			if wantSender == "" {
				wantSender = "caller-session"
			}
			wantMessage := "Agent message from " + wantSender + ":\n\nexact\nmessage"
			if len(child.messages) != 1 || child.messages[0] != wantMessage || result.Metadata["message_id"] != "message-parent" || result.Metadata["session_id"] != "canonical-target" {
				t.Fatalf("messages = %#v, result = %#v", child.messages, result)
			}
		})
	}
}

func TestAgentToolsReusableLifecycle(t *testing.T) {
	executor := newReusableAgentExecutor()
	// The caller content, rather than the trusted attribution envelope, consumes
	// the configured prompt allowance for both steering and follow-up turns.
	manager := subagent.NewManager(executor, subagent.Config{MaxPromptBytes: len("continue")})
	lookup := func(id string) (bool, error) { return id != "build", nil }
	tools := make(map[string]Tool)
	for _, item := range NewAgentTools(managerAgentChildren{manager: manager}, lookup) {
		tools[item.ID()] = item
	}
	call := CallContext{SessionID: "root", Agent: "build", ToolCallID: "call-1"}
	spawn := tools[agentSpawnID]
	if description := spawn.Description(); !strings.Contains(description, "final result will automatically be sent to the caller") {
		t.Fatalf("agent_spawn description = %q", description)
	}
	if schema := string(spawn.JSONSchema()); !strings.Contains(schema, "friendly name for easy identification") || strings.Contains(schema, "UI name") {
		t.Fatalf("agent_spawn schema = %s", schema)
	}
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
	if sent.Metadata["message_id"] != "message-1" || sent.Metadata["session_id"] != sessionID || <-executor.sends != sessionID+":Agent message from root:\n\nfocus" {
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
	if second.SessionID != sessionID || second.Request.Prompt != "Agent message from root:\n\ncontinue" || second.Turn != 2 {
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
	if description := send.Description(); !strings.Contains(description, "direct parent or descendant") || !strings.Contains(description, "friendly name") {
		t.Fatalf("agent_send description = %q", description)
	}
	request, err := send.DescribeRequest(json.RawMessage(`{"session_id":"parent","message":"status"}`))
	if err != nil || request != `Send input to agent session "parent"` {
		t.Fatalf("agent_send request = %q, %v", request, err)
	}
}
