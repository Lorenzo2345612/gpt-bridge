package main

import (
	"fmt"
	"strings"
)

// Tests use the same chunk reconstruction the native model is instructed to do.
func contextText(context M, part string) string {
	var manifest M
	_ = decode([]byte(str(object(context["claude_host_manifest"])["value"])), &manifest)
	var text strings.Builder
	for i := 0; i < integer(manifest[part+"_chunks"]); i++ {
		text.WriteString(str(object(context[fmt.Sprintf("claude_host_%s_%06d", part, i)])["value"]))
	}
	return text.String()
}
