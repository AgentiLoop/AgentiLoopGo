package mcp

// Transport abstraction shared by stdio, Streamable HTTP and legacy HTTP+SSE,
// plus the JSON-RPC / SSE helpers they have in common.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// requestTimeout: MCP tools (domain checks, builds, …) can be slow: 15 minutes per request.
var requestTimeout = 900 * time.Second

// Msg is a decoded JSON-RPC message.
type Msg = map[string]any

type Transport interface {
	// Request sends a JSON-RPC request and waits for the matching response object.
	Request(ctx context.Context, method string, params any) (Msg, error)
	// Notify sends a JSON-RPC notification (no response expected).
	Notify(ctx context.Context, method string, params any) error
	IsAlive() bool
	Close()
}

// pending tracks in-flight requests keyed by JSON-RPC id; failAll unblocks every waiter.
type pending struct {
	mu      sync.Mutex
	waiters map[int64]chan Msg
	nextID  int64
}

func newPending() *pending { return &pending{waiters: map[int64]chan Msg{}} }

func (p *pending) register() (int64, chan Msg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	ch := make(chan Msg, 1)
	p.waiters[p.nextID] = ch
	return p.nextID, ch
}

func (p *pending) nextOnly() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.nextID++
	return p.nextID
}

func (p *pending) remove(id int64) {
	p.mu.Lock()
	delete(p.waiters, id)
	p.mu.Unlock()
}

// dispatch hands a message to whichever request is waiting on its id.
func (p *pending) dispatch(msg Msg) {
	id, ok := responseID(msg)
	if !ok {
		return
	}
	p.mu.Lock()
	ch, found := p.waiters[id]
	delete(p.waiters, id)
	p.mu.Unlock()
	if found {
		ch <- msg
	}
}

func (p *pending) failAll() {
	p.mu.Lock()
	for id, ch := range p.waiters {
		close(ch)
		delete(p.waiters, id)
	}
	p.mu.Unlock()
}

// await waits for a registered response with the standard timeout; cleans up on failure.
func (p *pending) await(ctx context.Context, id int64, ch chan Msg, method string) (Msg, error) {
	timer := time.NewTimer(requestTimeout)
	defer timer.Stop()
	select {
	case msg, ok := <-ch:
		if !ok {
			return nil, errors.New("MCP server closed the connection")
		}
		return msg, nil
	case <-timer.C:
		p.remove(id)
		return nil, fmt.Errorf("timeout waiting for %s", method)
	case <-ctx.Done():
		p.remove(id)
		return nil, ctx.Err()
	}
}

func rpcRequest(id int64, method string, params any) Msg {
	m := Msg{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

func rpcNotification(method string, params any) Msg {
	m := Msg{"jsonrpc": "2.0", "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

// responseID returns the JSON-RPC id as an integer (servers may echo it back as a numeric string).
func responseID(msg Msg) (int64, bool) {
	switch v := msg["id"].(type) {
	case float64:
		if v == float64(int64(v)) {
			return int64(v), true
		}
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// sseEvent is one complete Server-Sent Event.
type sseEvent struct {
	Event, Data string
}

// messageJSON is the JSON payload of a `message` (or untyped) event, per the MCP spec.
func (e sseEvent) messageJSON() (Msg, bool) {
	if e.Event != "" && e.Event != "message" {
		return nil, false
	}
	var m Msg
	if json.Unmarshal([]byte(e.Data), &m) != nil || m == nil {
		return nil, false
	}
	return m, true
}

// sseParser is an incremental SSE parser: feed raw bytes, get events out.
type sseParser struct {
	buf         []byte
	event, data string
}

func (p *sseParser) push(b []byte) []sseEvent {
	p.buf = append(p.buf, b...)
	var out []sseEvent
	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			return out
		}
		line := strings.TrimRight(string(p.buf[:i]), "\r")
		p.buf = p.buf[i+1:]
		if ev, ok := p.line(line); ok {
			out = append(out, ev)
		}
	}
}

func (p *sseParser) line(line string) (sseEvent, bool) {
	if line == "" {
		return p.take()
	}
	if v, ok := strings.CutPrefix(line, "event:"); ok {
		p.event = strings.TrimSpace(v)
	} else if v, ok := strings.CutPrefix(line, "data:"); ok {
		v = strings.TrimPrefix(v, " ")
		if p.data != "" {
			p.data += "\n"
		}
		p.data += v
	}
	// id:, retry: and `:` comments are ignored.
	return sseEvent{}, false
}

// flush returns an event still buffered when the stream closed without a trailing blank line.
func (p *sseParser) flush() (sseEvent, bool) {
	if len(p.buf) > 0 {
		rest := strings.TrimRight(string(p.buf), " \t\r\n")
		p.buf = nil
		p.line(rest)
	}
	return p.take()
}

func (p *sseParser) take() (sseEvent, bool) {
	ev := sseEvent{p.event, p.data}
	p.event, p.data = "", ""
	return ev, ev.Data != ""
}
