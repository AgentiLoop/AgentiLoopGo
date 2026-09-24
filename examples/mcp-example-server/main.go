// Command mcp-example-server is a minimal example MCP server used by the transport
// tests. One binary, three transports:
//
//	go run ./examples/mcp-example-server --stdio          newline-delimited JSON-RPC on stdin/stdout
//	go run ./examples/mcp-example-server --http <port>    Streamable HTTP on http://127.0.0.1:<port>/mcp
//	go run ./examples/mcp-example-server --sse  <port>    legacy HTTP+SSE: GET /sse, POST /messages?session_id=…
//
// Port 0 picks a free port; HTTP modes print `listening on <port>` as the first stdout line.
// Tools: echo (text), add (read-only), fail (isError), slow (sleeps `ms`), listed
// over two tools/list pages. Resource: example://greeting.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type msg = map[string]any

func main() {
	args := os.Args[1:]
	port := 0
	if len(args) > 1 {
		port, _ = strconv.Atoi(args[1])
	}
	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "--stdio":
		stdio()
	case "--http":
		serve(port, streamableHandler())
	case "--sse":
		serve(port, sseHandler())
	default:
		fmt.Fprintln(os.Stderr, "usage: mcp-example-server --stdio | --http <port> | --sse <port>")
		os.Exit(2)
	}
}

// ---- MCP logic (shared by all transports) -------------------------------------

type rpcErr struct {
	code int
	msg  string
}

// handle processes one JSON-RPC message; nil for notifications.
func handle(m msg, transport string) msg {
	id, ok := m["id"]
	if !ok {
		return nil
	}
	method, _ := m["method"].(string)
	params, _ := m["params"].(map[string]any)
	var result any
	var e *rpcErr
	switch method {
	case "initialize":
		result = msg{
			"protocolVersion": "2024-11-05",
			"capabilities":    msg{"tools": msg{}, "resources": msg{}},
			"serverInfo":      msg{"name": "example-" + transport, "version": "1.0.0"},
		}
	case "tools/list":
		if _, ok := params["cursor"]; !ok {
			result = msg{"tools": []any{
				msg{"name": "echo", "description": "Echo back a message",
					"inputSchema": msg{"type": "object", "properties": msg{"message": msg{"type": "string"}}, "required": []any{"message"}}},
				msg{"name": "add", "description": "Add two numbers",
					"inputSchema": msg{"type": "object", "properties": msg{"a": msg{"type": "number"}, "b": msg{"type": "number"}}},
					"annotations": msg{"readOnlyHint": true}},
				msg{"name": "bad name!", "description": "invalid name, must be dropped by the client"},
			}, "nextCursor": "page2"}
		} else {
			result = msg{"tools": []any{
				msg{"name": "fail", "description": "Always reports a tool error"},
				msg{"name": "slow", "description": "Sleep then answer",
					"inputSchema": msg{"type": "object", "properties": msg{"ms": msg{"type": "integer"}}}},
			}}
		}
	case "tools/call":
		result, e = callTool(params)
	case "resources/list":
		result = msg{"resources": []any{msg{"uri": "example://greeting", "name": "Greeting", "mimeType": "text/plain"}}}
	case "resources/read":
		if uri, _ := params["uri"].(string); uri == "example://greeting" {
			result = msg{"contents": []any{msg{"uri": uri, "mimeType": "text/plain", "text": "Hello from " + transport + "!"}}}
		} else {
			e = &rpcErr{-32002, "resource not found: " + uri}
		}
	case "ping":
		result = msg{}
	default:
		e = &rpcErr{-32601, "method not found: " + method}
	}
	if e != nil {
		return msg{"jsonrpc": "2.0", "id": id, "error": msg{"code": e.code, "message": e.msg}}
	}
	return msg{"jsonrpc": "2.0", "id": id, "result": result}
}

