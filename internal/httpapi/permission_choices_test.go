package httpapi

import (
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

// ReplyPermission used to hardcode request_write_permission to reject a scoped
// allow. That rule now comes from the tool's own declared choices. This asserts
// the invariant is unchanged: the escape hatch cannot be widened, and ordinary
// tools keep every standard scope.
func TestDeclaredChoicesPreserveWritePermissionInvariant(t *testing.T) {
	write := tool.ChoicesFor(tool.NewWritePermissionTool(nil))
	ordinary := tool.ChoicesFor(tool.NewReadTool(tool.ReadConfig{}))

	for _, test := range []struct {
		name     string
		choices  []permission.Choice
		decision string
		scope    string
		reason   string
		want     bool
	}{
		{name: "write permission allows once", choices: write, decision: "allow", want: true},
		{name: "write permission refuses session scope", choices: write, decision: "allow", scope: "session"},
		{name: "write permission refuses workspace scope", choices: write, decision: "allow", scope: "workspace"},
		{name: "write permission refuses yolo", choices: write, decision: "allow", scope: "yolo"},
		{name: "write permission denies", choices: write, decision: "deny", want: true},
		{name: "write permission denies with reason", choices: write, decision: "deny", reason: "no", want: true},
		{name: "ordinary tool allows session scope", choices: ordinary, decision: "allow", scope: "session", want: true},
		{name: "ordinary tool allows yolo", choices: ordinary, decision: "allow", scope: "yolo", want: true},
		{name: "allow with a reason is refused", choices: ordinary, decision: "allow", reason: "why"},
		{name: "deny with a scope is refused", choices: ordinary, decision: "deny", scope: "session"},
		{name: "unknown decision is refused", choices: ordinary, decision: "maybe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := replyMatchesChoices(dtoChoices(test.choices), v1.PermissionReply{
				Decision: test.decision, Scope: test.scope, Reason: test.reason,
			}); got != test.want {
				t.Fatalf("accepted = %t, want %t", got, test.want)
			}
		})
	}
}

func dtoChoices(choices []permission.Choice) []v1.PermissionChoice {
	out := make([]v1.PermissionChoice, len(choices))
	for i, choice := range choices {
		out[i] = v1.PermissionChoice{
			Value: choice.Value, Decision: choice.Decision, Scope: choice.Scope,
			Label: choice.Label, Description: choice.Description, RequiresReason: choice.RequiresReason,
		}
	}
	return out
}
