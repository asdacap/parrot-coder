package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type agentTestChild struct {
	task      AgentTask
	messageID string
	messages  []string
}

func (c *agentTestChild) Status() AgentTask { return c.task }
func (c *agentTestChild) Send(_ context.Context, message AgentMessage) (AgentTask, string, error) {
	c.messages = append(c.messages, message.String())
	return c.task, c.messageID, nil
}

type agentTestSession struct {
	name    string
	child   ChildAgent
	targets map[string]ResolvedAgent
	created struct {
		callerAgent string
		prompt      string
		agent       string
		model       string
		name        string
	}
}

func (*agentTestSession) SessionID() string     { return "caller-session" }
func (s *agentTestSession) SessionName() string { return s.name }
func (*agentTestSession) IsSubagent() bool      { return true }
func (s *agentTestSession) CreateAgent(_ context.Context, callerAgent, prompt, agent, model, name string) (ChildAgent, error) {
	s.created.callerAgent = callerAgent
	s.created.prompt = prompt
	s.created.agent = agent
	s.created.model = model
	s.created.name = name
	return s.child, nil
}
func (s *agentTestSession) ResolveAgent(identifier string) (ResolvedAgent, error) {
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
			child := &agentTestChild{task: AgentTask{SessionID: "canonical-target", Agent: testCase.target, Status: "running"}, messageID: "message-parent"}
			session := &agentTestSession{name: testCase.senderName, targets: map[string]ResolvedAgent{
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

func TestAgentToolsContract(t *testing.T) {
	child := &agentTestChild{
		task:      AgentTask{SessionID: "session-child", Agent: "explorer", Name: "code-review", Status: "blocked", Turn: 1, Depth: 1},
		messageID: "message-1",
	}
	session := &agentTestSession{
		name:  "root",
		child: child,
		targets: map[string]ResolvedAgent{
			"code-review": {Agent: child, Relationship: AgentRelationshipDescendant},
		},
	}
	lookup := func(id string) (bool, error) { return id != "build", nil }
	tools := map[string]Tool{
		agentSpawnID: &AgentTool{Kind: agentSpawnID, Session: session, Agents: lookup},
		agentSendID:  &AgentTool{Kind: agentSendID, Session: session, Agents: lookup},
	}
	call := CallContext{SessionID: "root", Agent: "build", ToolCallID: "call-1"}
	spawn := tools[agentSpawnID]
	if description := spawn.Descriptor().Description; !strings.Contains(description, "final result will automatically be sent to the caller") || !strings.Contains(description, "blocked") || !strings.Contains(description, "interrupt") {
		t.Fatalf("agent_spawn description = %q", description)
	}
	if schema := string(spawn.JSONSchema()); !strings.Contains(schema, "friendly name for easy identification") || !strings.Contains(schema, "inherit the parent's complete selector") || strings.Contains(schema, "UI name") {
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

	spawned := execute(agentSpawnID, `{"prompt":"inspect","agent":"explorer","model":"provider/fast/high","name":"code-review"}`)
	if spawned.Metadata["session_id"] != "session-child" || spawned.Metadata["name"] != "code-review" || spawned.Metadata["status"] != "blocked" || spawned.Metadata["turn"] != 1 {
		t.Fatalf("spawned = %#v", spawned)
	}
	if session.created.callerAgent != "build" || session.created.prompt != "inspect" || session.created.agent != "explorer" || session.created.model != "provider/fast/high" || session.created.name != "code-review" {
		t.Fatalf("created = %#v", session.created)
	}
	sent := execute(agentSendID, `{"session_id":"code-review","message":"focus"}`)
	if sent.Metadata["message_id"] != "message-1" || sent.Metadata["session_id"] != "session-child" || len(child.messages) != 1 || child.messages[0] != "Agent message from root:\n\nfocus" {
		t.Fatalf("sent = %#v, messages = %#v", sent, child.messages)
	}

	send := tools[agentSendID]
	presentation := send.Presentation()
	if len(presentation.Label.Fields) != 2 || presentation.Label.Fields[0].Names[0] != "session_id" || !presentation.Label.Fields[0].TaskName {
		t.Fatalf("agent_send presentation = %#v", presentation)
	}
	if schema := string(send.JSONSchema()); !strings.Contains(schema, `"session_id"`) || !strings.Contains(schema, "friendly name") {
		t.Fatalf("agent_send schema = %s", schema)
	}
	if description := send.Descriptor().Description; !strings.Contains(description, "direct parent or descendant") || !strings.Contains(description, "friendly name") {
		t.Fatalf("agent_send description = %q", description)
	}
	request, err := send.DescribeRequest(json.RawMessage(`{"session_id":"parent","message":"status"}`))
	if err != nil || request != `Send input to agent session "parent"` {
		t.Fatalf("agent_send request = %q, %v", request, err)
	}
}