func callTool(params map[string]any) (any, *rpcErr) {
	args, _ := params["arguments"].(map[string]any)
	text := func(t string) any { return msg{"content": []any{msg{"type": "text", "text": t}}} }
	name, _ := params["name"].(string)
	switch name {
	case "echo":
		s, _ := args["message"].(string)
		return text(s), nil
	case "add":
		a, _ := args["a"].(float64)
		b, _ := args["b"].(float64)
		return text(strconv.FormatFloat(a+b, 'f', -1, 64)), nil
	case "fail":
		return msg{"content": []any{msg{"type": "text", "text": "this tool always fails"}}, "isError": true}, nil
	case "slow":
		ms := 100.0
		if v, ok := args["ms"].(float64); ok {
			ms = v
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return text(fmt.Sprintf("slept %dms", int(ms))), nil
	}
	return nil, &rpcErr{-32602, "unknown tool: " + name}
}

// ---- stdio --------------------------------------------------------------------

func stdio() {
	fmt.Fprintln(os.Stderr, "example server: stdio ready") // exercises the client's stderr drain
	var mu sync.Mutex
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var m msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		// Answer concurrently so replies can arrive out of order.
		go func() {
			if resp := handle(m, "stdio"); resp != nil {
				data, _ := json.Marshal(resp)
				mu.Lock()
				os.Stdout.Write(append(data, '\n'))
				mu.Unlock()
			}
		}()
	}
}

// ---- HTTP -----------------------------------------------------------------------

func serve(port int, h http.Handler) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("listening on %d\n", ln.Addr().(*net.TCPAddr).Port)
	os.Stdout.Sync()
	http.Serve(ln, h)
}

func readJSON(r *http.Request) (msg, bool) {
	var m msg
	body, _ := io.ReadAll(r.Body)
	return m, json.Unmarshal(body, &m) == nil
}

func streamableHandler() http.Handler {
	var next atomic.Int64
	var mu sync.Mutex
	// sessions that were initialized and not DELETEd.
	sessions := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			mu.Lock()
			delete(sessions, r.Header.Get("Mcp-Session-Id"))
			mu.Unlock()
			return
		case http.MethodPost:
		default:
			http.NotFound(w, r)
			return
		}
		m, ok := readJSON(r)
		if !ok {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		method, _ := m["method"].(string)
		if method == "initialize" {
			sid := fmt.Sprintf("sess-%d", next.Add(1)-1)
			mu.Lock()
			sessions[sid] = true
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sid)
		} else {
			mu.Lock()
			known := sessions[r.Header.Get("Mcp-Session-Id")]
			mu.Unlock()
			if !known {
				// Unknown / missing session → 404, which clients must treat as "re-initialize".
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
		}
		resp := handle(m, "http")
		if resp == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		data, _ := json.Marshal(resp)
		if method == "tools/call" && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			// Stream a progress notification before the actual response, like real servers do.
			progress, _ := json.Marshal(msg{"jsonrpc": "2.0", "method": "notifications/progress", "params": msg{"progress": 1}})
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, ": keepalive\n\nevent: message\ndata: %s\n\ndata: %s\n\n", progress, data)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	})
	return mux
}

func sseHandler() http.Handler {
	var next atomic.Int64
	var mu sync.Mutex
	// streams: session id → channel feeding that client's GET stream.
	streams := map[string]chan string{}
	mux := http.NewServeMux()
	stream := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		sid := fmt.Sprintf("s%d", next.Add(1)-1)
		ch := make(chan string, 64)
		mu.Lock()
		streams[sid] = ch
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(streams, sid)
			mu.Unlock()
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprintf(w, "event: endpoint\ndata: /messages?session_id=%s\n\n", sid)
		w.(http.Flusher).Flush()
		for {
			select {
			case ev := <-ch:
				if _, err := io.WriteString(w, ev); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			case <-r.Context().Done(): // client hung up
				return
			}
		}
	}
	mux.HandleFunc("/sse", stream)
	// /events lets tests check that "transport": "sse" forces the legacy transport.
	mux.HandleFunc("/events", stream)
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		ch, ok := streams[r.URL.Query().Get("session_id")]
		mu.Unlock()
		if !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		m, ok := readJSON(r)
		if !ok {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		// The reply travels back over the GET stream, not this response.
		go func() {
			if resp := handle(m, "sse"); resp != nil {
				data, _ := json.Marshal(resp)
				ch <- fmt.Sprintf("event: message\ndata: %s\n\n", data)
			}
		}()
	})
	return mux
}
