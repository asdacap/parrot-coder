package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

type writeResult struct {
	written int
	err     error
}

type dispatchError struct {
	err  error
	safe bool
}

func (e *dispatchError) Error() string { return e.err.Error() }
func (e *dispatchError) Unwrap() error { return e.err }

type rpcPeer struct {
	reader        *FramedReader
	writer        *FramedWriter
	nextID        atomic.Int64
	mu            sync.Mutex
	pending       map[int64]chan rpcResponse
	closed        bool
	closeErr      error
	done          chan struct{}
	notifications chan Notification
	server        string
}

func newRPCPeer(server string, input io.Reader, output io.Writer, max int64) *rpcPeer {
	p := &rpcPeer{
		reader:        NewFramedReader(input, max),
		writer:        NewFramedWriter(output, max),
		pending:       make(map[int64]chan rpcResponse),
		done:          make(chan struct{}),
		notifications: make(chan Notification, 64),
		server:        server,
	}
	go p.readLoop()
	return p
}

func (p *rpcPeer) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	id := p.nextID.Add(1)
	response := make(chan rpcResponse, 1)
	p.mu.Lock()
	if p.closed {
		err := p.closeErr
		p.mu.Unlock()
		return nil, &dispatchError{err: err, safe: true}
	}
	p.pending[id] = response
	p.mu.Unlock()

	message := wireMessage{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: paramsJSON}
	body, err := json.Marshal(message)
	if err != nil {
		p.removePending(id)
		return nil, err
	}
	dispatched := make(chan writeResult, 1)
	go func() {
		written, writeErr := p.writer.writeBodyContext(ctx, body)
		dispatched <- writeResult{written: written, err: writeErr}
	}()
	select {
	case outcome := <-dispatched:
		if outcome.err != nil {
			p.removePending(id)
			return nil, &dispatchError{err: outcome.err, safe: outcome.written == 0}
		}
	case <-ctx.Done():
		if p.removePending(id) {
			go p.notify("notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
		}
		return nil, ctx.Err()
	case <-p.done:
		p.removePending(id)
		return nil, p.closeErr
	}
	select {
	case item := <-response:
		return item.result, item.err
	case <-ctx.Done():
		if p.removePending(id) {
			go p.notify("notifications/cancelled", map[string]any{"requestId": id, "reason": ctx.Err().Error()})
		}
		return nil, ctx.Err()
	case <-p.done:
		// close() delivers to every request before closing done. This branch is
		// retained for callers racing request registration and shutdown.
		select {
		case item := <-response:
			return item.result, item.err
		default:
			return nil, p.closeErr
		}
	}
}

func (p *rpcPeer) notify(method string, params any) error {
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return err
	}
	body, err := json.Marshal(wireMessage{JSONRPC: "2.0", Method: method, Params: paramsJSON})
	if err != nil {
		return err
	}
	_, err = p.writer.writeBody(body)
	return err
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		if !json.Valid(raw) {
			return nil, errors.New("mcp: params are not valid JSON")
		}
		return append(json.RawMessage(nil), raw...), nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode params: %w", err)
	}
	return raw, nil
}

func (p *rpcPeer) removePending(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.pending[id]; !ok {
		return false
	}
	delete(p.pending, id)
	return true
}

func (p *rpcPeer) readLoop() {
	for {
		raw, err := p.reader.Read()
		if err != nil {
			p.close(err)
			return
		}
		var message wireMessage
		if err := json.Unmarshal(raw, &message); err != nil || message.JSONRPC != "2.0" {
			p.close(errors.New("mcp: invalid JSON-RPC message"))
			return
		}
		if len(message.ID) != 0 && message.Method == "" {
			p.handleResponse(message)
			continue
		}
		if message.Method == "" {
			p.close(errors.New("mcp: JSON-RPC message has neither method nor response"))
			return
		}
		if len(message.ID) != 0 {
			p.rejectServerRequest(message)
			continue
		}
		item := Notification{Server: p.server, Method: message.Method, Params: append(json.RawMessage(nil), message.Params...)}
		select {
		case p.notifications <- item:
		default:
		}
	}
}

func (p *rpcPeer) handleResponse(message wireMessage) {
	var id int64
	if err := json.Unmarshal(message.ID, &id); err != nil {
		p.close(errors.New("mcp: response has an invalid request ID"))
		return
	}
	p.mu.Lock()
	response, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	p.mu.Unlock()
	if !ok {
		return
	}
	if message.Error != nil {
		response <- rpcResponse{err: message.Error}
		return
	}
	if message.Result == nil {
		response <- rpcResponse{err: errors.New("mcp: response contains neither result nor error")}
		return
	}
	response <- rpcResponse{result: append(json.RawMessage(nil), message.Result...)}
}

func (p *rpcPeer) rejectServerRequest(message wireMessage) {
	detail := "client-side requests are unsupported"
	if message.Method == "elicitation/create" {
		detail = "elicitation is unsupported"
	} else if message.Method == "sampling/createMessage" {
		detail = "sampling is unsupported"
	}
	response := wireMessage{
		JSONRPC: "2.0",
		ID:      append(json.RawMessage(nil), message.ID...),
		Error:   &RPCError{Code: -32601, Message: detail},
	}
	_ = p.writer.Write(response)
}

func (p *rpcPeer) close(err error) {
	if err == nil {
		err = io.EOF
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.closeErr = err
	pending := p.pending
	p.pending = make(map[int64]chan rpcResponse)
	close(p.done)
	p.mu.Unlock()
	for _, response := range pending {
		response <- rpcResponse{err: err}
	}
}
