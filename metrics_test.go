package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gpt-bridge-metrics-")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CODEX_METRICS_FILE", filepath.Join(dir, "metrics.jsonl"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestReasoningStreamsAsThinkingAndMetricsRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	t.Setenv("CLAUDE_CODEX_METRICS_FILE", path)
	f := newHTTPFixture(t)
	f.configure = func(r *fakeRPC) { r.reasoning = true }
	body := testRequest(false)
	body["stream"] = true
	body["thinking"] = M{"type": "adaptive"}
	status, raw := f.fetch(t, "POST", "/v1/messages", body, "test-local-secret-123456789")
	if status != 200 || !bytes.Contains(raw, []byte(`"thinking_delta"`)) || !bytes.Contains(raw, []byte("Revisando el código")) || !bytes.Contains(raw, []byte("message_stop")) {
		t.Fatal(status, string(raw))
	}
	if bytes.Index(raw, []byte(`"thinking_delta"`)) > bytes.Index(raw, []byte(`"text_delta"`)) {
		t.Fatal("thinking must precede text", string(raw))
	}
	delete(body, "thinking")
	body["messages"] = append(array(body["messages"]), M{"role": "assistant", "content": []any{M{"type": "text", "text": "x"}}}, M{"role": "user", "content": "otra"})
	_, raw = f.fetch(t, "POST", "/v1/messages", body, "test-local-secret-123456789")
	if bytes.Contains(raw, []byte("thinking")) {
		t.Fatal("thinking emitted without being requested", string(raw))
	}
	logged, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(logged, []byte(`"first_output_ms"`)) || bytes.Contains(logged, []byte("Revisando")) {
		t.Fatal(err, string(logged))
	}
}

func TestCommentaryAndReconnectsAreProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.jsonl")
	t.Setenv("CLAUDE_CODEX_METRICS_FILE", path)
	for _, thinking := range []bool{true, false} {
		f := newHTTPFixture(t)
		f.configure = func(r *fakeRPC) { r.commentary = true }
		body := testRequest(false)
		if thinking {
			body["thinking"] = M{"type": "adaptive"}
		}
		status, raw := f.fetch(t, "POST", "/v1/messages", body, "test-local-secret-123456789")
		var response M
		if status != 200 || decode(raw, &response) != nil {
			t.Fatal(status, string(raw))
		}
		got := map[string]string{}
		for _, v := range array(response["content"]) {
			b := object(v)
			got[str(b["type"])] += str(b["text"]) + str(b["thinking"])
		}
		if thinking {
			if !strings.Contains(got["thinking"], "Estoy revisando") || !strings.Contains(got["thinking"], "Reconectando con OpenAI (intento 1)") || got["text"] != "Primera parte.\n\n¡Hecho!" {
				t.Fatal(got)
			}
		} else if got["thinking"] != "" || got["text"] != "Estoy revisando\n\nPrimera parte.\n\n¡Hecho!" {
			t.Fatalf("%q", got)
		}
	}
	logged, _ := os.ReadFile(path)
	if !bytes.Contains(logged, []byte(`"retries":1`)) {
		t.Fatal(string(logged))
	}
}
