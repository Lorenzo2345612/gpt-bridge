package main

import (
	"context"
	"strings"
)

// Host summaries must also release native context. Appending them to an
// uncompressed native history makes Claude immediately compact again because
// its usage accounting sees Codex's still-large prompt.
func (c *conversation) compactNative(ctx context.Context) error {
	if _, err := c.rpc.Call(ctx, "thread/compact/start", M{"threadId": c.threadID}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.rpc.Done():
			return upstream("Codex exited during native compaction")
		case event := <-c.rpc.Events():
			p := object(event["params"])
			if id := str(p["threadId"]); id != "" && id != c.threadID {
				continue
			}
			if id, ok := event["id"]; ok {
				_ = c.rpc.Send(M{"id": id, "error": M{"code": -32601, "message": "Tools are unavailable during context compaction"}})
				return upstream("Codex requested a tool during native compaction")
			}
			switch str(event["method"]) {
			case "turn/completed":
				turn := object(p["turn"])
				if turn["status"] != "completed" {
					return upstream("Codex native compaction failed: %s", encode(turn["error"]))
				}
				c.usage = M{"input_tokens": 0, "output_tokens": 0}
				return nil
			case "error":
				if !boolean(p["willRetry"]) {
					return upstream("Codex native compaction: %s", str(object(p["error"])["message"]))
				}
			}
		}
	}
}

func (c *conversation) interruptForCompaction(ctx context.Context) error {
	if len(c.pending) == 0 {
		return nil
	}
	turn := c.pending[0].TurnID
	if _, err := c.rpc.Call(ctx, "turn/interrupt", M{"threadId": c.threadID, "turnId": turn}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.rpc.Done():
			return upstream("Codex exited while compacting host history")
		case event := <-c.rpc.Events():
			if id, ok := event["id"]; ok {
				_ = c.rpc.Send(M{"id": id, "error": M{"code": -32800, "message": "Host compacted the transcript; pending invocation cancelled"}})
			}
			if event["method"] == "turn/completed" {
				c.pending = nil
				return nil
			}
		}
	}
}

func compactionRequest(messages []any) bool {
	if len(messages) == 0 {
		return false
	}
	last := object(messages[len(messages)-1])
	if last["role"] != "user" {
		return false
	}
	bs, _ := blocks(last["content"])
	for _, v := range bs {
		text := str(object(v)["text"])
		if strings.Contains(text, "Your task is to create a detailed summary of the conversation so far") && strings.Contains(text, "<summary>") {
			return true
		}
	}
	return false
}
