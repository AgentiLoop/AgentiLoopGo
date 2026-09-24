package mcp

// HTTP transports: Streamable HTTP (MCP 2025-03-26, single POST endpoint)
// and the legacy HTTP+SSE transport (MCP 2024-11-05, GET /sse stream +
// POST to the URL announced by the server's `endpoint` event).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const sessionHeader = "Mcp-Session-Id"

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout: 30 * time.Second,
		},
	}
}

func errorBody(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	s := string(body)
	if utf8.RuneCountInString(s) > 512 {
		s = string([]rune(s)[:512])
	}
	return fmt.Errorf("HTTP %d: %s", resp.StatusCode, s)
}

func setHeaders(r *http.Request, h map[string]string) {
	for k, v := range h {
		r.Header.Set(k, v)
	}
}

// ---- Streamable HTTP --------------------------------------------------------

type HTTPTransport struct {
	client    *http.Client
	url       string
	headers   map[string]string
	mu        sync.Mutex
	sessionID string
	nextID    atomic.Int64
	alive     atomic.Bool
}

func NewHTTPTransport(u string, headers map[string]string) *HTTPTransport {
	t := &HTTPTransport{client: newHTTPClient(600 * time.Second), url: u, headers: headers}
	t.alive.Store(true)
	return t
}

func (t *HTTPTransport) post(ctx context.Context, body Msg) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	setHeaders(r, t.headers)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	t.mu.Lock()
	if t.sessionID != "" {
		r.Header.Set(sessionHeader, t.sessionID)
	}
	t.mu.Unlock()
	resp, err := t.client.Do(r)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get(sessionHeader); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		t.mu.Lock()
		t.sessionID = ""
		t.mu.Unlock()
		return nil, errors.New("MCP session expired (404)")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, errorBody(resp)
	}
	return resp, nil
}

func (t *HTTPTransport) Request(ctx context.Context, method string, params any) (Msg, error) {
	if !t.IsAlive() {
		return nil, errors.New("HTTP connection is closed")
	}
	id := t.nextID.Add(1)
	resp, err := t.post(ctx, rpcRequest(id, method, params))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		var m Msg
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return nil, fmt.Errorf("invalid JSON-RPC response: %w", err)
		}
		return m, nil
	}
	// The server may stream progress notifications before the final response.
	var parser sseParser
	var last Msg
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		for _, ev := range parser.push(buf[:n]) {
			if m, ok := ev.messageJSON(); ok {
				if rid, ok := responseID(m); ok && rid == id {
					return m, nil
				}
				last = m
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if ev, ok := parser.flush(); ok {
		if m, ok := ev.messageJSON(); ok {
			return m, nil
		}
	}
	if last == nil {
		return nil, errors.New("event stream closed without a response")
	}
	return last, nil
}

func (t *HTTPTransport) Notify(ctx context.Context, method string, params any) error {
	if !t.IsAlive() {
		return nil
	}
	resp, err := t.post(ctx, rpcNotification(method, params))
	if err != nil {
		return err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (t *HTTPTransport) IsAlive() bool { return t.alive.Load() }

func (t *HTTPTransport) Close() {
	t.alive.Store(false)
	// Per spec, DELETE ends the server-side session.
	t.mu.Lock()
	sid := t.sessionID
	t.sessionID = ""
	t.mu.Unlock()
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.url, nil)
	if err != nil {
		return
	}
	setHeaders(r, t.headers)
	r.Header.Set(sessionHeader, sid)
	if resp, err := t.client.Do(r); err == nil {
		resp.Body.Close()
	}
}

// ---- Legacy HTTP+SSE --------------------------------------------------------

type LegacySSETransport struct {
	// postClient is separate so POSTs never share a connection with the long-lived stream.
	postClient *http.Client
	endpoint   string
	headers    map[string]string
	pending    *pending
	alive      atomic.Bool
	cancel     context.CancelFunc
}

// ConnectLegacySSE opens the GET stream and waits (≤30 s) for the server's `endpoint` event.
func ConnectLegacySSE(ctx context.Context, rawURL string, headers map[string]string) (*LegacySSETransport, error) {
	base, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	r, err := http.NewRequestWithContext(streamCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	setHeaders(r, headers)
	r.Header.Set("Accept", "text/event-stream")
	resp, err := newHTTPClient(0).Do(r)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not open SSE stream %s: %w", rawURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		cancel()
		return nil, errorBody(resp)
	}

	t := &LegacySSETransport{postClient: newHTTPClient(180 * time.Second), headers: headers, pending: newPending(), cancel: cancel}
	t.alive.Store(true)
	epCh := make(chan string, 1)
	go func() {
		defer resp.Body.Close()
		var parser sseParser
		sentEP := false
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			for _, ev := range parser.push(buf[:n]) {
				if ev.Event == "endpoint" {
					if !sentEP {
						sentEP = true
						epCh <- strings.TrimSpace(ev.Data)
					}
				} else if m, ok := ev.messageJSON(); ok {
					t.pending.dispatch(m)
				}
			}
			if rerr != nil {
				break
			}
		}
		t.alive.Store(false)
		t.pending.failAll()
		close(epCh)
	}()

	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case ep, ok := <-epCh:
		if !ok {
			cancel()
			return nil, errors.New("SSE stream closed before the `endpoint` event")
		}
		ref, err := url.Parse(ep)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("bad endpoint `%s`: %w", ep, err)
		}
		t.endpoint = base.ResolveReference(ref).String()
		return t, nil
	case <-timer.C:
		cancel()
		return nil, errors.New("SSE endpoint handshake timed out after 30s")
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

func (t *LegacySSETransport) post(ctx context.Context, body Msg) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	setHeaders(r, t.headers)
	r.Header.Set("Content-Type", "application/json")
	resp, err := t.postClient.Do(r)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, errorBody(resp)
	}
	return resp, nil
}

func (t *LegacySSETransport) Request(ctx context.Context, method string, params any) (Msg, error) {
	if !t.IsAlive() {
		return nil, errors.New("SSE stream is closed")
	}
	id, ch := t.pending.register()
	resp, err := t.post(ctx, rpcRequest(id, method, params))
	if err != nil {
		t.pending.remove(id)
		return nil, err
	}
	// Normally 202 + empty body, the reply arrives on the stream. Some servers
	// answer inline instead; accept that too.
	var m Msg
	if json.NewDecoder(resp.Body).Decode(&m) == nil {
		if rid, ok := responseID(m); ok && rid == id {
			resp.Body.Close()
			t.pending.remove(id)
			return m, nil
		}
	}
	resp.Body.Close()
	return t.pending.await(ctx, id, ch, method)
}

func (t *LegacySSETransport) Notify(ctx context.Context, method string, params any) error {
	resp, err := t.post(ctx, rpcNotification(method, params))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (t *LegacySSETransport) IsAlive() bool { return t.alive.Load() }

func (t *LegacySSETransport) Close() {
	t.alive.Store(false)
	t.cancel()
	t.pending.failAll()
}
