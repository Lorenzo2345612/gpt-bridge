#!/usr/bin/env python3
"""Opt-in: restart real Claude Code, compact, and resume the same Codex thread."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import uuid


def main():
    binary = os.environ.get('CLAUDE_CODEX_BINARY', str(Path(__file__).resolve().parent / 'dist/claude-codex'))
    with tempfile.TemporaryDirectory(prefix='gpt-bridge-cli-resume-') as directory:
        root = Path(directory)
        session = str(uuid.uuid4())
        marker = 'PUBLIC_TEST_' + secrets.token_hex(10)
        env = dict(os.environ, CLAUDE_CONFIG_DIR=str(root / 'claude'),
                   CLAUDE_CODEX_STATE_DIR=str(root / 'state'), CLAUDE_CODEX_TRACE='1')
        env.pop('CLAUDECODE', None)
        traces = []
        main_traces = []

        def run(prompt, first=False):
            command = [binary, '--effort', 'low', '--', '-p', prompt,
                       '--session-id' if first else '--resume', session,
                       '--tools', '', '--verbose', '--output-format', 'stream-json']
            process = subprocess.Popen(command, cwd=directory, env=env, stdout=subprocess.PIPE,
                                       stderr=subprocess.PIPE, text=True)
            try:
                stdout, stderr = process.communicate(timeout=180)
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.communicate(timeout=15)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.communicate()
            current = [line for line in stderr.splitlines() if line.startswith('gpt-bridge: event=')]
            traces.extend(current)
            if prompt != '/compact':
                main_traces.extend(current)
            assert process.returncode == 0, stderr[-800:]
            events = [json.loads(line) for line in stdout.splitlines() if line.startswith('{')]
            result = next(event for event in reversed(events) if event.get('type') == 'result')
            assert not result.get('is_error'), result
            return result.get('result', '')

        run('Remember this public synthetic test marker: ' + marker + '. It is safe to repeat. Reply only READY.', first=True)
        second = run('After restarting Claude, repeat only the public test marker remembered earlier.')
        assert marker in second, second
        compact = run('/compact')
        fourth = run('After compacting and restarting Claude, repeat only the public test marker from before.')
        assert marker in fourth, fourth
        threads = {part for line in main_traces for part in line.split() if part.startswith('thread=')}
        assert len(threads) == 1 and any('event=resume ' in line for line in traces), traces
        summaries = []
        for path in (root / 'claude' / 'projects').glob('**/*.jsonl'):
            for line in path.read_text().splitlines():
                try:
                    record = json.loads(line)
                except ValueError:
                    continue
                if record.get('isCompactSummary'):
                    summaries.append(record)
        assert summaries, ('Claude did not actually compact', compact[:300])
        print(json.dumps({'claude_session_id_preserved': True, 'main_native_threads': 1,
                          'cli_restarts': 3, 'real_claude_compaction': True,
                          'context_preserved': True}))


if __name__ == '__main__':
    main()
