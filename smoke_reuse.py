#!/usr/bin/env python3
"""Opt-in real-Claude test: three dependent Reads must reuse one Codex thread."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile


def main():
    binary = os.environ.get('CLAUDE_CODEX_BINARY', str(Path(__file__).resolve().parent / 'dist/claude-codex'))
    with tempfile.TemporaryDirectory(prefix='gpt-bridge-reuse-') as directory:
        root = Path(directory)
        config = root / 'config'
        config.mkdir()
        marker = 'REUSE_' + secrets.token_hex(12)
        first, second, third = [root / (secrets.token_hex(6) + '.txt') for _ in range(3)]
        first.write_text('Read the next file with Read: ' + str(second))
        second.write_text('Read the final file with Read: ' + str(third))
        third.write_text('The requested marker is ' + marker)
        env = dict(os.environ, CLAUDE_CONFIG_DIR=str(config), CLAUDE_CODEX_TRACE='1')
        env.pop('CLAUDECODE', None)
        command = [binary, '--effort', 'low', '--', '-p',
                   'Use Read to read ' + str(first) + '. Follow the file chain using Read; '
                   'the last file contains a marker. Reply only with that marker. Use no other tools.',
                   '--tools', 'Read', '--allowedTools', 'Read', '--verbose',
                   '--output-format', 'stream-json', '--no-session-persistence']
        process = subprocess.Popen(command, cwd=directory, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
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
        assert process.returncode == 0, 'Bridge execution failed'
        events = [json.loads(line) for line in stdout.splitlines() if line.startswith('{')]
        final = next(event for event in reversed(events) if event.get('type') == 'result')
        assert marker in final.get('result', '') and not final.get('is_error'), 'Context/marker was lost'
        traces = [line for line in stderr.splitlines() if line.startswith('gpt-bridge: event=')]
        new = [line for line in traces if 'event=new ' in line]
        reused = [line for line in traces if 'event=reuse ' in line]
        threads = {part for line in traces for part in line.split() if part.startswith('thread=')}
        hosts = {part for line in traces for part in line.split() if part.startswith('host=')}
        assert len(new) == 1 and len(reused) >= 3 and len(threads) == 1 and len(hosts) == 1 and "host=" not in hosts, traces
        print(json.dumps({'new_conversations': len(new), 'reused_requests': len(reused),
                          'native_threads': len(threads), 'host_identities': len(hosts),
                          'dependent_reads': 3, 'context_preserved': True}))


if __name__ == '__main__':
    main()
