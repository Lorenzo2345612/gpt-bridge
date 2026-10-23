package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const bridgeVersion = "0.6.10"

type options struct {
	serve, check, models, version      bool
	dashboard                          bool
	port, timeout, maxSessions, window int
	idleTimeout                        int
	model, effort, tokenFile           string
	claudeArgs                         []string
}

func parseOptions(args []string) (options, error) {
	o := options{timeout: 300, maxSessions: 0, idleTimeout: 120, model: os.Getenv("CLAUDE_CODEX_MODEL"), effort: os.Getenv("CLAUDE_CODEX_EFFORT")}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			o.claudeArgs = append(o.claudeArgs, args[i+1:]...)
			break
		}
		name, value, inline := strings.Cut(arg, "=")
		switch name {
		case "--dashboard":
			o.dashboard = true
		case "--serve":
			o.serve = true
		case "--check":
			o.check = true
		case "--models":
			o.models = true
		case "--bridge-version":
			o.version = true
		case "--port", "--timeout", "--max-sessions", "--idle-timeout", "--context-window", "--model", "--codex-model", "--effort", "--token-file":
			if !inline {
				i++
				if i >= len(args) {
					return o, bad("%s requires a value", name)
				}
				value = args[i]
			}
			switch name {
			case "--model", "--codex-model":
				o.model = value
			case "--effort":
				o.effort = value
			case "--token-file":
				o.tokenFile = value
			default:
				n, e := strconv.Atoi(value)
				if e != nil {
					return o, bad("%s requires an integer", name)
				}
				switch name {
				case "--port":
					o.port = n
				case "--timeout":
					o.timeout = n
				case "--max-sessions":
					o.maxSessions = n
				case "--idle-timeout":
					o.idleTimeout = n
				case "--context-window":
					if n <= 0 {
						return o, bad("--context-window must be positive")
					}
					o.window = n
				}
			}
		default:
			o.claudeArgs = append(o.claudeArgs, arg)
		}
	}
	if o.timeout < 1 || o.maxSessions < 0 || o.idleTimeout < 0 || o.port < 0 || o.port > 65535 {
		return o, bad("Invalid timeout, idle timeout, maximum session count or port")
	}
	if o.serve && o.tokenFile == "" {
		return o, bad("--serve requires --token-file")
	}
	return o, nil
}
func readSettings(path string) M {
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil
	}
	var m M
	if decode(raw, &m) != nil {
		return nil
	}
	return m
}
func restoreHostModel(path string, original M, models map[string]M) error {
	if original == nil {
		return nil
	}
	current := readSettings(path)
	if current == nil || models[str(current["model"])] == nil || models[str(original["model"])] != nil {
		return nil
	}
	if v, ok := original["model"]; ok {
		current["model"] = v
	} else {
		delete(current, "model")
	}
	info, e := os.Stat(path)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".claude-codex-restore-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, e = f.Write(append(encode(current), '\n')); e != nil {
		f.Close()
		return e
	}
	if e = f.Chmod(info.Mode().Perm()); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(tmp, path)
}
func run(args []string) (exitCode int, runError error) {
	opts, e := parseOptions(args)
	if e != nil {
		return 1, e
	}
	if opts.version {
		fmt.Printf("claude-codex-bridge %s (Go)\n", bridgeVersion)
		return 0, nil
	}
	if opts.dashboard {
		return runDashboard(opts.port)
	}
	binary, e := exec.LookPath("codex")
	if e != nil {
		return 1, bad("codex is not on PATH")
	}
	command, e := codexCommand(binary)
	if e != nil {
		return 1, e
	}
	cwd, e := os.Getwd()
	if e != nil {
		return 1, e
	}
	timeout := time.Duration(opts.timeout) * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Install handlers before spawning any subprocess, so startup interruption
	// also cleans up. Each Codex backend owns its separate process group.
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, terminationSignals()...)
	defer signal.Stop(signals)
	var childStarted atomic.Bool
	var interrupted atomic.Int32
	notifications := make(chan os.Signal, 4)
	monitorStop := make(chan struct{})
	defer close(monitorStop)
	defer func() {
		if sig := interrupted.Load(); sig != 0 {
			exitCode = 128 + int(sig)
			runError = nil
		}
	}()
	go func() {
		for {
			select {
			case <-monitorStop:
				return
			case sig := <-signals:
				if sig == os.Interrupt && childStarted.Load() {
					continue
				}
				interrupted.CompareAndSwap(0, int32(signalNumber(sig)))
				cancel()
				select {
				case notifications <- sig:
				case <-monitorStop:
					return
				}
			}
		}
	}()
	rpc, e := newRPC(ctx, command, cwd, min(timeout, 45*time.Second))
	if e != nil {
		return 1, e
	}
	catalog, e := loadCatalog(ctx, rpc)
	rpc.Close()
	if e != nil {
		return 1, e
	}
	enrichCatalog(catalog, filepath.Join(codexHome(), "models_cache.json"))
	window, e := clientContext(catalog, opts.window)
	if e != nil {
		return 1, e
	}
	def := str(catalog[0]["model"])
	known := map[string]M{}
	for _, m := range catalog {
		known[str(m["model"])] = m
		if boolean(m["isDefault"]) {
			def = str(m["model"])
		}
	}
	roles, e := modelAliases(catalog, def, envMap(os.Environ()))
	if e != nil {
		return 1, e
	}
	model, e := resolveModel(fallback(opts.model, def), def, roles)
	if e != nil {
		return 1, e
	}
	if known[model] == nil {
		return 1, bad("Selected model is not in your Codex catalog; run claude-codex --models")
	}
	roles, e = modelAliases(catalog, model, envMap(os.Environ()))
	if e != nil {
		return 1, e
	}
	if opts.check {
		names := []any{}
		windows := M{}
		for _, m := range catalog {
			n := str(m["model"])
			names = append(names, n)
			v := integer(m["maxContextWindow"])
			if opts.window > 0 {
				v = opts.window
			}
			windows[n] = v
		}
		agents := modelAgents(catalog)
		agentNames := []string{}
		for k := range agents {
			agentNames = append(agentNames, k)
		}
		sort.Strings(agentNames)
		fmt.Println(string(encode(M{"auth": "chatgpt", "runtime": "go", "default_model": model, "models": names, "aliases": roles, "subagents": agentNames, "client_context_window": window, "native_context_windows": windows, "claude_config_dir": claudeConfig(), "idle_timeout_seconds": opts.idleTimeout})))
		return 0, nil
	}
	if opts.models {
		for _, m := range catalog {
			efforts := []string{}
			for _, v := range array(m["supportedReasoningEfforts"]) {
				efforts = append(efforts, str(object(v)["reasoningEffort"]))
			}
			fmt.Printf("%s\t%s\tcontext=%d\t%s\n", str(m["model"]), str(m["displayName"]), integer(m["effectiveContextWindow"]), strings.Join(efforts, ", "))
		}
		return 0, nil
	}
	token := uid("") + uid("")
	if opts.tokenFile != "" {
		raw, e := os.ReadFile(opts.tokenFile)
		if e != nil {
			return 1, e
		}
		token = strings.TrimSpace(string(raw))
	}
	if len(token) < 24 {
		return 1, bad("Local bridge token must have at least 24 characters")
	}
	b := newBridge(command, cwd, model, catalog, roles, timeout, opts.maxSessions, opts.window, opts.effort)
	b.idleTimeout = time.Duration(opts.idleTimeout) * time.Second
	b.stateDir = fallback(os.Getenv("CLAUDE_CODEX_STATE_DIR"), filepath.Join(claudeConfig(), "gpt-bridge", "sessions", hash(codexHome())[:16]))
	for _, arg := range opts.claudeArgs {
		if arg == "--no-session-persistence" {
			b.stateDir = ""
		}
	}
	defer b.close()
	go b.janitor(ctx)
	listener, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.port))
	if e != nil {
		return 1, e
	}
	server := &http.Server{Handler: &apiHandler{b, token}, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	url := "http://" + listener.Addr().String()
	if opts.serve {
		fmt.Printf("Claude Codex Bridge listening at %s (Go)\n", url)
		sig := <-notifications
		cancel()
		return 128 + signalNumber(sig), nil
	}
	claude, e := exec.LookPath("claude")
	if e != nil {
		return 1, bad("claude is not on PATH")
	}
	env := claudeEnvironment(envMap(os.Environ()), token, url, model, roles, window)
	overrides := M{}
	for k, v := range env {
		if strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_CODE_SUBAGENT_MODEL") || k == "CLAUDE_CODE_MAX_CONTEXT_TOKENS" || k == "CLAUDE_CODE_DISABLE_1M_CONTEXT" {
			overrides[k] = v
		}
	}
	temporary, e := os.MkdirTemp("", "claude-codex-")
	if e != nil {
		return 1, e
	}
	defer os.RemoveAll(temporary)
	settingsPath := filepath.Join(temporary, "settings.json")
	settings, hostArgs, e := mergedHostSettings(opts.claudeArgs, M{"env": overrides, "model": model, "modelPicker": modelPicker(catalog)})
	if e != nil {
		return 1, e
	}
	if e = os.WriteFile(settingsPath, encode(settings), 0600); e != nil {
		return 1, e
	}
	profile := filepath.Join(claudeConfig(), "settings.json")
	original := readSettings(profile)
	fmt.Fprintf(os.Stderr, "claude-codex: %s · ChatGPT subscription · %s · Go\n", model, claudeConfig())
	claudeArgs := append([]string{"--settings", settingsPath, "--agents", string(encode(modelAgents(catalog)))}, hostArgs...)
	child, e := platformCommand(claude, claudeArgs...)
	if e != nil {
		return 1, e
	}
	child.Env = envList(env)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if e = child.Start(); e != nil {
		return 1, e
	}
	childScope, e := trackProcess(child.Process, false)
	if e != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return 1, fmt.Errorf("could not isolate Claude process: %w", e)
	}
	defer childScope.close()
	childStarted.Store(true)
	defer func() {
		if e := restoreHostModel(profile, original, known); e != nil {
			fmt.Fprintln(os.Stderr, "claude-codex: could not restore the native profile model")
		}
	}()
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	for {
		select {
		case err := <-exited:
			cancel()
			if err == nil {
				return 0, nil
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return processExitCode(exit), nil
			}
			return 1, err
		case sig := <-notifications:
			cancel()
			childScope.terminate()
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				childScope.kill()
				<-exited
			}
			return 128 + signalNumber(sig), nil
		}
	}
}
func main() {
	code, e := run(os.Args[1:])
	if e != nil {
		fmt.Fprintln(os.Stderr, "claude-codex:", e)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
