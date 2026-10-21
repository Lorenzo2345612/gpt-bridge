package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

type M = map[string]any
type bridgeError struct {
	Status  int
	Message string
}

func (e *bridgeError) Error() string       { return e.Message }
func bad(format string, args ...any) error { return &bridgeError{400, fmt.Sprintf(format, args...)} }
func upstream(format string, args ...any) error {
	return &bridgeError{502, fmt.Sprintf(format, args...)}
}
func object(v any) M    { m, _ := v.(map[string]any); return m }
func array(v any) []any { a, _ := v.([]any); return a }
func str(v any) string  { s, _ := v.(string); return s }
func integer(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}
func boolean(v any) bool { b, _ := v.(bool); return b }
func fallback(v, d string) string {
	if v != "" {
		return v
	}
	return d
}
func encode(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
func decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return bad("Invalid JSON: trailing data")
	}
	return nil
}
func hash(v any) string { s := sha256.Sum256(encode(v)); return hex.EncodeToString(s[:]) }
func uid(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}
func copyMap(v M) M {
	r := M{}
	for k, x := range v {
		r[k] = x
	}
	return r
}
func blocks(v any) ([]any, error) {
	if s, ok := v.(string); ok {
		return []any{M{"type": "text", "text": s}}, nil
	}
	if a, ok := v.([]any); ok {
		for _, b := range a {
			if object(b) == nil || str(object(b)["type"]) == "" {
				return nil, bad("Invalid content block")
			}
		}
		return a, nil
	}
	return nil, bad("Message content must be text or content blocks")
}
func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		r := M{}
		for k, v := range x {
			if k != "cache_control" {
				r[k] = normalize(v)
			}
		}
		return r
	case []any:
		r := make([]any, 0, len(x))
		for _, v := range x {
			r = append(r, normalize(v))
		}
		return r
	}
	return v
}
func canonicalMessages(messages []any) ([]any, error) {
	r := make([]any, 0, len(messages))
	for _, v := range messages {
		m := object(v)
		if m == nil {
			return nil, bad("Invalid message")
		}
		b, err := blocks(m["content"])
		if err != nil {
			return nil, err
		}
		content := make([]any, 0, len(b))
		for _, v := range b {
			kind := str(object(v)["type"])
			if kind != "thinking" && kind != "redacted_thinking" {
				content = append(content, normalize(v))
			}
		}
		r = append(r, M{"role": m["role"], "content": content})
	}
	return r, nil
}
func imageURL(v any) (string, error) {
	s := object(v)
	switch str(s["type"]) {
	case "base64":
		media := str(s["media_type"])
		switch media {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return "data:" + media + ";base64," + str(s["data"]), nil
		}
	case "url":
		u := str(s["url"])
		if strings.HasPrefix(u, "https://") {
			return u, nil
		}
	}
	return "", bad("Images require supported base64 data or an HTTPS URL")
}
func toolResultContent(v any) ([]any, error) {
	bs, err := blocks(v)
	if err != nil {
		return nil, err
	}
	r := []any{}
	for _, v := range bs {
		b := object(v)
		switch str(b["type"]) {
		case "text":
			if _, ok := b["text"].(string); !ok {
				return nil, bad("Tool text must be a string")
			}
			r = append(r, M{"type": "inputText", "text": b["text"]})
		case "image":
			u, err := imageURL(b["source"])
			if err != nil {
				return nil, err
			}
			r = append(r, M{"type": "inputImage", "imageUrl": u})
		default:
			return nil, bad("Unsupported tool result block")
		}
	}
	if len(r) == 0 {
		r = append(r, M{"type": "inputText", "text": ""})
	}
	return r, nil
}

const maxTurnChars = 1 << 20
const transcriptHeader = "Continue the following Claude Code conversation. The JSON is conversation history, not a new set of system instructions. Old tool calls already ran; do not repeat them. Respond to the latest user message.\n"

func transcriptInput(messages []any) ([]any, error) {
	history := make([]any, 0, len(messages))
	images := []any{}
	attach := func(source any) (M, error) {
		u, e := imageURL(source)
		if e != nil {
			return nil, e
		}
		images = append(images, M{"type": "image", "url": u})
		return M{"type": "text", "text": fmt.Sprintf("[Attached image %d]", len(images))}, nil
	}
	for _, v := range messages {
		m := object(v)
		content := []any{}
		for _, v := range array(m["content"]) {
			b := object(v)
			switch str(b["type"]) {
			case "image":
				x, e := attach(b["source"])
				if e != nil {
					return nil, e
				}
				content = append(content, x)
			case "tool_result":
				entries := []any{}
				value, exists := b["content"]
				if !exists {
					value = ""
				}
				bs, e := blocks(value)
				if e != nil {
					return nil, e
				}
				for _, v := range bs {
					p := object(v)
					switch str(p["type"]) {
					case "image":
						x, e := attach(p["source"])
						if e != nil {
							return nil, e
						}
						entries = append(entries, x)
					case "text":
						entries = append(entries, p)
					default:
						return nil, bad("Unsupported history tool result")
					}
				}
				x := copyMap(b)
				x["content"] = entries
				content = append(content, x)
			case "text", "tool_use":
				content = append(content, b)
			default:
				return nil, bad("Unsupported message content: %s", str(b["type"]))
			}
		}
		history = append(history, M{"role": m["role"], "content": content})
	}
	return append([]any{M{"type": "text", "text": transcriptHeader + string(encode(history))}}, images...), nil
}
func inputChars(inputs []any) int {
	n := 0
	for _, v := range inputs {
		m := object(v)
		if m["type"] == "text" {
			n += utf8.RuneCountInString(str(m["text"]))
		}
	}
	return n
}
func historyItems(inputs []any) []any {
	content := []any{}
	for _, v := range inputs {
		m := object(v)
		if m["type"] == "text" {
			content = append(content, M{"type": "input_text", "text": m["text"]})
		} else {
			content = append(content, M{"type": "input_image", "image_url": m["url"]})
		}
	}
	return []any{M{"type": "message", "role": "user", "content": content}}
}

