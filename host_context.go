package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

const dispatcherName = "cc_dispatch"
const dispatcherInstructions = "\nHost configuration arrives as application context. claude_host_manifest gives manifest_chunks; concatenate claude_host_manifest_000000, claude_host_manifest_000001, ... in numeric order to get a JSON manifest. Manifest \"system\" is an ordered list of ids: concatenating the values of claude_host_system_<id> in that order (ids may repeat) recovers the complete current host instructions exactly. Manifest \"tools\" lists [id, chunks] pairs: concatenating claude_host_tool_<id>_000 .. claude_host_tool_<id>_<chunks-1> recovers one tool definition as JSON; together they form the current tool catalog. A key keeps its latest value until replaced, and unchanged keys are not re-sent. Only keys referenced by the latest manifest are current; ignore any other claude_host_* value as obsolete. Current host instructions supersede previous host instructions. Follow the current catalog: call cc_dispatch with name equal to a listed Claude tool name and arguments matching that tool's inputSchema. Never call a removed or unlisted tool. Previously loaded skill contents and completed tool results remain in the conversation; do not reload or repeat them merely because host configuration was updated.\n"

func dispatcherSpecs() []any {
	return []any{M{"name": dispatcherName, "description": "Call a currently available Claude Code tool. The host executes it with Claude permissions and hooks. Use the current host tool catalog for names and input schemas.", "inputSchema": M{"type": "object", "properties": M{"name": M{"type": "string"}, "arguments": M{"type": "object", "additionalProperties": true}}, "required": []any{"name", "arguments"}, "additionalProperties": false}}}
}

// Codex caps EACH additional-context value at 1,000 tokens. Splitting at
// <=768 UTF-8 bytes leaves room for wrappers even for worst-case tokenization.
const hostChunkBytes = 768

// Codex re-sends only additional-context keys whose value changed, and every
// re-sent value stays in native history. Keys must therefore depend on content
// (system) or identity (tools), never on byte offsets: with positional keys one
// edit near the start of Claude's system prompt shifted every later chunk and
// re-sent the whole prompt and tool catalog on almost every turn.
func splitHostValue(value string) []string {
	parts := []string{}
	for len(value) > 0 {
		end := len(value)
		if end > hostChunkBytes {
			end = hostChunkBytes
			for !utf8.RuneStart(value[end]) {
				end--
			}
		}
		parts = append(parts, value[:end])
		value = value[end:]
	}
	return parts
}

// systemChunks packs paragraphs into chunks whose boundaries are chosen by
// paragraph content, so an edit only changes the chunks around it. The
// concatenation of the result is exactly the input.
func systemChunks(system string) []string {
	chunks := []string{}
	var current strings.Builder
	flush := func() {
		if current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
	}
	for len(system) > 0 {
		end := strings.Index(system, "\n\n")
		if end < 0 {
			end = len(system)
		} else {
			end += 2
		}
		paragraph := system[:end]
		system = system[end:]
		if len(paragraph) > hostChunkBytes {
			flush()
			chunks = append(chunks, splitHostValue(paragraph)...)
			continue
		}
		if current.Len()+len(paragraph) > hostChunkBytes {
			flush()
		}
		current.WriteString(paragraph)
		if s := sha256.Sum256([]byte(paragraph)); current.Len() >= hostChunkBytes*2/3 && s[0]&3 == 0 {
			flush()
		}
	}
	flush()
	return chunks
}

func hostKeyID(value string) string {
	s := sha256.Sum256([]byte(value))
	return hex.EncodeToString(s[:6])
}

func currentHostContext(p *prepared) M {
	context := M{}
	put := func(key, value string) {
		context[key] = M{"kind": "application", "value": value}
	}
	system := []string{}
	for _, chunk := range systemChunks(p.System) {
		id := hostKeyID(chunk)
		put("claude_host_system_"+id, chunk)
		system = append(system, id)
	}
	tools := []any{}
	for _, v := range p.Specs {
		spec := copyMap(object(v))
		name := p.Names[str(spec["name"])]
		spec["name"] = name
		id := hostKeyID(name)
		chunks := splitHostValue(string(encode(spec)))
		for i, chunk := range chunks {
			put(fmt.Sprintf("claude_host_tool_%s_%03d", id, i), chunk)
		}
		tools = append(tools, []any{id, len(chunks)})
	}
	manifest := splitHostValue(string(encode(M{"system": system, "tools": tools})))
	for i, chunk := range manifest {
		put(fmt.Sprintf("claude_host_manifest_%06d", i), chunk)
	}
	put("claude_host_manifest", string(encode(M{"manifest_chunks": len(manifest), "revision": hash(M{"system": p.System, "specs": p.Specs})[:16]})))
	return context
}

func (c *conversation) updateHost(p *prepared) {
	c.names = p.Names
	c.baseSystemHash = p.BaseSystemHash
	c.allowed = map[string]bool{}
	for _, name := range p.Names {
		c.allowed[name] = true
	}
	c.currentContext = currentHostContext(p)
	c.contextChanged = c.contextHash != hash(c.currentContext)
	c.effort = p.Effort
}

func (c *conversation) contextSent() {
	c.contextHash = hash(c.currentContext)
	c.contextChanged = false
}
