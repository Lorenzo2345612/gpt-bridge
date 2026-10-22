#!/usr/bin/env python3
"""Opt-in: resume real Claude through the bridge with cw-style hook settings."""
import json
import os
from pathlib import Path
import secrets
import shlex
import subprocess
import sys
import tempfile
import uuid


def main():
    binary = os.environ.get('CLAUDE_CODEX_BINARY', str(Path(__file__).resolve().parent / 'dist/claude-codex'))
    with tempfile.TemporaryDirectory(prefix='gpt-bridge-workspace-') as directory:
        root = Path(directory)
        hook = root / 'hook.py'
        hook.write_text('import json,sys\nfrom pathlib import Path\np=json.load(sys.stdin)\n'
                        'with Path(sys.argv[1]).open("a") as f: f.write(p.get("hook_event_name", "")+"\\n")\n')
        events = root / 'hook-events'
        settings = json.dumps({'hooks': {'SessionStart': [{'hooks': [{'type': 'command',
            'command': shlex.quote(sys.executable)+' '+shlex.quote(str(hook))+' '+shlex.quote(str(events))}]}]}})
        env = dict(os.environ, CLAUDE_CONFIG_DIR=str(root / 'claude'),
                   CLAUDE_CODEX_STATE_DIR=str(root / 'state'))
        env.pop('CLAUDECODE', None)
        session = str(uuid.uuid4())
        marker = 'PUBLIC_WORKSPACE_' + secrets.token_hex(8)
        for first in (True, False):
            prompt = ('Remember this public synthetic marker: '+marker+'. Reply only READY.' if first
                      else 'Repeat only the public synthetic marker remembered earlier.')
            cmd = [binary, '--effort', 'low', '--', '-p', prompt,
                   '--session-id' if first else '--resume', session,
                   '--tools', '', '--settings', settings, '--verbose', '--output-format', 'stream-json']
            result = subprocess.run(cmd, cwd=root, env=env, capture_output=True, text=True, timeout=180)
            assert result.returncode == 0, result.stderr[-800:]
            assert 'could not be restored' not in result.stderr+result.stdout, 'Lost model picker on resume'
            records = [json.loads(line) for line in result.stdout.splitlines() if line.startswith('{')]
            final = next(r for r in reversed(records) if r.get('type') == 'result')
            assert not final.get('is_error'), final
            if not first:
                assert marker in final.get('result', ''), final
                models = {r.get('message', {}).get('model') for r in records if r.get('type') == 'assistant'}
                assert any(m and m.startswith('gpt-') for m in models), models
        assert events.read_text().count('SessionStart') >= 2, 'Workspace hooks were lost'
        print(json.dumps({'workspace_hooks_preserved': True, 'gpt_model_restored': True,
                          'resume_context_preserved': True}))


if __name__ == '__main__':
    main()
