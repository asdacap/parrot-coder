package mode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/security"
)

type profileWithRules struct {
	agent.Profile
	rules []security.Rule
}

type typedNilModeProfile struct{ agent.Profile }

func (p profileWithRules) Rules() []security.Rule { return append([]security.Rule(nil), p.rules...) }

func testModeProfiles() []agent.Profile {
	return []agent.Profile{
		agent.NewProfile(BuildID, "build", "usage", []string{"rule"}, nil, 64, 3, false, nil),
		agent.NewProfile(PlanID, "plan profile", "usage", []string{"rule"}, nil, 24, 1, true, nil),
		agent.NewProfile(QueryID, "query", "usage", []string{"rule"}, nil, 24, 1, true, nil),
	}
}

func testModeRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(Builtins(testModeProfiles()...)...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuiltinsExposeOnlyForegroundModes(t *testing.T) {
	r := testModeRegistry(t)
	items := r.List()
	if len(items) != 3 || items[0].ID() != BuildID || items[1].ID() != PlanID || items[2].ID() != QueryID {
		t.Fatalf("modes = %#v", items)
	}
	if items[0].Profile().IsReadOnly() || !items[1].Profile().IsReadOnly() || !items[2].Profile().IsReadOnly() {
		t.Fatal("unexpected mode policies")
	}
	if _, err := r.Get("explorer"); err == nil {
		t.Fatal("explorer exposed as foreground mode")
	}
	var typedNil *typedNilModeProfile
	if _, err := NewRegistry(builtin{profile: typedNil}); err == nil {
		t.Fatal("typed-nil mode profile was accepted")
	}
}

func TestPlanPrepareTurnCreatesWritableArtifactAndPreservesRevisions(t *testing.T) {
	directory := t.TempDir()
	r, err := NewRegistryWithPlanDirectory(directory, testModeProfiles()...)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Get(PlanID)
	if err != nil {
		t.Fatal(err)
	}
	baseRule := security.Rule{Path: "/protected", Action: security.ActionDenyWrite}
	plan.(*planMode).profile = profileWithRules{Profile: plan.Profile(), rules: []security.Rule{baseRule}}
	prepared, err := plan.OnTurnStart("session")
	if err != nil {
		t.Fatal(err)
	}
	profile := prepared.Profile()
	capabilities := prepared.CapabilityRules()
	if rules := profile.Rules(); len(rules) != 1 || rules[0] != baseRule || len(capabilities) != 1 || capabilities[0] != (security.Rule{Path: directory, Action: security.ActionAllowWrite}) {
		t.Fatalf("plan profile = %#v, capabilities = %#v", profile, capabilities)
	}
	path := plan.(*planMode).files["session"]
	if filepath.Dir(path) != directory || filepath.Ext(path) != ".md" || !strings.HasPrefix(filepath.Base(path), "plan-") {
		t.Fatalf("canonical plan path = %q", path)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("initial plan artifact = %#v, %v", info, err)
	}
	supporting := filepath.Join(directory, "supporting", "details.md")
	if !strings.Contains(profile.Prompt(), path) || !strings.Contains(profile.Prompt(), directory) || !security.CanWrite(prepared, path) || !security.CanWrite(prepared, supporting) || security.CanWrite(prepared, filepath.Join(filepath.Dir(directory), "outside.md")) {
		t.Fatalf("plan prompt or write capability = %q, %#v", profile.Prompt(), capabilities)
	}
	const existingPlan = "existing plan"
	if err := os.MkdirAll(filepath.Dir(supporting), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(existingPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(supporting, []byte("supporting artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	completed, err := plan.OnTurnFinish("session", "")
	if err != nil || len(completed) != 1 {
		t.Fatalf("plan completion = %#v, %v", completed, err)
	}
	payload, ok := completed[0].Payload.(event.PlanCompletedPayload)
	if !ok || payload.Markdown != existingPlan {
		t.Fatalf("plan completion payload = %#v", completed[0].Payload)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	revised, err := plan.OnTurnStart("session")
	if err != nil {
		t.Fatal(err)
	}
	revisedProfile := revised.Profile()
	revisedCapabilities := revised.CapabilityRules()
	if rules := revisedProfile.Rules(); len(rules) != 1 || rules[0] != baseRule || len(revisedCapabilities) != 1 || revisedCapabilities[0] != (security.Rule{Path: directory, Action: security.ActionAllowWrite}) {
		t.Fatalf("revised security layers = base %#v, capabilities %#v", revisedProfile.Rules(), revisedCapabilities)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != int64(len(existingPlan)) || info.Mode().Perm() != 0o600 {
		t.Fatalf("revised plan artifact = %#v, %v", info, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != existingPlan {
		t.Fatalf("revised plan contents = %q, %v", data, err)
	}
	if _, err := plan.OnTurnStart("other-session"); err != nil {
		t.Fatal(err)
	}
	if other := plan.(*planMode).files["other-session"]; other == path || filepath.Dir(other) != directory {
		t.Fatalf("second session plan path = %q, want a distinct child of %q", other, directory)
	}
}

func TestModesFinishAndPlanEvent(t *testing.T) {
	r, err := NewRegistryWithPlanDirectory(t.TempDir(), testModeProfiles()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{BuildID, QueryID} {
		item, _ := r.Get(id)
		events, err := item.OnTurnFinish("session", "message")
		if err != nil || len(events) != 0 {
			t.Fatalf("%s finish = %#v, %v", id, events, err)
		}
	}
	plan, _ := r.Get(PlanID)
	if _, err := plan.OnTurnStart("session"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.(*planMode).files["session"], []byte("  # Plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := plan.OnTurnFinish("session", "message")
	if err != nil || len(events) != 1 {
		t.Fatalf("plan finish = %#v, %v", events, err)
	}
	payload, ok := events[0].Payload.(event.PlanCompletedPayload)
	if !ok || payload.SessionID != "session" || payload.MessageID != "message" || payload.Markdown != "# Plan" || payload.Dialog.Choices[0].Action.Agent != BuildID {
		t.Fatalf("plan payload = %#v", events[0].Payload)
	}
}
