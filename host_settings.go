package main

import (
	"fmt"
	"os"
	"strings"
)

// Claude's --settings flag is a single option, not an accumulating list.
// Hosts such as cw supply hook settings after our arguments; forward one merged
// file so those hooks cannot replace the provider's model catalog and vice versa.
func mergeSettings(dst, src M) {
	for k, v := range src {
		if nested, ok := v.(map[string]any); ok {
			current := object(dst[k])
			if current == nil {
				current = M{}
			}
			mergeSettings(current, nested)
			dst[k] = current
		} else if list, ok := v.([]any); ok {
			if old, ok := dst[k].([]any); ok {
				dst[k] = append(append([]any{}, old...), list...)
			} else {
				dst[k] = list
			}
		} else {
			dst[k] = v
		}
	}
}

func mergedHostSettings(args []string, provider M) (M, []string, error) {
	settings := M{}
	rest := []string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, value, inline := strings.Cut(args[i], "=")
		if name != "--settings" {
			rest = append(rest, args[i])
			continue
		}
		if !inline {
			i++
			if i >= len(args) {
				return nil, nil, bad("--settings requires a JSON object or file")
			}
			value = args[i]
		}
		raw := []byte(strings.TrimSpace(value))
		if len(raw) == 0 {
			return nil, nil, bad("--settings requires a JSON object or file")
		}
		if raw[0] != '{' {
			var err error
			raw, err = os.ReadFile(value)
			if err != nil {
				return nil, nil, fmt.Errorf("could not read host settings: %w", err)
			}
		}
		var host M
		if decode(raw, &host) != nil || host == nil {
			return nil, nil, bad("Host --settings must contain a valid JSON object")
		}
		mergeSettings(settings, host)
	}
	// Preserve user/host hooks and unrelated settings. Provider routing keys and
	// the model picker must describe the backend this bridge actually launches.
	for k, v := range provider {
		if k == "env" {
			current := object(settings[k])
			if current == nil {
				current = M{}
			}
			mergeSettings(current, object(v))
			settings[k] = current
		} else {
			settings[k] = v
		}
	}
	return settings, rest, nil
}
