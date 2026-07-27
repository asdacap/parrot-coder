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
	HostKey    string
	PID        int
	ProcessKey string
}

type Liveness uint8

const (
	LivenessUnknown Liveness = iota
	LivenessAlive
	LivenessDead
)

// Load returns this process's identity, creating a private stable host key in
// stateDir when this installation does not have one yet.
func Load(stateDir string) (Identity, error) {
	processKey, err := processKey(os.Getpid())
	if err != nil {
		return Identity{}, err
	}
	// Prefer the operating system's machine identity so state shared by multiple
	// hosts does not confuse an unrelated PID for a local owner.
	//
	// The hostname qualifies every key, including this one. A machine ID is not
	// reliably unique: hosts cloned from one image, and containers sharing the
	// host's /etc/machine-id, report the same value. Two such machines would
	// otherwise share a host key, and so would claim each other's sessions and
	// pass each other's ownership checks.
	for _, machineID := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if key, err := os.ReadFile(machineID); err == nil && strings.TrimSpace(string(key)) != "" {
			return Identity{HostKey: qualifiedHostKey(string(key)), PID: os.Getpid(), ProcessKey: processKey}, nil
		}
	}
	path := filepath.Join(stateDir, hostKeyFile)
	key, err := os.ReadFile(path)
	if err == nil && len(key) > 0 {
		return Identity{HostKey: qualifiedHostKey(string(key)), PID: os.Getpid(), ProcessKey: processKey}, nil
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
		return Identity{HostKey: qualifiedHostKey(string(key)), PID: os.Getpid(), ProcessKey: processKey}, nil
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
	return Identity{HostKey: qualifiedHostKey(string(created)), PID: os.Getpid(), ProcessKey: processKey}, nil
}

// Inspect reports whether owner still identifies the same process. Foreign
// hosts and platforms which cannot distinguish PID reuse are conservative:
// unknown ownership never authorizes destructive repair.
func Inspect(local, owner Identity) Liveness {
	if owner == (Identity{}) {
		return LivenessDead // Ownerless rows predate durable ownership.
	}
	if owner.HostKey == "" || owner.PID <= 0 || owner.ProcessKey == "" {
		return LivenessUnknown
	}
	if owner.HostKey != local.HostKey {
		return LivenessUnknown
	}
	if owner == local {
		return LivenessAlive
	}
	return inspectProcess(owner.PID, owner.ProcessKey)
}

func qualifiedHostKey(key string) string {
	hostname, _ := os.Hostname()
	return strings.TrimSpace(hostname) + ":" + strings.TrimSpace(key)
}
