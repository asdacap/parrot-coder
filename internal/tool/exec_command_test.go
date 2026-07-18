package tool

import "testing"

func TestSafePrefixCommandRejectsShellSyntax(t *testing.T) {
	tests := []struct {
		name    string
		command string
		prefix  []string
		want    bool
	}{
		{name: "exact", command: "git pull", prefix: []string{"git", "pull"}, want: true},
		{name: "arguments", command: "git pull --ff-only", prefix: []string{"git", "pull"}, want: true},
		{name: "different command", command: "git push", prefix: []string{"git", "pull"}},
		{name: "and operator", command: "git pull && malicious-command", prefix: []string{"git", "pull"}},
		{name: "substitution", command: "git pull $(malicious-command)", prefix: []string{"git", "pull"}},
		{name: "newline", command: "git pull\nmalicious-command", prefix: []string{"git", "pull"}},
		{name: "unsafe prefix", command: "git pull", prefix: []string{"git", "pull;"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safePrefixCommand(test.command, test.prefix); got != test.want {
				t.Fatalf("safePrefixCommand(%q, %#v) = %t, want %t", test.command, test.prefix, got, test.want)
			}
		})
	}
}
