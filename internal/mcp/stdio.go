package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type stdioEndpoint struct {
	command  *exec.Cmd
	stdin    io.WriteCloser
	peer     *rpcPeer
	waitDone chan struct{}
	waitMu   sync.Mutex
	waitErr  error
	stopOnce sync.Once
}

func startStdio(config Config) (*stdioEndpoint, error) {
	command := exec.Command(config.Command, config.Args...)
	command.Dir = config.Cwd
	command.Env = controlledEnvironment(config.Env)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: create stdin pipe: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp: create stdout pipe: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("mcp: create stderr pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("mcp: start stdio server: %w", err)
	}
	e := &stdioEndpoint{command: command, stdin: stdin, waitDone: make(chan struct{})}
	e.peer = newRPCPeer(config.Name, stdout, stdin, config.MaxMessageBytes)
	go io.Copy(io.Discard, stderr)
	go func() {
		err := command.Wait()
		e.waitMu.Lock()
		e.waitErr = err
		e.waitMu.Unlock()
		// A server parent can exit while leaving descendants and inherited pipes
		// alive. Tear down the complete group and wake pending callers promptly.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		e.peer.close(fmt.Errorf("mcp: stdio server exited: %w", err))
		close(e.waitDone)
	}()
	return e, nil
}

func (e *stdioEndpoint) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return e.peer.call(ctx, method, params)
}

func (e *stdioEndpoint) notify(ctx context.Context, method string, params any) error {
	done := make(chan error, 1)
	go func() { done <- e.peer.notify(method, params) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *stdioEndpoint) setProtocolVersion(string) {}

func (e *stdioEndpoint) close(ctx context.Context) error {
	e.stopOnce.Do(func() {
		_ = e.stdin.Close()
		if e.command.Process != nil {
			_ = syscall.Kill(-e.command.Process.Pid, syscall.SIGTERM)
		}
	})
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-e.waitDone:
	case <-ctx.Done():
		if e.command.Process != nil {
			_ = syscall.Kill(-e.command.Process.Pid, syscall.SIGKILL)
		}
		<-e.waitDone
		return ctx.Err()
	case <-timer.C:
		if e.command.Process != nil {
			_ = syscall.Kill(-e.command.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-e.waitDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.waitMu.Lock()
	err := e.waitErr
	e.waitMu.Unlock()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return err
	}
	return nil
}

func (e *stdioEndpoint) done() <-chan struct{}                    { return e.peer.done }
func (e *stdioEndpoint) notificationChannel() <-chan Notification { return e.peer.notifications }
func (e *stdioEndpoint) pid() int {
	if e.command.Process == nil {
		return 0
	}
	return e.command.Process.Pid
}
