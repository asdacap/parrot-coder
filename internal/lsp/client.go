package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrClosed = errors.New("lsp: client is closed")

type callResult struct {
	result json.RawMessage
	err    error
}

type documentState struct {
	URI        DocumentURI
	LanguageID string
	Text       string
	Version    int
}

type serverProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
	ready  bool
	failed error
}

// Client is a concurrency-safe, restartable connection to one language server.
type Client struct {
	config Config

	startMu  sync.Mutex
	writeMu  sync.Mutex
	changeMu sync.Mutex
	mu       sync.Mutex
	process  *serverProcess
	pending  map[int64]chan callResult
	nextID   int64
	closed   bool
	docs     map[DocumentURI]documentState
	diags    map[DocumentURI][]Diagnostic
}

func NewClient(ctx context.Context, config Config) (*Client, error) {
	config, err := config.normalized()
	if err != nil {
		return nil, err
	}
	c := &Client{
		config:  config,
		pending: make(map[int64]chan callResult),
		docs:    make(map[DocumentURI]documentState),
		diags:   make(map[DocumentURI][]Diagnostic),
	}
	if err := c.ensureProcess(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) ensureProcess(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.process != nil && c.process.ready {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	cmd := exec.Command(c.config.Command, c.config.Args...)
	cmd.Dir = c.config.Workspace
	cmd.Env = controlledEnvironment(c.config.Environment)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("lsp: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("lsp: stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("lsp: stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("lsp: start: %w", err)
	}
	p := &serverProcess{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	c.mu.Lock()
	c.process = p
	c.mu.Unlock()
	go io.Copy(io.Discard, io.LimitReader(stderr, 1<<20))
	go c.readLoop(p, stdout)
	go c.waitLoop(p)

	initCtx, cancel := c.withTimeout(ctx)
	defer cancel()
	params := map[string]any{
		"processId":             cmd.Process.Pid,
		"rootUri":               mustFileURI(c.config.Workspace),
		"workspaceFolders":      []map[string]any{{"uri": mustFileURI(c.config.Workspace), "name": c.config.Name}},
		"capabilities":          map[string]any{},
		"initializationOptions": c.config.InitializationOptions,
	}
	var initialize json.RawMessage
	if err := c.requestCurrent(initCtx, p, "initialize", params, &initialize); err != nil {
		c.failProcess(p, fmt.Errorf("lsp: initialize: %w", err))
		c.terminate(p)
		return err
	}
	if err := c.notifyCurrentContext(initCtx, p, "initialized", map[string]any{}); err != nil {
		c.failProcess(p, err)
		c.terminate(p)
		return err
	}

	c.mu.Lock()
	if c.process != p || p.failed != nil {
		err := p.failed
		c.mu.Unlock()
		if err == nil {
			err = errors.New("lsp: server exited during initialization")
		}
		return err
	}
	p.ready = true
	docs := make([]documentState, 0, len(c.docs))
	for _, doc := range c.docs {
		docs = append(docs, doc)
	}
	c.mu.Unlock()

	// Reopening the latest known document state after a crash is safe. Requests
	// are never replayed because their execution state cannot be known.
	for _, doc := range docs {
		params := didOpenParams(doc)
		if err := c.notifyCurrentContext(initCtx, p, "textDocument/didOpen", params); err != nil {
			c.failProcess(p, err)
			return err
		}
	}
	return nil
}

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= c.config.Timeout {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.config.Timeout)
}

// Call performs an LSP request. A cancelled request sends $/cancelRequest.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if method == "" {
		return errors.New("lsp: method is required")
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	if err := c.ensureProcess(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	p := c.process
	c.mu.Unlock()
	return c.requestCurrent(ctx, p, method, params, result)
}

func (c *Client) requestCurrent(ctx context.Context, p *serverProcess, method string, params, result any) error {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.process != p || p.failed != nil {
		err := p.failed
		c.mu.Unlock()
		if err == nil {
			err = errors.New("lsp: server unavailable")
		}
		return err
	}
	c.nextID++
	id := c.nextID
	response := make(chan callResult, 1)
	c.pending[id] = response
	c.mu.Unlock()

	message := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int64           `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{"2.0", id, method, paramsJSON}
	if err := c.writeMessageContext(ctx, p, message); err != nil {
		c.removePending(id)
		c.failProcess(p, err)
		return fmt.Errorf("lsp: request delivery uncertain: %w", err)
	}
	select {
	case reply := <-response:
		if reply.err != nil {
			return reply.err
		}
		if result != nil && len(reply.result) != 0 && string(reply.result) != "null" {
			if err := json.Unmarshal(reply.result, result); err != nil {
				return fmt.Errorf("lsp: decode response: %w", err)
			}
		}
		return nil
	case <-ctx.Done():
		c.removePending(id)
		cancelCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_ = c.notifyCurrentContext(cancelCtx, p, "$/cancelRequest", map[string]any{"id": id})
		cancel()
		return ctx.Err()
	}
}

func (c *Client) removePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Notify sends an LSP notification.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if err := c.ensureProcess(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	p := c.process
	c.mu.Unlock()
	return c.notifyCurrentContext(ctx, p, method, params)
}

func (c *Client) notifyCurrentContext(ctx context.Context, p *serverProcess, method string, params any) error {
	message := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", method, params}
	if err := c.writeMessageContext(ctx, p, message); err != nil {
		c.failProcess(p, err)
		return err
	}
	return nil
}

func (c *Client) writeMessageContext(ctx context.Context, p *serverProcess, message any) error {
	done := make(chan error, 1)
	go func() { done <- c.writeMessage(p, message) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		c.failProcess(p, ctx.Err())
		c.terminate(p)
		<-done
		return ctx.Err()
	}
}

func (c *Client) writeMessage(p *serverProcess, message any) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if int64(len(body)) > c.config.MaxMessageBytes {
		return errors.New("lsp: message exceeds limit")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	valid := c.process == p && p.failed == nil && !c.closed
	c.mu.Unlock()
	if !valid {
		return errors.New("lsp: server unavailable")
	}
	header := []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)))
	if _, err := p.stdin.Write(header); err != nil {
		return err
	}
	_, err = p.stdin.Write(body)
	return err
}

func (c *Client) readLoop(p *serverProcess, stdout io.Reader) {
	reader := bufio.NewReader(stdout)
	for {
		body, err := readFrame(reader, c.config.MaxMessageBytes)
		if err != nil {
			c.failProcess(p, err)
			c.terminate(p)
			return
		}
		var message rpcMessage
		if err := json.Unmarshal(body, &message); err != nil {
			c.failProcess(p, fmt.Errorf("lsp: invalid JSON: %w", err))
			c.terminate(p)
			return
		}
		if len(message.ID) != 0 && message.Method == "" {
			id, err := strconv.ParseInt(string(message.ID), 10, 64)
			if err == nil {
				c.mu.Lock()
				response := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if response != nil {
					if message.Error != nil {
						response <- callResult{err: message.Error}
					} else {
						response <- callResult{result: message.Result}
					}
				}
			}
			continue
		}
		if message.Method == "textDocument/publishDiagnostics" {
			c.recordDiagnostics(message.Params)
		}
	}
}

func readFrame(reader *bufio.Reader, max int64) ([]byte, error) {
	length := int64(-1)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(line) > 8<<10 {
			return nil, errors.New("lsp: oversized header")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, errors.New("lsp: malformed header")
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return nil, errors.New("lsp: invalid content length")
			}
		}
	}
	if length < 0 || length > max {
		return nil, errors.New("lsp: content length outside limit")
	}
	body := make([]byte, length)
	_, err := io.ReadFull(reader, body)
	return body, err
}

func (c *Client) recordDiagnostics(raw json.RawMessage) {
	var params struct {
		URI         DocumentURI  `json:"uri"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	path, err := PathFromURI(c.config.Workspace, params.URI)
	if err != nil {
		return
	}
	uri, err := FileURI(c.config.Workspace, path)
	if err != nil {
		return
	}
	if len(params.Diagnostics) > c.config.MaxDiagnostics {
		params.Diagnostics = params.Diagnostics[:c.config.MaxDiagnostics]
	}
	diagnostics := append([]Diagnostic(nil), params.Diagnostics...)
	c.mu.Lock()
	c.diags[uri] = diagnostics
	c.mu.Unlock()
}

func (c *Client) failProcess(p *serverProcess, cause error) {
	c.mu.Lock()
	if c.process != p || p.failed != nil {
		c.mu.Unlock()
		return
	}
	p.failed = cause
	p.ready = false
	c.process = nil
	pending := c.pending
	c.pending = make(map[int64]chan callResult)
	c.mu.Unlock()
	for _, response := range pending {
		response <- callResult{err: fmt.Errorf("lsp: server stopped; request not replayed: %w", cause)}
	}
}

func (c *Client) waitLoop(p *serverProcess) {
	err := p.cmd.Wait()
	close(p.done)
	if err == nil {
		err = io.EOF
	}
	c.failProcess(p, err)
}

func (c *Client) terminate(p *serverProcess) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(c.config.ShutdownTimeout):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// Close shuts down the server and its process group. It is idempotent.
func (c *Client) Close() error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	p := c.process
	c.mu.Unlock()
	if p != nil && p.ready {
		ctx, cancel := context.WithTimeout(context.Background(), c.config.ShutdownTimeout)
		_ = c.requestCurrent(ctx, p, "shutdown", nil, nil)
		cancel()
		exitCtx, exitCancel := context.WithTimeout(context.Background(), c.config.ShutdownTimeout)
		_ = c.notifyCurrentContext(exitCtx, p, "exit", nil)
		exitCancel()
	}
	c.mu.Lock()
	c.closed = true
	if c.process == p {
		c.process = nil
	}
	c.mu.Unlock()
	c.terminate(p)
	return nil
}

func mustFileURI(path string) DocumentURI {
	uri, err := FileURI(path, path)
	if err != nil {
		return ""
	}
	return uri
}

func didOpenParams(doc documentState) map[string]any {
	return map[string]any{"textDocument": map[string]any{
		"uri": doc.URI, "languageId": doc.LanguageID, "version": doc.Version, "text": doc.Text,
	}}
}

// EncodeFrame is useful for protocol helpers and tests.
func EncodeFrame(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var framed bytes.Buffer
	fmt.Fprintf(&framed, "Content-Length: %d\r\n\r\n", len(body))
	framed.Write(body)
	return framed.Bytes(), nil
}