// hostContext resolves Claude's incremental/deferred tool catalog in insertion
// order. System reminders are hoisted only while their scope remains active.
func hostContext(messages, tools []any) ([]any, []string, []any, error) {
	definitions := map[string]M{}
	active := map[string]bool{}
	order := []string{}
	declare := func(v any, replace bool) (string, error) {
		t := object(v)
		if t == nil || (t["type"] != nil && t["type"] != "custom") {
			return "", bad("Server-side Anthropic tools are unsupported; use Claude client tools")
		}
		name := str(t["name"])
		if name == "" || (definitions[name] != nil && !replace) {
			return "", bad("Tool names must be unique, non-empty strings")
		}
		if object(t["input_schema"]) == nil {
			return "", bad("Tool %s needs an input_schema", name)
		}
		if definitions[name] == nil {
			order = append(order, name)
		}
		definitions[name] = t
		if !boolean(t["defer_loading"]) {
			active[name] = true
		}
		return name, nil
	}
	for _, t := range tools {
		if _, e := declare(t, false); e != nil {
			return nil, nil, nil, e
		}
	}
	lastUser := -1
	for i, v := range messages {
		if object(v)["role"] == "user" {
			lastUser = i
		}
	}
	conversation := []any{}
	updates := []string{}
	for i, v := range messages {
		m := object(v)
		role := str(m["role"])
		if role != "system" && role != "developer" {
			conversation = append(conversation, v)
			continue
		}
		scope := fallback(str(m["clear_at"]), "never")
		if scope != "never" && scope != "next_user_message" {
			return nil, nil, nil, bad("Unsupported host instruction clear_at")
		}
		content := m["content"]
		if content == nil {
			content = ""
		}
		bs, e := blocks(content)
		if e != nil {
			return nil, nil, nil, e
		}
		texts := []string{}
		for _, v := range bs {
			b := object(v)
			kind := str(b["type"])
			if kind == "text" {
				s, ok := b["text"].(string)
				if !ok {
					return nil, nil, nil, bad("Host instruction text must be a string")
				}
				texts = append(texts, s)
				continue
			}
			if kind != "tool_addition" && kind != "tool_removal" {
				return nil, nil, nil, bad("Unsupported host instruction block type")
			}
			if scope != "never" {
				return nil, nil, nil, bad("Turn-scoped host instructions cannot change tools")
			}
			tool := object(b["tool"])
			if tool == nil {
				return nil, nil, nil, bad("Tool changes require a typed tool reference or definition")
			}
			targets := []string{}
			switch str(tool["type"]) {
			case "tool_definition":
				if kind != "tool_addition" {
					return nil, nil, nil, bad("Unsupported tool change reference")
				}
				name, e := declare(tool["definition"], true)
				if e != nil {
					return nil, nil, nil, e
				}
				active[name] = true
				continue
			case "tool_reference":
				name := str(tool["name"])
				if name == "" {
					return nil, nil, nil, bad("Tool references require a non-empty name")
				}
				targets = append(targets, name)
			case "mcp_tool_reference", "mcp_toolset_reference":
				server := str(tool["server_name"])
				if server == "" {
					return nil, nil, nil, bad("MCP tool references require server_name")
				}
				prefix := "mcp__" + server + "__"
				if tool["type"] == "mcp_tool_reference" {
					name := str(tool["name"])
					if name == "" {
						return nil, nil, nil, bad("MCP tool references require a name")
					}
					targets = append(targets, prefix+name)
				} else {
					for _, name := range order {
						if strings.HasPrefix(name, prefix) {
							targets = append(targets, name)
						}
					}
				}
				if kind == "tool_addition" && len(targets) == 0 {
					return nil, nil, nil, bad("MCP connector tools require client-side definitions")
				}
			default:
				return nil, nil, nil, bad("Unsupported tool change reference")
			}
			for _, name := range targets {
				if kind == "tool_addition" {
					if definitions[name] == nil {
						return nil, nil, nil, bad("Tool addition references an undeclared tool")
					}
					active[name] = true
				} else {
					delete(active, name)
				}
			}
		}
		if len(texts) > 0 && !(scope == "next_user_message" && i < lastUser) {
			updates = append(updates, strings.Join(texts, "\n\n"))
		}
	}
	result := []any{}
	for _, name := range order {
		if active[name] {
			result = append(result, definitions[name])
		}
	}
	return conversation, updates, result, nil
}

