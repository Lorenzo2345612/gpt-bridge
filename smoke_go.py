#!/usr/bin/env python3
"""Opt-in tests against the compiled Go runtime (Python is only the test client)."""
import http.client
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
from urllib.parse import urlsplit


def main():
    root = Path(__file__).resolve().parent
    binary = Path(os.environ.get("CLAUDE_CODEX_BINARY", root / "dist/claude-codex"))
    token = "go-live-test-local-token-123456789"
    with tempfile.TemporaryDirectory(prefix="claude-codex-go-smoke-") as directory:
        token_file = Path(directory) / "token"
        token_file.write_text(token)
        token_file.chmod(0o600)
        process = subprocess.Popen([str(binary), "--serve", "--token-file", str(token_file),
                                    "--effort", "low", "--max-sessions", "4"],
                                   cwd=directory, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            banner = process.stdout.readline()
            assert "listening at" in banner, (banner, process.stderr.read()[-1000:])
            url = urlsplit(banner.split("listening at ", 1)[1].split()[0])

            def post(body):
                connection = http.client.HTTPConnection(url.hostname, url.port, timeout=190)
                start = time.perf_counter()
                connection.request("POST", "/v1/messages", json.dumps(body), {"x-api-key": token})
                response = connection.getresponse()
                raw = response.read()
                connection.close()
                assert response.status == 200, (response.status, raw[:1000])
                return json.loads(raw), round((time.perf_counter()-start)*1000)

            body = {"model": "default", "max_tokens": 100,
                    "system": "Follow the user's tool and test instructions precisely.",
                    "tools": [{"name": "Echo", "description": "Return text unchanged.",
                               "input_schema": {"type": "object", "properties": {"text": {"type": "string"}},
                                                "required": ["text"], "additionalProperties": False}}],
                    "messages": [{"role": "user", "content": "Call Echo with GO_TOOL_OK, then reply only with its exact result."}]}
            first, first_ms = post(body)
            assert first["stop_reason"] == "tool_use", first
            tool = next(x for x in first["content"] if x["type"] == "tool_use")
            assert tool["name"] == "Echo" and tool["input"] == {"text": "GO_TOOL_OK"}, tool
            body["messages"] += [{"role": "assistant", "content": first["content"]},
                                 {"role": "user", "content": [{"type": "tool_result", "tool_use_id": tool["id"],
                                                               "content": "GO_TOOL_OK"}]}]
            last, last_ms = post(body)
            assert last["stop_reason"] == "end_turn" and "GO_TOOL_OK" in "".join(x.get("text", "") for x in last["content"]), last
            oversized = {"model": first["model"], "max_tokens": 80, "tools": [],
                         "messages": [{"role": "user", "content": "ALPHA_HISTORY\n" + "x"*1100000 + "\nOMEGA_HISTORY"},
                                      {"role": "assistant", "content": "Reference received."},
                                      {"role": "user", "content": "Reply with exactly the two reference markers, separated by one space."}]}
            large, large_ms = post(oversized)
            text = "".join(x.get("text", "") for x in large["content"])
            assert "ALPHA_HISTORY OMEGA_HISTORY" in text, large
            rss = None
            for line in Path(f"/proc/{process.pid}/status").read_text().splitlines():
                if line.startswith("VmRSS:"):
                    rss = round(int(line.split()[1])/1024, 1)
            report = {"runtime": "go", "model": first["model"], "tool_roundtrip": True,
                      "oversized_history_chars": 1100000, "oversized_history_intact": True,
                      "tool_request_ms": first_ms, "tool_result_ms": last_ms, "oversized_request_ms": large_ms,
                      "bridge_rss_after_large_history_mib": rss, "usage": large["usage"]}
            (root / "go-live-protocol-result.json").write_text(json.dumps(report, indent=2)+"\n")
            print(json.dumps(report))
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    main()
