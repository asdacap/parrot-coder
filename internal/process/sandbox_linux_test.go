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
	for _, name := range []string{".parrot", "parrot.jsonc"} {
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
	writable := filepath.Join(t.TempDir(), "granted")
	if err := os.WriteFile(writable, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	externalCwd := t.TempDir()
	temporaryDirectory := t.TempDir()

	implementation := linuxSandbox{workspace: root, workingDirectory: nested}
	program, args, err := implementation.command("/bin/sh", "printf ok", externalCwd, []string{writable}, temporaryDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if program != bwrap {
		t.Fatalf("program = %q", program)
	}
	for _, expected := range [][]string{
		{"--unshare-user"}, {"--unshare-pid"}, {"--cap-drop", "ALL"},
		{"--ro-bind", "/", "/"}, {"--bind", root, root}, {"--bind", temporaryDirectory, "/tmp"},
		{"--bind", writable, writable},
		{"--dir", externalCwd, "--ro-bind", externalCwd, externalCwd},
		{"--ro-bind", filepath.Join(root, ".parrot"), filepath.Join(root, ".parrot")},
		{"--ro-bind", filepath.Join(root, "parrot.jsonc"), filepath.Join(root, "parrot.jsonc")},
		{"--ro-bind", filepath.Join(nested, "parrot.jsonc"), filepath.Join(nested, "parrot.jsonc")},
		{"--chdir", externalCwd, "--", "/bin/sh", "-c", "printf ok"},
	} {
		if !containsSequence(args, expected) {
			t.Fatalf("arguments do not contain %q: %q", expected, args)
		}
	}
	if slices.Contains(args, "--unshare-net") {
		t.Fatalf("network unexpectedly isolated: %q", args)
	}
}

func TestLinuxSandboxMountsExternalWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	bin := t.TempDir()
	bwrap := filepath.Join(bin, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	external := t.TempDir()

	_, args, err := (linuxSandbox{workspace: root, workingDirectory: root}).command("/bin/sh", "pwd", external, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !containsSequence(args, []string{"--ro-bind", external, external}) {
		t.Fatalf("external cwd is not mounted read-only: %q", args)
	}
	if indexSequence(args, []string{"--ro-bind", external, external}) > indexSequence(args, []string{"--bind", root, root}) {
		t.Fatalf("workspace must be mounted after external cwd: %q", args)
	}

	_, args, err = (linuxSandbox{workspace: root, workingDirectory: root}).command("/bin/sh", "pwd", filepath.Dir(root), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ancestorMount := indexSequence(args, []string{"--ro-bind", filepath.Dir(root), filepath.Dir(root)})
	if ancestorMount >= 0 && ancestorMount > indexSequence(args, []string{"--bind", root, root}) {
		t.Fatalf("workspace must be mounted after its cwd ancestor: %q", args)
	}
}

func TestLinuxSandboxAllowsLinkedWorktreeGitMetadata(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "worktree")
	common := filepath.Join(base, "repository", ".git")
	gitDirectory := filepath.Join(common, "worktrees", "worktree")
	bin := filepath.Join(base, "bin")
	for _, directory := range []string{root, gitDirectory, bin} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		filepath.Join(root, ".git"):              "gitdir: " + gitDirectory + "\n",
		filepath.Join(gitDirectory, "gitdir"):    filepath.Join(root, ".git") + "\n",
		filepath.Join(gitDirectory, "commondir"): "../..\n",
		filepath.Join(bin, "bwrap"):              "#!/bin/sh\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)

	_, args, err := (linuxSandbox{workspace: root, workingDirectory: root}).command("/bin/sh", "git commit", root, nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !containsSequence(args, []string{"--bind", common, common}) {
		t.Fatalf("arguments do not make linked Git metadata writable: %q", args)
	}
	if containsSequence(args, []string{"--ro-bind", filepath.Join(root, ".git"), filepath.Join(root, ".git")}) {
		t.Fatalf("worktree .git file unexpectedly read-only: %q", args)
	}
}

func TestLinuxSandboxDoesNotShadowPrivateTmp(t *testing.T) {
	bin := t.TempDir()
	bwrap := filepath.Join(bin, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	implementation := linuxSandbox{workspace: root, workingDirectory: root}
	if _, _, err := implementation.command("/bin/sh", "true", "/tmp", nil, t.TempDir()); err == nil {
		t.Fatal("host /tmp accepted as working directory")
	}
	_, args, err := implementation.command("/bin/sh", "true", "/", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if containsSequence(args, []string{"--ro-bind", "/", "/"}) && countSequence(args, []string{"--ro-bind", "/", "/"}) != 1 {
		t.Fatalf("host root remounted over synthetic mounts: %q", args)
	}
}

func containsSequence(values, sequence []string) bool {
	return indexSequence(values, sequence) >= 0
}

func indexSequence(values, sequence []string) int {
	for i := 0; i+len(sequence) <= len(values); i++ {
		if slices.Equal(values[i:i+len(sequence)], sequence) {
			return i
		}
	}
	return -1
}

func countSequence(values, sequence []string) int {
	count := 0
	for i := 0; i+len(sequence) <= len(values); i++ {
		if slices.Equal(values[i:i+len(sequence)], sequence) {
			count++
		}
	}
	return count
}
