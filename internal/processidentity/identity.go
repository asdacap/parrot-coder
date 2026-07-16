// Package processidentity provides the stable host and process identity used
// to associate an interactive terminal with its durable session.
package processidentity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const hostKeyFile = "host-key"

type Identity struct {
	HostKey string
	PID     int
}

// Load returns this process's identity, creating a private stable host key in
// stateDir when this installation does not have one yet.
func Load(stateDir string) (Identity, error) {
	// Prefer the operating system's machine identity so a database shared by
	// multiple hosts does not confuse an unrelated PID for a local owner.
	for _, machineID := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if key, err := os.ReadFile(machineID); err == nil && strings.TrimSpace(string(key)) != "" {
			return Identity{HostKey: strings.TrimSpace(string(key)), PID: os.Getpid()}, nil
		}
	}
	path := filepath.Join(stateDir, hostKeyFile)
	key, err := os.ReadFile(path)
	if err == nil && len(key) > 0 {
		return Identity{HostKey: fallbackHostKey(string(key)), PID: os.Getpid()}, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Identity{}, fmt.Errorf("process identity: read host key: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Identity{}, fmt.Errorf("process identity: generate host key: %w", err)
	}
	created := []byte(hex.EncodeToString(raw))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		key, err = os.ReadFile(path)
		if err != nil || len(key) == 0 {
			return Identity{}, fmt.Errorf("process identity: read concurrent host key: %w", err)
		}
		return Identity{HostKey: fallbackHostKey(string(key)), PID: os.Getpid()}, nil
	}
	if err != nil {
		return Identity{}, fmt.Errorf("process identity: create host key: %w", err)
	}
	if _, err = file.Write(created); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return Identity{}, fmt.Errorf("process identity: write host key: %w", err)
	}
	if err = file.Close(); err != nil {
		return Identity{}, fmt.Errorf("process identity: close host key: %w", err)
	}
	return Identity{HostKey: fallbackHostKey(string(created)), PID: os.Getpid()}, nil
}

func fallbackHostKey(key string) string {
	hostname, _ := os.Hostname()
	return strings.TrimSpace(hostname) + ":" + strings.TrimSpace(key)
}
