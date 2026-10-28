package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

func envMap(env []string) map[string]string {
	r := map[string]string{}
	for _, s := range env {
		if k, v, ok := strings.Cut(s, "="); ok {
			r[k] = v
		}
	}
	return r
}
func envList(env map[string]string) []string {
	r := make([]string, 0, len(env))
	for k, v := range env {
		r = append(r, k+"="+v)
	}
	return r
}
func without(env map[string]string, keys ...string) map[string]string {
	for _, k := range keys {
		delete(env, k)
	}
	return env
}
func userHome() string { h, _ := os.UserHomeDir(); return h }
func codexHome() string {
	return fallback(os.Getenv("CODEX_HOME"), filepath.Join(userHome(), ".codex"))
}
func claudeConfig() string {
	return fallback(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(userHome(), ".claude"))
}
func codexCommand(binary string) ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return codexCommandForCWD(binary, cwd)
}

func codexCommandForCWD(binary, cwd string) ([]string, error) {
	args := []string{binary, "app-server", "--stdio"}
	for _, name := range []string{"shell_tool", "unified_exec", "apps", "plugins", "hooks", "multi_agent", "browser_use", "computer_use", "view_image", "image_generation", "goals", "tool_suggest", "skill_search", "sleep_tool", "code_mode"} {
		args = append(args, "-c", "features."+name+"=false")
	}
	args = append(args, "-c", "features.skip_host_skill_discovery=true", "-c", `web_search="disabled"`, "-c", "project_doc_max_bytes=0", "-c", `model_provider="openai"`)
	paths := []string{filepath.Join(codexHome(), "config.toml")}
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		paths = append(paths, filepath.Join(dir, ".codex", "config.toml"))
		if filepath.Dir(dir) == dir {
			break
		}
	}
	seen := map[string]bool{}
	for _, path := range paths {
		data, e := os.ReadFile(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		var config M
		if e = toml.Unmarshal(data, &config); e != nil {
			return nil, bad("Could not parse Codex config.toml at %s", path)
		}
		for name := range object(config["mcp_servers"]) {
			seen[name] = true
		}
	}
	// Codex's dotted override parser does not unquote key segments. A single
	// inline TOML table preserves names with dots/spaces. Supply a harmless,
	// disabled stdio transport because the override replaces the whole table;
	// enabled=false alone is not a valid MCP transport in current Codex.
	if len(seen) > 0 {
		names := make([]string, 0, len(seen))
		for name := range seen {
			names = append(names, name)
		}
		sort.Strings(names)
		entries := make([]string, 0, len(names))
		for _, name := range names {
			entries = append(entries, string(encode(name))+`={enabled=false,command="gpt-bridge-disabled-native-mcp"}`)
		}
		args = append(args, "-c", "mcp_servers={"+strings.Join(entries, ",")+"}")
	}
	return args, nil
}
func loadCatalog(ctx context.Context, rpc backend) ([]M, error) {
	catalog := []M{}
	index := map[string]int{}
	seen := map[string]bool{}
	cursor := ""
	for {
		params := M{"includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page, e := rpc.Call(ctx, "model/list", params)
		if e != nil {
			return nil, e
		}
		for _, v := range array(page["data"]) {
			m := object(v)
			name := str(m["model"])
			if name == "" {
				return nil, upstream("Codex returned an invalid model catalog")
			}
			if i, ok := index[name]; ok {
				catalog[i] = m
			} else {
				index[name] = len(catalog)
				catalog = append(catalog, m)
			}
		}
		cursor = str(page["nextCursor"])
		if cursor == "" {
			break
		}
		if seen[cursor] {
			return nil, upstream("Codex model catalog repeated a pagination cursor")
		}
		seen[cursor] = true
	}
	if len(catalog) == 0 {
		return nil, &bridgeError{503, "Codex returned an empty model catalog"}
	}
	return catalog, nil
}
func enrichCatalog(catalog []M, path string) {
	data, e := os.ReadFile(path)
	if e != nil {
		return
	}
	var cache M
	if decode(data, &cache) != nil {
		return
	}
	metadata := map[string]M{}
	for _, v := range array(cache["models"]) {
		m := object(v)
		metadata[str(m["slug"])] = m
	}
	for _, m := range catalog {
		info := metadata[str(m["model"])]
		maximum := integer(info["max_context_window"])
		if maximum == 0 {
			maximum = integer(info["context_window"])
		}
		percent := 95
		if v, exists := info["effective_context_window_percent"]; exists {
			percent = integer(v)
		}
		if policy := object(info["truncation_policy"]); policy["mode"] == "tokens" && integer(policy["limit"]) > 0 {
			m["toolOutputTokenLimit"] = integer(policy["limit"])
		}
		if maximum > 0 && percent > 0 && percent <= 100 {
			m["contextWindow"] = info["context_window"]
			m["maxContextWindow"] = maximum
			m["effectiveContextWindowPercent"] = percent
			m["effectiveContextWindow"] = maximum * percent / 100
		}
	}
}
func clientContext(catalog []M, requested int) (int, error) {
	limit := 0
	for _, m := range catalog {
		maximum := integer(m["maxContextWindow"])
		if requested != 0 {
			if requested < 0 || maximum == 0 || requested > maximum {
				return 0, bad("Requested context exceeds a model's advertised Codex maximum")
			}
			maximum = requested
		}
		percent := integer(m["effectiveContextWindowPercent"])
		if percent == 0 {
			percent = 95
		}
		window := 200000
		if maximum > 0 {
			window = maximum * percent / 100
		}
		if limit == 0 || window < limit {
			limit = window
		}
	}
	if limit == 0 {
		limit = 200000
	}
	return limit, nil
}
func modelAliases(catalog []M, model string, env map[string]string) (map[string]string, error) {
	names := map[string]bool{}
	first := func(suffix, def string) string {
		for _, m := range catalog {
			if n := str(m["model"]); strings.HasSuffix(n, suffix) {
				return n
			}
		}
		return def
	}
	for _, m := range catalog {
		names[str(m["model"])] = true
	}
	sol := first("-sol", model)
	roles := map[string]string{"opus": sol, "sonnet": first("-terra", sol), "haiku": first("-luna", model), "fable": first("-astra", model)}
	for role, def := range roles {
		n := fallback(env["CLAUDE_CODEX_"+strings.ToUpper(role)+"_MODEL"], def)
		if !names[n] {
			return nil, bad("Configured %s model is not in your Codex catalog", role)
		}
		roles[role] = n
	}
	return roles, nil
}
func modelPicker(catalog []M) M {
	options := []any{}
	for _, m := range catalog {
		options = append(options, M{"model": m["model"], "label": fallback(str(m["displayName"]), str(m["model"])), "description": fallback(str(m["description"]), "ChatGPT / Codex subscription")})
	}
	return M{"replaceBuiltInOptions": true, "options": options}
}
func modelAgents(catalog []M) M {
	agents := M{}
	for _, m := range catalog {
		n := str(m["model"])
		agents["codex-"+strings.ReplaceAll(n, ".", "-")] = M{"model": n, "description": "Delegate work to " + n + " using the user's Codex subscription. Use this agent when assigning work to this specific model; omit the model parameter on Agent calls so its declared model is preserved.", "prompt": "Complete the delegated task using the Claude Code tools available to you. Follow project instructions and applicable skills. Report concrete results."}
	}
	return agents
}
func claudeEnvironment(base map[string]string, token, url, model string, aliases map[string]string, window int) map[string]string {
	env := map[string]string{}
	for k, v := range base {
		env[k] = v
	}
	without(env, "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS")
	env["ANTHROPIC_BASE_URL"] = url
	env["ANTHROPIC_AUTH_TOKEN"] = token
	env["ANTHROPIC_API_KEY"] = ""
	env["ANTHROPIC_MODEL"] = model
	for _, role := range []string{"opus", "sonnet", "haiku", "fable"} {
		env["ANTHROPIC_DEFAULT_"+strings.ToUpper(role)+"_MODEL"] = fallback(aliases[role], model)
	}
	env["CLAUDE_CODE_SUBAGENT_MODEL"] = "inherit"
	env["CLAUDE_CODE_SUBAGENT_MODEL_FORCE"] = "0"
	env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	// A configured HTTP(S)_PROXY must never intercept the loopback bridge.
	bypass := []string{}
	for _, key := range []string{"NO_PROXY", "no_proxy"} {
		for _, host := range strings.Split(env[key], ",") {
			if host = strings.TrimSpace(host); host != "" && !slices.Contains(bypass, host) {
				bypass = append(bypass, host)
			}
		}
	}
	for _, host := range []string{"127.0.0.1", "localhost"} {
		if !slices.Contains(bypass, host) {
			bypass = append(bypass, host)
		}
	}
	env["NO_PROXY"] = strings.Join(bypass, ",")
	env["no_proxy"] = env["NO_PROXY"]
	if window > 0 {
		env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = fmt.Sprint(window)
		env["CLAUDE_CODE_DISABLE_1M_CONTEXT"] = "1"
	}
	return env
}