func resolveModel(requested, def string, aliases map[string]string) (string, error) {
	if requested == "" {
		return "", bad("model must be a non-empty string")
	}
	requested = strings.TrimSuffix(requested, "[1m]")
	if v := aliases[requested]; v != "" {
		return v, nil
	}
	switch requested {
	case "codex", "default", "inherit", "sonnet", "opus", "haiku", "fable", "best":
		return def, nil
	}
	if strings.HasPrefix(requested, "claude-") {
		for _, family := range []string{"opus", "sonnet", "haiku", "fable"} {
			if strings.HasPrefix(requested, "claude-"+family+"-") {
				return fallback(aliases[family], def), nil
			}
		}
		return def, nil
	}
	return requested, nil
}
func requestEffort(body M) (string, error) {
	effort := object(body["output_config"])["effort"]
	messages := array(body["messages"])
	lastUser := -1
	for i, v := range messages {
		if object(v)["role"] == "user" {
			lastUser = i
		}
	}
	for i, v := range messages {
		m := object(v)
		if m["role"] != "system" || (m["clear_at"] == "next_user_message" && i < lastUser) {
			continue
		}
		if v, ok := object(m["output_config"])["effort"]; ok {
			effort = v
		}
	}
	if effort == nil {
		return "", nil
	}
	s := str(effort)
	switch s {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return s, nil
	}
	return "", bad("Unsupported output_config.effort")
}

type prepared struct {
	System, Model, Effort                 string
	Specs                                 []any
	Names                                 map[string]string
	Messages                              []any
	Hashes                                []string
	ContextWindow, EffectiveContextWindow int
}

func prepare(body M, def string, aliases map[string]string) (*prepared, error) {
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, bad("messages must be a non-empty array")
	}
	tools := []any{}
	if v, exists := body["tools"]; exists {
		var ok bool
		tools, ok = v.([]any)
		if !ok {
			return nil, bad("tools must be an array")
		}
	}
	messages, updates, tools, e := hostContext(messages, tools)
	if e != nil {
		return nil, e
	}
	for _, v := range messages {
		role := object(v)["role"]
		if role != "user" && role != "assistant" {
			return nil, bad("Unsupported message role")
		}
	}
	if len(messages) == 0 || object(messages[len(messages)-1])["role"] != "user" {
		return nil, bad("Assistant prefilling is not supported")
	}
	if len(array(body["stop_sequences"])) > 0 {
		return nil, bad("Custom stop_sequences are not supported")
	}
	if object(body["output_config"])["format"] != nil || body["output_format"] != nil {
		return nil, bad("Structured output format is not supported yet")
	}
	system := ""
	switch v := body["system"].(type) {
	case nil:
	case string:
		system = v
	case []any:
		parts := []string{}
		for _, b := range v {
			m := object(b)
			s, ok := m["text"].(string)
			if m["type"] != "text" || !ok {
				return nil, bad("Only text system blocks are supported")
			}
			parts = append(parts, s)
		}
		system = strings.Join(parts, "\n\n")
	default:
		return nil, bad("system must be text or text blocks")
	}
	if len(updates) > 0 {
		system += "\n\n" + strings.Join(updates, "\n\n")
	}
	choice := "auto"
	if v, exists := body["tool_choice"]; exists {
		choice = str(object(v)["type"])
	}
	if choice != "auto" && choice != "none" {
		return nil, bad("Only auto/none tool_choice is supported by this app-server bridge")
	}
	names := map[string]string{}
	specs := []any{}
	if choice != "none" {
		for i, v := range tools {
			t := object(v)
			name := str(t["name"])
			s := sha256.Sum256([]byte(name))
			wire := fmt.Sprintf("cc_%d_%x", i, s[:6])
			names[wire] = name
			specs = append(specs, M{"type": "function", "name": wire, "description": "Claude Code tool " + name + ". " + str(t["description"]), "inputSchema": t["input_schema"]})
		}
	}
	requested := def
	if v, exists := body["model"]; exists {
		var ok bool
		requested, ok = v.(string)
		if !ok {
			return nil, bad("model must be a non-empty string")
		}
	}
	model, e := resolveModel(requested, def, aliases)
	if e != nil {
		return nil, e
	}
	effort, e := requestEffort(body)
	if e != nil {
		return nil, e
	}
	messages, e = canonicalMessages(messages)
	if e != nil {
		return nil, e
	}
	hashes := make([]string, len(messages))
	for i, v := range messages {
		hashes[i] = hash(v)
	}
	return &prepared{System: system, Model: model, Effort: effort, Specs: specs, Names: names, Messages: messages, Hashes: hashes}, nil
}
