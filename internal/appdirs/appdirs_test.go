package appdirs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	home := filepath.Join(t.TempDir(), "home")

	got, err := Resolve(Overrides{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Config: filepath.Join(home, ".config", "parrot"),
		Data:   filepath.Join(home, ".local", "share", "parrot"),
		State:  filepath.Join(home, ".local", "state", "parrot"),
		Cache:  filepath.Join(home, ".cache", "parrot"),
	}
	if got != want {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
}

func TestResolveOverridesEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "environment"))
	paths, err := Resolve(Overrides{
		Home:       root,
		ConfigHome: filepath.Join(root, "explicit"),
		DataHome:   filepath.Join(root, "data"),
		StateHome:  filepath.Join(root, "state"),
		CacheHome:  filepath.Join(root, "cache"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if paths.Config != filepath.Join(root, "explicit", "parrot") {
		t.Fatalf("Config = %q", paths.Config)
	}
}

func TestResolveRejectsRelativeXDGPath(t *testing.T) {
	_, err := Resolve(Overrides{Home: t.TempDir(), ConfigHome: "relative"})
	if err == nil {
		t.Fatal("Resolve accepted a relative path")
	}
}

func TestEnsureCreatesPrivateDirectories(t *testing.T) {
	root := t.TempDir()
	paths := Paths{
		Config: filepath.Join(root, "config"),
		Data:   filepath.Join(root, "data"),
		State:  filepath.Join(root, "state"),
		Cache:  filepath.Join(root, "cache"),
	}
	if err := os.Mkdir(paths.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{paths.Config, paths.Data, paths.State, paths.Cache} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, got)
		}
	}
}
