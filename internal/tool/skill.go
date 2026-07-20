package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	"strings"
)

type SkillTool struct {
	BasePresentation
	ReadOnlyTool
	Registry *skill.Registry
}

func NewSkillTool(registry *skill.Registry) *SkillTool { return &SkillTool{Registry: registry} }
func (*SkillTool) ID() string                          { return "skill" }
func (*SkillTool) Presentation() Presentation {
	return Presentation{Label: LabelSpec{Fields: []LabelField{{Names: []string{"name"}}}}}
}

func (t *SkillTool) Description() string {
	items := t.Registry.List()
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name+": "+item.Description)
	}
	if len(names) == 0 {
		return "Load the exact body and execution metadata of a discovered skill."
	}
	return "Load the exact body and execution metadata of a discovered skill. Available skills: " + strings.Join(names, "; ")
}
func (*SkillTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Load skill %q", input.Name), nil
}
func (*SkillTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`)
}
func (t *SkillTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	loaded, err := t.Registry.Load(input.Name)
	if err != nil {
		return Plan{}, err
	}
	review, _ := json.Marshal(map[string]any{"name": loaded.Name, "description": loaded.Description, "agent": loaded.Agent, "model": loaded.Model, "allowed_tools": loaded.AllowedTools})
	return NewPlan(t.ID(), raw, nil, review, loaded)
}
func (t *SkillTool) Execute(_ context.Context, plan Plan, _ CallContext) (Result, error) {
	loaded, ok := plan.Data.(skill.Skill)
	if !ok {
		return Result{}, errors.New("skill: incompatible plan")
	}
	return Result{Text: loaded.Prompt, ModelText: modelText(loaded.Prompt), Metadata: map[string]any{"name": loaded.Name, "description": loaded.Description, "agent": loaded.Agent, "model": loaded.Model, "allowed_tools": loaded.AllowedTools}}, nil
}
