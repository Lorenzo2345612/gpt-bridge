package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type backend interface {
	Call(context.Context, string, M) (M, error)
	Send(M) error
	Events() <-chan M
	Done() <-chan struct{}
	Close()
}
type rpcClient struct {
	cmd          *exec.Cmd
	stdin        io.WriteCloser
	events       chan M
	done, waited chan struct{}
	closeOnce    sync.Once
	mu, writeMu  sync.Mutex
	next         uint64
	replies      map[string]chan M
	timeout      time.Duration
}

func newRPC(ctx context.Context, command []string, cwd string, timeout time.Duration) (backend, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = cwd
	cmd.Env = envList(without(envMap(os.Environ()), "OPENAI_API_KEY"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, e := cmd.StdinPipe()
	if e != nil {
		return nil, e
	}
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		stdin.Close()
		return nil, e
	}
	cmd.Stderr = io.Discard
	if e = cmd.Start(); e != nil {
		stdin.Close()
		return nil, e
	}
	r := &rpcClient{cmd: cmd, stdin: stdin, events: make(chan M, 256), done: make(chan struct{}), waited: make(chan struct{}), replies: map[string]chan M{}, timeout: timeout}
	go r.read(stdout)
	go func() { _ = cmd.Wait(); close(r.waited) }()
	_, e = r.Call(ctx, "initialize", M{"clientInfo": M{"name": "claude_codex_bridge", "title": "Claude Codex Bridge", "version": bridgeVersion}, "capabilities": M{"experimentalApi": true}})
	if e == nil {
		e = r.Send(M{"method": "initialized"})
	}
	if e == nil {
		var result M
		result, e = r.Call(ctx, "account/read", M{"refreshToken": false})
		if e == nil {
			kind := str(object(result["account"])["type"])
			if kind != "chatgpt" && kind != "chatgptAuthTokens" {
				e = &bridgeError{503, "Run codex login with your ChatGPT account first (API-key auth refused)"}
			}
		}
	}
	if e != nil {
		r.Close()
		return nil, e
	}
	return r, nil
}
func (r *rpcClient) read(stdout io.Reader) {
	defer r.stop()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64*1024), 128*1024*1024)
	for scan.Scan() {
		var msg M
		if decode(scan.Bytes(), &msg) != nil {
			continue
		}
		if id, ok := msg["id"]; ok && msg["method"] == nil {
			r.mu.Lock()
			waiter := r.replies[fmt.Sprint(id)]
			r.mu.Unlock()
			if waiter != nil {
				select {
				case waiter <- msg:
				default:
				}
			}
		} else {
			select {
			case r.events <- msg:
			case <-r.done:
				return
			}
		}
	}
}
func (r *rpcClient) stop()                 { r.closeOnce.Do(func() { close(r.done) }) }
func (r *rpcClient) Events() <-chan M      { return r.events }
func (r *rpcClient) Done() <-chan struct{} { return r.done }
func (r *rpcClient) Send(msg M) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	select {
	case <-r.done:
		return upstream("Codex app-server is no longer running")
	default:
	}
	_, e := r.stdin.Write(append(encode(msg), '\n'))
	if e != nil {
		return upstream("Could not write to Codex app-server")
	}
	return nil
}
func (r *rpcClient) Call(ctx context.Context, method string, params M) (M, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	r.mu.Lock()
	r.next++
	id := r.next
	key := fmt.Sprint(id)
	waiter := make(chan M, 1)
	r.replies[key] = waiter
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.replies, key); r.mu.Unlock() }()
	if e := r.Send(M{"id": id, "method": method, "params": params}); e != nil {
		return nil, e
	}
	select {
	case msg := <-waiter:
		if e := object(msg["error"]); e != nil {
			status := 502
			if integer(e["code"]) == -32602 {
				status = 400
			}
			return nil, &bridgeError{status, "Codex " + method + ": " + str(e["message"])}
		}
		result := object(msg["result"])
		if result == nil {
			result = M{}
		}
		return result, nil
	case <-ctx.Done():
		return nil, &bridgeError{504, "Timed out or cancelled waiting for Codex " + method}
	case <-r.done:
		return nil, upstream("Codex app-server stopped; check codex login and local permissions")
	}
}
func (r *rpcClient) Close() {
	r.stop()
	_ = r.stdin.Close()
	select {
	case <-r.waited:
		return
	default:
	}
	// Each backend is a new session/process group. Never signal the user's shell.
	_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-r.waited:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-r.waited:
		case <-time.After(time.Second):
		}
	}
}
