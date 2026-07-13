// Package appdirs resolves and creates Parrot's platform directories.
package appdirs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const appName = "parrot"

// Overrides makes directory resolution deterministic for callers and tests.
// Empty fields use the corresponding environment variable and then the XDG
// default below Home.
type Overrides struct {
	Home       string
	ConfigHome string
	DataHome   string
	StateHome  string
	CacheHome  string
}

// Paths contains application-specific, absolute XDG paths.
type Paths struct {
	Config string
	Data   string
	State  string
	Cache  string
}

// Resolve computes Parrot's XDG paths without touching the filesystem.
func Resolve(overrides Overrides) (Paths, error) {
	home := overrides.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolve home directory: %w", err)
		}
	}
	if home == "" || !filepath.IsAbs(home) {
		return Paths{}, errors.New("home directory must be absolute")
	}

	config, err := xdgHome(overrides.ConfigHome, "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if err != nil {
		return Paths{}, err
	}
	data, err := xdgHome(overrides.DataHome, "XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	if err != nil {
		return Paths{}, err
	}
	state, err := xdgHome(overrides.StateHome, "XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	if err != nil {
		return Paths{}, err
	}
	cache, err := xdgHome(overrides.CacheHome, "XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if err != nil {
		return Paths{}, err
	}

	return Paths{
		Config: filepath.Join(config, appName),
		Data:   filepath.Join(data, appName),
		State:  filepath.Join(state, appName),
		Cache:  filepath.Join(cache, appName),
	}, nil
}

func xdgHome(override, environment, fallback string) (string, error) {
	dir := override
	if dir == "" {
		dir = os.Getenv(environment)
	}
	if dir == "" {
		dir = fallback
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s must be absolute", environment)
	}
	return filepath.Clean(dir), nil
}

// Ensure creates every application directory with private permissions. It also
// tightens permissions on an existing final directory.
func (p Paths) Ensure() error {
	for _, dir := range []string{p.Config, p.Data, p.State, p.Cache} {
		if dir == "" || !filepath.IsAbs(dir) {
			return fmt.Errorf("application directory must be absolute: %q", dir)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create application directory %q: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure application directory %q: %w", dir, err)
		}
	}
	return nil
}

// ResolveAndEnsure resolves the paths and creates them.
func ResolveAndEnsure(overrides Overrides) (Paths, error) {
	paths, err := Resolve(overrides)
	if err != nil {
		return Paths{}, err
	}
	if err := paths.Ensure(); err != nil {
		return Paths{}, err
	}
	return paths, nil
}
