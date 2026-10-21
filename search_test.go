package main

import (
	"context"
	"strings"
	"testing"
)

func webSearchRequest() M {
	body := testRequest(false)
	body["system"] = "Search once, summarize the results."
	body["messages"] = []any{M{"role": "user", "content": "Perform a web search for the query: Go official website"}}
	body["tools"] = []any{M{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}}
	body["tool_choice"] = M{"type": "tool", "name": "web_search"}
	return body
}

func TestWebSearchHelperPreparationAndRestrictions(t *testing.T) {
	for _, choice := range []any{nil, M{"type": "auto"}, M{"type": "tool", "name": "web_search"}} {
		body := webSearchRequest()
		body["tool_choice"] = choice
		before := string(encode(body))
		p, err := prepare(body, "gpt-test-sol", nil)
		if err != nil || p.Search == nil || len(p.Specs) != 0 {
			t.Fatal(p, err)
		}
		if string(encode(body)) != before {
			t.Fatal("Mutated caller")
		}
	}
	for _, modify := range []func(M){
		func(b M) { b["tool_choice"] = M{"type": "none"} },
		func(b M) { object(array(b["tools"])[0])["blocked_domains"] = []any{"example.com"} },
		func(b M) { object(array(b["tools"])[0])["allowed_domains"] = "invalid" },
		func(b M) { object(array(b["tools"])[0])["allowed_domains"] = []any{12} },
		func(b M) { object(array(b["tools"])[0])["user_location"] = M{"type": "approximate"} },
		func(b M) { object(array(b["tools"])[0])["max_uses"] = 0 },
		func(b M) {
			b["tools"] = append(array(b["tools"]), M{"name": "Bash", "input_schema": M{"type": "object"}})
		},
	} {
		body := webSearchRequest()
		modify(body)
		if _, err := prepare(body, "gpt-test-sol", nil); err == nil {
			t.Fatal("Accepted invalid search request", body)
		}
	}
}

func TestWebSearchIsIsolatedAndRequiresNativeSearch(t *testing.T) {
	f := newHTTPFixture(t)
	f.configure = func(r *fakeRPC) {
		r.events <- M{"method": "item/completed", "params": M{"item": M{"type": "webSearch", "id": "search-1", "query": "Go official website"}}}
	}
	body := webSearchRequest()
	object(array(body["tools"])[0])["allowed_domains"] = []any{"go.dev"}
	status, response := f.post(t, body)
	if status != 200 || response["stop_reason"] != "end_turn" {
		t.Fatal(status, response)
	}
	calls, _ := f.rpc().snapshot()
	params := calls[0].Params
	config := object(params["config"])
	if config["web_search"] != "live" || !strings.Contains(str(params["baseInstructions"]), "native web search") || len(array(params["dynamicTools"])) != 0 {
		t.Fatal(params)
	}
	if !strings.Contains(string(encode(config)), "go.dev") {
		t.Fatal(config)
	}
	// A helper with different domain constraints must not reuse the prior thread/cache.
	f.configure = nil
	body = webSearchRequest()
	status, response = f.post(t, body)
	if status != 502 || !strings.Contains(string(encode(response)), "without executing a search") {
		t.Fatal(status, response)
	}
	ordinary, err := prepare(testRequest(false), "gpt-test-sol", nil)
	if err != nil {
		t.Fatal(err)
	}
	rpc := newFake()
	defer rpc.Close()
	if _, err = newConversation(context.Background(), rpc, ordinary, t.TempDir(), "ordinary"); err != nil {
		t.Fatal(err)
	}
	calls, _ = rpc.snapshot()
	if object(calls[0].Params["config"])["web_search"] != "disabled" || !strings.Contains(str(calls[0].Params["baseInstructions"]), "Use only the cc_") {
		t.Fatal(calls)
	}
}
