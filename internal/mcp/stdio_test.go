package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestStdioHandshakeDiscoveryConcurrentCallsAndCleanup(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	manager := newStdioManager(t, Config{Env: map[string]string{"MCP_CHILD_PID": pidFile}})
	definitions, err := manager.DiscoverTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 || definitions[0].Name == definitions[1].Name {
		t.Fatalf("collision definitions = %#v", definitions)
	}
	firstNames := []string{definitions[0].Name, definitions[1].Name}
	again, err := manager.DiscoverTools(context.Background())
	if err != nil || again[0].Name != firstNames[0] || again[1].Name != firstNames[1] {
		t.Fatalf("unstable definitions = %#v, %v", again, err)
	}
	var echoName string
	for _, definition := range definitions {
		if definition.Tool == "echo-tool" {
			echoName = definition.Name
		}
	}

	const calls = 20
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value := "value-" + strconv.Itoa(i)
			result, err := manager.CallTool(context.Background(), echoName, json.RawMessage(`{"value":"`+value+`"}`))
			if err != nil || len(result.Content) != 1 || result.Content[0].Text != value {
				errs <- errors.New("concurrent call returned the wrong response")
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	if _, err := manager.CallTool(context.Background(), echoName, json.RawMessage(`{"value":"spawn"}`)); err != nil {
		t.Fatal(err)
	}
	childPID := waitForPID(t, pidFile)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	waitForProcessExit(t, childPID)
}

func TestStdioTimeoutCrashAndRestart(t *testing.T) {
	t.Parallel()
	manager := newStdioManager(t, Config{CallTimeout: 75 * time.Millisecond})
	defer manager.Close()
	definitions, err := manager.DiscoverTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var echoName string
	for _, definition := range definitions {
		if definition.Tool == "echo-tool" {
			echoName = definition.Name
		}
	}
	_, err = manager.CallTool(context.Background(), echoName, json.RawMessage(`{"value":"hang"}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
	_, err = manager.CallTool(context.Background(), echoName, json.RawMessage(`{"value":"crash"}`))
	if err == nil {
		t.Fatal("crashing call unexpectedly succeeded")
	}
	deadline := time.Now().Add(time.Second)
	for {
		status, statusErr := manager.Status("stdio")
		if statusErr != nil {
			t.Fatal(statusErr)
		}
		if status.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not enter failed state: %#v", status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := manager.Restart(context.Background(), "stdio"); err != nil {
		t.Fatal(err)
	}
	result, err := manager.CallTool(context.Background(), echoName, json.RawMessage(`{"value":"after-restart"}`))
	if err != nil || result.Content[0].Text != "after-restart" {
		t.Fatalf("restart result = %#v, %v", result, err)
	}
}

func TestStdioConfigSecurity(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invalid := []Config{
		{Name: "relative", Transport: TransportStdio, Enabled: true, Command: "server"},
		{Name: "unsafe", Transport: TransportStdio, Enabled: true, Command: executable, Env: map[string]string{"LD_PRELOAD": "evil"}},
		{Name: "cwd", Transport: TransportStdio, Enabled: true, Command: executable, Cwd: "relative"},
		{Name: "timeout", Transport: TransportStdio, Enabled: true, Command: executable, CallTimeout: -1},
	}
	for _, config := range invalid {
		if _, err := NewManager([]Config{config}); err == nil {
			t.Fatalf("invalid config accepted: %#v", config)
		}
	}
}

func newStdioManager(t *testing.T, override Config) *Manager {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := map[string]string{"MCP_HELPER_PROCESS": "1"}
	for name, value := range override.Env {
		environment[name] = value
	}
	config := Config{
		Name:            "stdio",
		Transport:       TransportStdio,
		Enabled:         true,
		Command:         executable,
		Args:            []string{"-test.run=^TestMCPHelperProcess$"},
		Env:             environment,
		StartupTimeout:  2 * time.Second,
		CallTimeout:     time.Second,
		MaxMessageBytes: 1 << 20,
		MaxOutputBytes:  64 << 10,
	}
	if override.CallTimeout != 0 {
		config.CallTimeout = override.CallTimeout
	}
	manager, err := NewManager([]Config{config})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("MCP_HELPER_PROCESS") != "1" {
		return
	}
	reader := NewFramedReader(os.Stdin, 1<<20)
	writer := NewFramedWriter(os.Stdout, 1<<20)
	for {
		raw, err := reader.Read()
		if err != nil {
			os.Exit(0)
		}
		var request wireMessage
		if json.Unmarshal(raw, &request) != nil {
			os.Exit(2)
		}
		if len(request.ID) == 0 {
			continue
		}
		go serveHelperRequest(writer, request)
	}
}

func serveHelperRequest(writer *FramedWriter, request wireMessage) {
	var result any
	switch request.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "stdio-fixture", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{
			map[string]any{"name": "echo-tool", "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": "echo tool", "inputSchema": map[string]any{"type": "object"}},
		}}
	case "tools/call":
		var params struct {
			Arguments struct {
				Value string `json:"value"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(request.Params, &params)
		value := params.Arguments.Value
		switch value {
		case "hang":
			select {}
		case "crash":
			os.Exit(23)
		case "spawn":
			command := exec.Command("/bin/sh", "-c", `trap '' TERM; while :; do sleep 1; done`)
			if command.Start() == nil {
				_ = os.WriteFile(os.Getenv("MCP_CHILD_PID"), []byte(strconv.Itoa(command.Process.Pid)), 0600)
			}
		default:
			if strings.HasSuffix(value, "0") {
				time.Sleep(10 * time.Millisecond)
			}
		}
		result = map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": value}},
			"structuredContent": map[string]any{"value": value},
		}
	default:
		_ = writer.Write(wireMessage{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32601, Message: "not found"}})
		return
	}
	raw, _ := json.Marshal(result)
	_ = writer.Write(wireMessage{JSONRPC: "2.0", ID: request.ID, Result: raw})
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("helper child PID was not written")
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper descendant %d survived manager cleanup", pid)
}
