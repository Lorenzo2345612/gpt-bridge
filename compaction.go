package main

import (
	"context"
	"strings"
)

// Host summaries must also release native context, or Claude compacts again
// at once because its usage accounting sees Codex's still-large prompt. Codex
// compaction is not enough: it keeps every earlier user message, and for the
// bridge each Claude turn is one, including full transcript snapshots from
// earlier compactions (~70k tokens retained in a real session). Like Claude,
// keep only its summary: continue in a fresh native thread on the same
// executor, checkpoint and lease. The old thread stays on disk.
func (c *conversation) restartNative(ctx context.Context, p *prepared) error {
	result, err := c.rpc.Call(ctx, "thread/start", threadStartParams(p, c.cwd, c.statePath != ""))
	if err != nil {
		return err
	}
	id := str(object(result["thread"])["id"])
	if id == "" {
		return upstream("Codex returned an invalid thread")
	}
	c.threadID = id
	c.nativeCompacted()
	c.freshThread = true
	return nil
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

const awayRecapPrompt = "The user stepped away and is coming back. Recap"

// compactionRequest reports Claude's one-shot side requests (compaction summary,
// away recap). They run in ephemeral threads that are never persisted, reused
// or matched to the conversation.
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
		// Claude Code's away recap forks the whole conversation for a one-shot
		// side answer. Treated as a branch, every recap re-appended the full
		// transcript to one persistent native thread (215k -> 379k chars).
		if strings.HasPrefix(strings.TrimSpace(text), awayRecapPrompt) {
			return true
		}
	}
	return false
}

// recoveryShare caps how much of the window a recovered host view may push an
// existing native thread to. Above it Codex's own remote compaction no longer
// fits and the thread fails every later turn, so continue in a fresh thread
// from the host view instead (the old thread stays on disk).
const recoveryShare = 60

func (c *conversation) recoveryOverflows(p *prepared) bool {
	window := c.contextWindow
	if c.actualContextWindow > 0 {
		window = c.actualContextWindow
	}
	if window <= 0 || c.threadID == "" {
		return false
	}
	inputs, err := transcriptInput(p.Messages)
	if err != nil {
		return false
	}
	return c.contextTokens()+estimateItems(inputs) > window*recoveryShare/100
}

// contextOverflow reports Codex failures caused by the native thread no longer
// fitting the model's window, including failed remote compactions. Retrying the
// same thread can never succeed.
func contextOverflow(detail any) bool {
	text := strings.ToLower(string(encode(detail)))
	for _, marker := range []string{"ran out of room", "remote compact task", "your input exceeds", "context_length_exceeded", "maximum context length"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// overflowed fails the request as Claude's own "prompt is too long" so Claude
// Code compacts instead of retrying a server error, and makes the next request
// continue from the host transcript in a fresh native thread.
func (c *conversation) overflowed(detail any) error {
	c.overflow = true
	c.resetHistory = true
	_ = c.saveState()
	return &bridgeError{400, "prompt is too long: Codex ran out of room in the model's context window (" + strings.TrimSpace(fallback(str(object(detail)["message"]), string(encode(detail)))) + "). The next request continues in a fresh Codex thread; run /compact if it persists."}
}
