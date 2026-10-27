package main

import "testing"

// Mirrors Codex's order: a sample's tokenUsage arrives only after the bridge has
// returned that sample's tool result.
func TestToolUseUsageIncludesResultsCodexHasNotMeasured(t *testing.T) {
	c := &conversation{usage: M{"input_tokens": 0, "output_tokens": 0}}
	c.addUnmeasured(1000) // turn input
	c.addUnmeasured(10)   // sample 1 tool arguments
	c.sampleEnded()
	if got := integer(c.reportedUsage()["input_tokens"]); got != 1010 {
		t.Fatal("first tool_use before any measurement", got)
	}
	c.addUnmeasured(estimateItems([]any{M{"type": "inputText", "text": string(make([]byte, 400000))}})) // 100k tokens of turn input are not truncated
	c.sampleMeasured(M{"input_tokens": 1000, "output_tokens": 10, "cache_read_input_tokens": 0})
	c.addUnmeasured(5) // sample 2 tool arguments
	c.sampleEnded()
	if got := integer(c.reportedUsage()["input_tokens"]); got != 1000+100000+5 {
		t.Fatal("tool_use reported a stale sample", got)
	}
	c.addUnmeasured(estimateItems([]any{M{"type": "inputImage", "imageUrl": "data:"}}))
	c.sampleMeasured(M{"input_tokens": 101000, "output_tokens": 5, "cache_read_input_tokens": 0})
	c.sampleMeasured(M{"input_tokens": 101900, "output_tokens": 20, "cache_read_input_tokens": 0}) // final answer
	c.turnEnded()
	if got := integer(c.reportedUsage()["input_tokens"]); got != 101900 {
		t.Fatal("end_turn must report the measured sample only", got)
	}
	c.nativeCompacted()
	c.addUnmeasured(300)
	c.sampleEnded()
	if got := integer(c.reportedUsage()["input_tokens"]); got != 300 {
		t.Fatal("compaction kept estimates of the replaced history", got)
	}
}

func TestContextDeltaSendsOnlyKeysTheThreadLacks(t *testing.T) {
	c := &conversation{currentContext: M{"a": M{"value": string(make([]byte, 4000))}, "b": M{"value": "xxxx"}}}
	delta, sent := c.contextDelta()
	if len(delta) != 2 {
		t.Fatal(delta)
	}
	sent()
	c.currentContext = M{"a": M{"value": string(make([]byte, 4000))}, "b": M{"value": string(make([]byte, 800))}}
	delta, sent = c.contextDelta()
	if len(delta) != 1 || delta["b"] == nil {
		t.Fatal(delta)
	}
	sent()
	if c.unmeasured[1] != 1000+1+200 {
		t.Fatal(c.unmeasured)
	}
	// A resumed executor restores the hashes from its checkpoint.
	resumed := &conversation{sentContext: c.sentContext, currentContext: c.currentContext}
	if delta, _ := resumed.contextDelta(); len(delta) != 0 {
		t.Fatal("resume re-sent unchanged host context", delta)
	}
	c.nativeCompacted()
	if delta, _ := c.contextDelta(); len(delta) != 2 {
		t.Fatal("compaction must re-send the host context")
	}
}

func TestToolOutputEstimateFollowsCodexTruncation(t *testing.T) {
	c := &conversation{toolOutputTokenLimit: 10000}
	big := []any{M{"type": "inputText", "text": string(make([]byte, 400000))}, M{"type": "inputImage", "imageUrl": "data:"}}
	if got := c.estimateToolOutput(big); got != 10000+estimatedImageTokens {
		t.Fatal(got)
	}
	if got := c.estimateToolOutput([]any{M{"type": "inputText", "text": "abcdefgh"}}); got != 2 {
		t.Fatal(got)
	}
}
