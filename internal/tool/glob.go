package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type GlobConfig struct {
	MaxResults int
	MaxVisited int
	Timeout    time.Duration
}
type GlobTool struct{ Config GlobConfig }

func NewGlobTool(config GlobConfig) *GlobTool {
	if config.MaxResults <= 0 {
		config.MaxResults = 1000
	}
	if config.MaxVisited <= 0 {
		config.MaxVisited = 100000
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Second
	}
	return &GlobTool{config}
}
func (*GlobTool) ID() string { return "glob" }
func (*GlobTool) Description() string {
	return "Find workspace paths with deterministic glob matching, including **."
}
func (*GlobTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input globInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Find workspace paths matching %q", input.Pattern), nil
}
func (*GlobTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"],"additionalProperties":false}`)
}

type globInput struct {
	Pattern string `json:"pattern"`
}
type globPlan struct {
	Input globInput
	Root  string
	Regex *regexp.Regexp
}

func (t *GlobTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("workspace is required")
	}
	var input globInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	rx, err := compileGlob(input.Pattern)
	if err != nil {
		return Plan{}, err
	}
	root, err := call.Workspace.ResolveRead(".")
	if err != nil {
		return Plan{}, err
	}
	req, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "filesystem", Identifier: root, Operation: "search"}}, nil)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{req}, nil, globPlan{input, root, rx})
}
func (t *GlobTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	p := plan.Data.(globPlan)
	revalidated, err := call.Workspace.ResolveRead(".")
	if err != nil || revalidated != p.Root {
		return Result{}, errors.New("glob root changed after planning")
	}
	ctx, cancel := context.WithTimeout(ctx, t.Config.Timeout)
	defer cancel()
	results := []string{}
	visited := 0
	err = filepath.WalkDir(p.Root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > t.Config.MaxVisited {
			return errors.New("glob traversal limit exceeded")
		}
		if path == p.Root {
			return nil
		}
		rel, err := filepath.Rel(p.Root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if p.Regex.MatchString(rel) {
			if _, err := call.Workspace.ResolveRead(path); err != nil {
				return nil
			}
			results = append(results, rel+map[bool]string{true: "/"}[d.IsDir()])
			if len(results) > t.Config.MaxResults {
				return errors.New("glob result limit exceeded")
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	sort.Strings(results)
	text := strings.Join(results, "\n")
	return Result{Text: text, ModelText: modelText(text), Metadata: map[string]any{"matches": len(results), "visited": visited}}, nil
}
