package main

// Codex publishes thread/tokenUsage/updated for a sample only after the
// sample's stream finishes, and a sample that calls a dynamic tool finishes only
// after the tool result is returned. A tool_use response therefore can only
// carry the PREVIOUS sample's usage, and Claude's auto-compaction ran one step
// (one tool result, sometimes hundreds of thousands of tokens) behind the real
// native context. The bridge tracks what it appended since the last measured
// sample and reports measured usage plus that estimate.

// Conservative relative to what Codex counts for tool text (~6 chars/token)
// and for a 1440x900 screenshot (~550 tokens): the estimate only covers one
// step and is replaced by the real number as soon as Codex publishes it.
const estimatedCharsPerToken = 4
const estimatedImageTokens = 800

func estimateItems(items []any) int {
	tokens := 0
	for _, v := range items {
		m := object(v)
		switch str(m["type"]) {
		case "text", "inputText", "input_text":
			tokens += len(str(m["text"])) / estimatedCharsPerToken
		case "image", "inputImage", "input_image":
			tokens += estimatedImageTokens
		}
	}
	return tokens
}

// Codex truncates each tool output to the model's truncation_policy before the
// model sees it (10k tokens for every current model).
const defaultToolOutputTokenLimit = 10000

func (c *conversation) estimateToolOutput(items []any) int {
	limit := c.toolOutputTokenLimit
	if limit <= 0 {
		limit = defaultToolOutputTokenLimit
	}
	text, images := []any{}, 0
	for _, v := range items {
		if t := str(object(v)["type"]); t == "image" || t == "inputImage" || t == "input_image" {
			images++
		} else {
			text = append(text, v)
		}
	}
	return min(estimateItems(text), limit) + images*estimatedImageTokens
}

// addUnmeasured records tokens that will first be counted by the next sample to
// finish: its input (turn input, tool results, host context) or its output.
func (c *conversation) addUnmeasured(tokens int) {
	if tokens <= 0 {
		return
	}
	if c.unmeasured == nil {
		c.unmeasured = map[int]int{}
	}
	c.unmeasured[c.samplesEnded+1] += tokens
}

// sampleEnded marks the open sample as finished from the bridge's side (its
// tool calls were emitted) even though Codex has not published its usage yet.
func (c *conversation) sampleEnded() {
	c.samplesEnded = max(c.samplesEnded+1, c.samplesMeasured)
}

func (c *conversation) sampleMeasured(usage M) {
	c.usage = usage
	c.samplesMeasured = min(c.samplesMeasured+1, c.samplesEnded+1)
	for sample := range c.unmeasured {
		if sample <= c.samplesMeasured {
			delete(c.unmeasured, sample)
		}
	}
}

func (c *conversation) turnEnded() {
	c.samplesEnded = max(c.samplesEnded, c.samplesMeasured)
}

// nativeCompacted discards measurements of the replaced history. Codex re-sends
// every additional-context key after compaction, so forget what it has seen.
func (c *conversation) nativeCompacted() {
	c.usage = M{"input_tokens": 0, "output_tokens": 0}
	c.unmeasured = nil
	c.samplesMeasured = c.samplesEnded
	c.sentContext = nil
}

// contextAdditions estimates the additional-context values Codex will append:
// only keys whose value differs from the last one sent.
func (c *conversation) contextAdditions() {
	if c.sentContext == nil {
		c.sentContext = map[string]string{}
	}
	tokens := 0
	for key, v := range c.currentContext {
		value := str(object(v)["value"])
		if c.sentContext[key] != value {
			tokens += len(value) / estimatedCharsPerToken
			c.sentContext[key] = value
		}
	}
	c.addUnmeasured(tokens)
}

func (c *conversation) reportedUsage() M {
	usage := copyMap(c.usage)
	pending := 0
	for sample, tokens := range c.unmeasured {
		if sample > c.samplesMeasured {
			pending += tokens
		}
	}
	if pending > 0 {
		usage["input_tokens"] = integer(usage["input_tokens"]) + pending
	}
	return usage
}
