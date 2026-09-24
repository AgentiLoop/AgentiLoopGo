package mcp

// Stdio transport: spawn the server, newline-delimited JSON-RPC over its pipes.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// maxLine: a single message larger than this (10 MB) disconnects the server.
const maxLine = 10 * 1024 * 1024

type StdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	pending *pending
	alive   atomic.Bool
	exited  chan struct{}
}

func SpawnStdio(program string, args []string, env map[string]string, cwd string) (*StdioTransport, error) {
	cmd := exec.Command(program, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to launch `%s`: %w", program, err)
	}
	t := &StdioTransport{cmd: cmd, stdin: stdin, pending: newPending(), exited: make(chan struct{})}
	t.alive.Store(true)

	// Drain stderr so a chatty server never blocks on a full pipe.
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), maxLine)
		for sc.Scan() {
			slog.Debug("mcp", "stderr", sc.Text())
		}
		io.Copy(io.Discard, stderr)
	}()

	go func() {
		r := bufio.NewReaderSize(stdout, 64*1024)
		for {
			line, err := readLine(r)
			if err != nil {
				if errors.Is(err, errTooLong) {
					slog.Warn("MCP server sent a message over 10 MB; disconnecting")
					cmd.Process.Kill()
				}
				break
			}
			var msg Msg
			if json.Unmarshal(line, &msg) == nil {
				t.pending.dispatch(msg)
			}
		}
		t.alive.Store(false)
		t.pending.failAll() // fail every waiter
		io.Copy(io.Discard, stdout)
		<-stderrDone
		cmd.Wait() // only after both pipes are drained, as exec requires
		close(t.exited)
	}()
	return t, nil
}

var errTooLong = errors.New("line too long")

func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, errTooLong
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF && len(buf) > 0 {
			return buf, nil
		}
		return buf, err
	}
}

func (t *StdioTransport) write(msg Msg) error {
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	_, err = t.stdin.Write(append(line, '\n'))
	return err
}

func (t *StdioTransport) Request(ctx context.Context, method string, params any) (Msg, error) {
	if !t.IsAlive() {
		return nil, errors.New("MCP server process is no longer running")
	}
	// Register before writing so a fast reply can't be missed.
	id, ch := t.pending.register()
	if err := t.write(rpcRequest(id, method, params)); err != nil {
		t.pending.remove(id)
		return nil, fmt.Errorf("write failed: %w", err)
	}
	return t.pending.await(ctx, id, ch, method)
}

func (t *StdioTransport) Notify(_ context.Context, method string, params any) error {
	return t.write(rpcNotification(method, params))
}

func (t *StdioTransport) IsAlive() bool {
	if !t.alive.Load() {
		return false
	}
	select {
	case <-t.exited:
		return false
	default:
		return true
	}
}

func (t *StdioTransport) Close() {
	t.alive.Store(false)
	t.pending.failAll()
	t.stdin.Close()
	if t.cmd.Process != nil {
		t.cmd.Process.Kill()
	}
}
