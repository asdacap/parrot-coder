package httpapi

import (
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

// A reply must be one of the answers the requesting tool declared, and a reason
// is accepted only for a choice which asks for one.
func TestReplyMatchesDeclaredChoices(t *testing.T) {
	write := tool.ChoicesFor(tool.NewWritePermissionTool(nil))
	ordinary := tool.ChoicesFor(tool.NewReadTool(tool.ReadConfig{}))

	for _, test := range []struct {
		name     string
		choices  []permission.Choice
		decision string
		reason   string
		want     bool
	}{
		{name: "write permission grants", choices: write, decision: "allow", want: true},
		{name: "write permission rejects", choices: write, decision: "deny", want: true},
		{name: "write permission rejects with reason", choices: write, decision: "deny", reason: "no", want: true},
		{name: "write permission refuses allow with a reason", choices: write, decision: "allow", reason: "why"},
		{name: "ordinary tool allows", choices: ordinary, decision: "allow", want: true},
		{name: "ordinary tool denies", choices: ordinary, decision: "deny", want: true},
		{name: "ordinary tool denies with reason", choices: ordinary, decision: "deny", reason: "no", want: true},
		{name: "allow with a reason is refused", choices: ordinary, decision: "allow", reason: "why"},
		{name: "unknown decision is refused", choices: ordinary, decision: "maybe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := replyMatchesChoices(dtoChoices(test.choices), v1.PermissionReply{
				Decision: test.decision, Reason: test.reason,
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
			Value: choice.Value, Decision: choice.Decision,
			Label: choice.Label, Description: choice.Description, RequiresReason: choice.RequiresReason,
		}
	}
	return out
}
