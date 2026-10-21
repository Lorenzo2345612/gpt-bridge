package main

import (
	"fmt"
	"testing"
)

func TestOptionalSessionLimit(t *testing.T) {
	for _, args := range [][]string{nil, {"--max-sessions", "0"}, {"--max-sessions=12"}} {
		o, err := parseOptions(args)
		if err != nil {
			t.Fatal(err)
		}
		expected := 0
		if len(args) == 1 {
			expected = 12
		}
		if o.maxSessions != expected {
			t.Fatal(o.maxSessions, expected)
		}
	}
	if _, err := parseOptions([]string{"--max-sessions", "-1"}); err == nil {
		t.Fatal("Accepted negative session limit")
	}
	for _, limit := range []int{0, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			f := newHTTPFixture(t)
			f.b.maxSessions = limit
			count := 34
			if limit > 0 {
				count = limit + 1
			}
			for i := 0; i < count; i++ {
				body := testRequest(true)
				body["metadata"] = M{"user_id": fmt.Sprintf("independent-%d", i)}
				status, response := f.post(t, body)
				if limit > 0 && i == limit {
					if status != 503 {
						t.Fatal(status, response)
					}
				} else if status != 200 || response["stop_reason"] != "tool_use" {
					t.Fatal(status, response)
				}
			}
			f.b.mu.Lock()
			defer f.b.mu.Unlock()
			expected := count
			if limit > 0 {
				expected = limit
			}
			if len(f.b.sessions) != expected {
				t.Fatal(len(f.b.sessions), expected)
			}
		})
	}
}
