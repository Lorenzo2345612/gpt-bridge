#!/usr/bin/env python3
"""Opt-in end-to-end WebSearch test through the real Claude and Codex CLIs."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time


def main():
    root = Path(__file__).resolve().parent
    binary = os.environ.get('CLAUDE_CODEX_BINARY', str(root / 'dist/claude-codex'))
    with tempfile.TemporaryDirectory(prefix='gpt-bridge-websearch-smoke-') as directory:
        config = Path(directory) / 'claude-config'
        config.mkdir()
        env = dict(os.environ, CLAUDE_CONFIG_DIR=str(config))
        env.pop('CLAUDECODE', None)
        command = [binary, '--effort', 'low', '--', '-p',
                   'Use WebSearch to search for the official Go programming language website. '
                   'Set allowed_domains to ["go.dev"]. Return the source URL from the results. '
                   'Use no other tools. If the search fails, report the error.',
                   '--tools', 'WebSearch', '--allowedTools', 'WebSearch',
                   '--output-format', 'stream-json', '--verbose', '--no-session-persistence']
        start = time.monotonic()
        run = subprocess.run(command, cwd=directory, env=env, capture_output=True, text=True, timeout=180)
        assert run.returncode == 0, run.stderr[-1000:]
        events = [json.loads(line) for line in run.stdout.splitlines() if line.startswith('{')]
        calls = []
        results = []
        for event in events:
            content = event.get('message', {}).get('content', [])
            if not isinstance(content, list):
                continue
            for block in content:
                if block.get('type') == 'tool_use' and block.get('name') == 'WebSearch':
                    calls.append(block)
                if block.get('type') == 'tool_result':
                    results.append(block)
        assert calls and any('go.dev' in c['input'].get('allowed_domains', []) for c in calls), 'No domain-filtered WebSearch call'
        ids = {call['id'] for call in calls}
        results = [result for result in results if result.get('tool_use_id') in ids]
        assert results and not any(result.get('is_error') for result in results), results
        assert any('https://go.dev' in str(result.get('content', '')) for result in results), results
        final = next(event for event in reversed(events) if event.get('type') == 'result')
        assert not final.get('is_error') and 'https://go.dev' in final.get('result', ''), final
        print(json.dumps({'web_search': True, 'allowed_domains': ['go.dev'],
                          'claude_tool_result': 'ok', 'native_search_required': True,
                          'seconds': round(time.monotonic() - start, 1)}))


if __name__ == '__main__':
    main()
