package main

import (
	"fmt"
	"strings"
)

// Tests use the same chunk reconstruction the native model is instructed to do.
func contextManifest(context M) M {
	var header, manifest M
	_ = decode([]byte(str(object(context["claude_host_manifest"])["value"])), &header)
	var text strings.Builder
	for i := 0; i < integer(header["manifest_chunks"]); i++ {
		text.WriteString(str(object(context[fmt.Sprintf("claude_host_manifest_%06d", i)])["value"]))
	}
	_ = decode([]byte(text.String()), &manifest)
	return manifest
}

func contextText(context M, part string) string {
	manifest := contextManifest(context)
	var text strings.Builder
	if part == "system" {
		for _, id := range array(manifest["system"]) {
			text.WriteString(str(object(context["claude_host_system_"+str(id)])["value"]))
		}
		return text.String()
	}
	tools := []string{}
	for _, v := range array(manifest["tools"]) {
		pair := array(v)
		var tool strings.Builder
		for i := 0; i < integer(pair[1]); i++ {
			tool.WriteString(str(object(context[fmt.Sprintf("claude_host_tool_%s_%03d", str(pair[0]), i)])["value"]))
		}
		tools = append(tools, tool.String())
	}
	return "[" + strings.Join(tools, ",") + "]"
}

func changedHostKeys(before, after M) int {
	changed := 0
	for key, v := range after {
		if key == "claude_host_manifest" || strings.HasPrefix(key, "claude_host_manifest_") {
			continue
		}
		if old, ok := before[key]; !ok || str(object(old)["value"]) != str(object(v)["value"]) {
			changed++
		}
	}
	return changed
}
