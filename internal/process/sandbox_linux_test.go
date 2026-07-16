//go:build linux

package process

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLinuxSandboxCommand(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	bwrap := filepath.Join(bin, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git", ".parrot", "parrot.jsonc"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("protected"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "parrot.jsonc"), []byte("protected"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+bin)

	implementation := linuxSandbox{workspace: root, workingDirectory: nested}
	program, args, err := implementation.command("/bin/sh", "printf ok", root)
	if err != nil {
		t.Fatal(err)
	}
	if program != bwrap {
		t.Fatalf("program = %q", program)
	}
	for _, expected := range [][]string{
		{"--unshare-user"}, {"--unshare-pid"}, {"--cap-drop", "ALL"},
		{"--ro-bind", "/", "/"}, {"--bind", root, root}, {"--tmpfs", "/tmp"},
		{"--ro-bind", filepath.Join(root, ".git"), filepath.Join(root, ".git")},
		{"--ro-bind", filepath.Join(root, ".parrot"), filepath.Join(root, ".parrot")},
		{"--ro-bind", filepath.Join(root, "parrot.jsonc"), filepath.Join(root, "parrot.jsonc")},
		{"--ro-bind", filepath.Join(nested, "parrot.jsonc"), filepath.Join(nested, "parrot.jsonc")},
		{"--chdir", root, "--", "/bin/sh", "-c", "printf ok"},
	} {
		if !containsSequence(args, expected) {
			t.Fatalf("arguments do not contain %q: %q", expected, args)
		}
	}
	if slices.Contains(args, "--unshare-net") {
		t.Fatalf("network unexpectedly isolated: %q", args)
	}
}

func containsSequence(values, sequence []string) bool {
	for i := 0; i+len(sequence) <= len(values); i++ {
		if slices.Equal(values[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}
