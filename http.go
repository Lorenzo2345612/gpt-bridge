package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type apiHandler struct {
	bridge *Bridge
	token  string
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	raw := encode(value)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	supplied := r.Header.Get("x-api-key")
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		supplied = strings.TrimPrefix(auth, "Bearer ")
	}
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(h.token)) != 1 {
		jsonResponse(w, 401, M{"type": "error", "error": M{"type": "authentication_error", "message": "Local bridge token required"}})
		return
	}
	if r.Method == "GET" {
		switch r.URL.Path {
		case "/health":
			jsonResponse(w, 200, M{"status": "ok", "backend": "codex-app-server", "runtime": "go"})
		case "/v1/models":
			entries := []any{}
			for _, m := range h.bridge.catalog {
				entries = append(entries, M{"id": m["model"], "type": "model", "display_name": fallback(str(m["displayName"]), str(m["model"])), "description": fallback(str(m["description"]), ""), "created_at": "2026-10-20T00:00:00Z"})
			}
			jsonResponse(w, 200, M{"data": entries, "has_more": false, "first_id": object(entries[0])["id"], "last_id": object(entries[len(entries)-1])["id"]})
		default:
			jsonResponse(w, 404, M{"error": M{"message": "Unknown endpoint"}})
		}
		return
	}
	if r.Method != "POST" || (r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens") {
		jsonResponse(w, 404, M{"error": M{"message": "Unknown endpoint"}})
		return
	}
	streaming := false
	metric := requestMetric{start: time.Now()}
	defer func() { h.bridge.recordMetric(metric) }()
	fail := func(err error) {
		status := errorStatus(err)
		metric.Status = status
		if os.Getenv("CLAUDE_CODEX_TRACE") == "1" {
			fmt.Fprintf(os.Stderr, "gpt-bridge: event=error status=%d error=%q\n", status, err.Error())
		}
		kind := "invalid_request_error"
		if status >= 500 {
			kind = "api_error"
		}
		if status == 429 {
			kind = "rate_limit_error"
		}
		message := err.Error()
		if _, ok := err.(*bridgeError); !ok {
			message = "Invalid request or local runtime error"
		}
		value := M{"type": "error", "error": M{"type": kind, "message": message}}
		if streaming {
			_ = writeSSE(w, "error", value)
		} else {
			jsonResponse(w, status, value)
		}
	}
	const maxBody = 32 << 20
	if len(r.TransferEncoding) > 0 {
		fail(bad("Chunked request bodies are unsupported"))
		return
	}
	if r.ContentLength < 1 || r.ContentLength > maxBody {
		fail(&bridgeError{413, "Request must be between 1 byte and 32 MiB"})
		return
	}
	// Limit both body size and read time, including clients that declare a length
	// and never finish writing. Reset before the potentially long model response.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
	raw, e := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	_ = controller.SetReadDeadline(time.Time{})
	if e != nil {
		fail(bad("Invalid or incomplete request body"))
		return
	}
	var body M
	if decode(raw, &body) != nil || body == nil {
		fail(bad("Invalid JSON request"))
		return
	}
	if r.URL.Path == "/v1/messages/count_tokens" {
		n := len(encode(body)) / 3
		if n < 1 {
			n = 1
		}
		jsonResponse(w, 200, M{"input_tokens": n})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.bridge.timeout)
	defer cancel()
	c, p, key, cached, e := h.bridge.acquire(ctx, body)
	if e != nil {
		fail(e)
		return
	}
	successful := false
	if c != nil {
		defer c.mu.Unlock()
		defer func() {
			if !successful {
				h.bridge.discard(c)
			}
		}()
		metric.FreshThread = c.freshThread || c.threadID == ""
		if e = c.advance(ctx, p); e != nil {
			fail(e)
			return
		}
		metric.Thread = c.threadID
		metric.SetupMS = time.Since(metric.start).Milliseconds()
	}
	metric.Model = p.Model
	metric.Replay = cached != nil
	wantThinking := str(object(body["thinking"])["type"]) != "" && str(object(body["thinking"])["type"]) != "disabled"
	response := M{"id": uid("msg_"), "type": "message", "role": "assistant", "model": p.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": M{"input_tokens": 0, "output_tokens": 0}}
	if cached != nil {
		var old M
		if e = decode(cached, &old); e != nil {
			fail(e)
			return
		}
		// A replay is the same response, including its durable lineage identity.
		response["id"] = old["id"]
	}
	if boolean(body["stream"]) {
		streaming = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "close")
		w.WriteHeader(200)
		if e = writeSSE(w, "message_start", M{"type": "message_start", "message": response}); e != nil {
			return
		}
	}
	content := []any{}
	openIndex, openKind := -1, ""
	closeText := func() error {
		if openIndex >= 0 {
			index := openIndex
			openIndex, openKind = -1, ""
			if streaming {
				return writeSSE(w, "content_block_stop", M{"type": "content_block_stop", "index": index})
			}
		}
		return nil
	}
	emit := func(kind string, value any) error {
		if kind != "ping" && metric.firstOutput.IsZero() {
			metric.firstOutput = time.Now()
			metric.FirstOutputMS = metric.firstOutput.Sub(metric.start).Milliseconds()
		}
		switch kind {
		case "ping":
			if streaming {
				return writeSSE(w, "ping", M{"type": "ping"})
			}
		case "text", "thinking":
			delta := str(value)
			if delta == "" || (kind == "thinking" && !wantThinking) {
				return nil
			}
			if kind == "text" && metric.FirstTextMS == 0 {
				metric.FirstTextMS = time.Since(metric.start).Milliseconds()
			}
			field, deltaType := "text", "text_delta"
			if kind == "thinking" {
				field, deltaType = "thinking", "thinking_delta"
			}
			if openKind != kind {
				if e := closeText(); e != nil {
					return e
				}
				openIndex, openKind = len(content), kind
				start := M{"type": kind, field: ""}
				if kind == "thinking" {
					// Codex reasoning summaries carry no Anthropic signature; history
					// parsing drops thinking blocks, so it is never sent back upstream.
					start["signature"] = ""
				}
				content = append(content, start)
				if streaming {
					if e := writeSSE(w, "content_block_start", M{"type": "content_block_start", "index": openIndex, "content_block": copyMap(start)}); e != nil {
						return e
					}
				}
			}
			block := object(content[openIndex])
			block[field] = str(block[field]) + delta
			if streaming {
				return writeSSE(w, "content_block_delta", M{"type": "content_block_delta", "index": openIndex, "delta": M{"type": deltaType, field: delta}})
			}
		case "tool", "done":
			if e := closeText(); e != nil {
				return e
			}
			v := object(value)
			if kind == "tool" {
				metric.Tools++
			}
			if kind == "done" {
				response["stop_reason"] = v["reason"]
				response["usage"] = v["usage"]
				metric.Stop = str(v["reason"])
				metric.Usage = object(v["usage"])
				return nil
			}
			index := len(content)
			content = append(content, v)
			if streaming {
				start := copyMap(v)
				start["input"] = M{}
				if e := writeSSE(w, "content_block_start", M{"type": "content_block_start", "index": index, "content_block": start}); e != nil {
					return e
				}
				if e := writeSSE(w, "content_block_delta", M{"type": "content_block_delta", "index": index, "delta": M{"type": "input_json_delta", "partial_json": string(encode(v["input"]))}}); e != nil {
					return e
				}
				return writeSSE(w, "content_block_stop", M{"type": "content_block_stop", "index": index})
			}
		}
		return nil
	}
	if c != nil {
		e = c.events(ctx, emit)
	} else {
		var old M
		if e = decode(cached, &old); e == nil {
			for _, v := range array(old["content"]) {
				b := object(v)
				if b["type"] == "text" {
					e = emit("text", b["text"])
				} else if b["type"] == "thinking" {
					e = emit("thinking", b["thinking"])
				} else {
					e = emit("tool", b)
				}
				if e != nil {
					break
				}
			}
			if e == nil {
				e = emit("done", M{"reason": old["stop_reason"], "usage": old["usage"]})
			}
		}
	}
	if e != nil {
		fail(e)
		return
	}
	response["content"] = content
	if c != nil {
		if e = h.bridge.commit(c, p, key, response); e != nil {
			fail(e)
			return
		}
	}
	successful = true
	if streaming {
		if e = writeSSE(w, "message_delta", M{"type": "message_delta", "delta": M{"stop_reason": response["stop_reason"], "stop_sequence": nil}, "usage": response["usage"]}); e == nil {
			e = writeSSE(w, "message_stop", M{"type": "message_stop"})
		}
	} else {
		jsonResponse(w, 200, response)
	}
}
func writeSSE(w http.ResponseWriter, event string, value any) error {
	if _, e := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encode(value)); e != nil {
		return e
	}
	return http.NewResponseController(w).Flush()
}
