package main

import (
	"fmt"
	"unicode/utf8"
)

const dispatcherName = "cc_dispatch"
const dispatcherInstructions = "\nHost configuration arrives as application context under claude_host_manifest, claude_host_system_NNNNNN and claude_host_tools_NNNNNN. The manifest gives the current chunk counts. Concatenate each numbered series in numeric order to recover the complete current instructions and JSON tool catalog. The latest value for each key replaces previous values; chunks outside the latest counts are obsolete. Current host instructions supersede previous host instructions. Follow the current catalog: call cc_dispatch with name equal to a listed Claude tool name and arguments matching that tool's inputSchema. Never call a removed or unlisted tool. Previously loaded skill contents and completed tool results remain in the conversation; do not reload or repeat them merely because host configuration was updated.\n"

func dispatcherSpecs() []any {
	return []any{M{"name": dispatcherName, "description": "Call a currently available Claude Code tool. The host executes it with Claude permissions and hooks. Use the current host tool catalog for names and input schemas.", "inputSchema": M{"type": "object", "properties": M{"name": M{"type": "string"}, "arguments": M{"type": "object", "additionalProperties": true}}, "required": []any{"name", "arguments"}, "additionalProperties": false}}}
}

func currentHostContext(p *prepared) M {
	tools := []any{}
	for _, v := range p.Specs {
		spec := copyMap(object(v))
		spec["name"] = p.Names[str(spec["name"])]
		tools = append(tools, spec)
	}
	context := M{}
	// Codex caps EACH additional-context value at 1,000 tokens. Splitting at
	// <=768 UTF-8 bytes leaves room for wrappers even for worst-case tokenization.
	put := func(prefix, value string) int {
		count := 0
		for len(value) > 0 {
			end := len(value)
			if end > 768 {
				end = 768
				for !utf8.RuneStart(value[end]) {
					end--
				}
			}
			context[fmt.Sprintf("claude_host_%s_%06d", prefix, count)] = M{"kind": "application", "value": value[:end]}
			value = value[end:]
			count++
		}
		return count
	}
	systemChunks := put("system", p.System)
	toolChunks := put("tools", string(encode(tools)))
	manifest := M{"system_chunks": systemChunks, "tools_chunks": toolChunks, "revision": hash(M{"system": p.System, "tools": tools})}
	context["claude_host_manifest"] = M{"kind": "application", "value": string(encode(manifest))}
	return context
}

func (c *conversation) updateHost(p *prepared) {
	c.names = p.Names
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
