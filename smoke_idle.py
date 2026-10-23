#!/usr/bin/env python3
"""Opt-in real Codex check: idle executor exits, resumes same thread, preserves pending tools."""
import http.client
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time
from urllib.parse import urlsplit


def main():
    binary = os.environ.get('CLAUDE_CODEX_BINARY', str(Path(__file__).resolve().parent / 'dist/claude-codex'))
    with tempfile.TemporaryDirectory(prefix='bridge-idle-smoke-') as folder:
        root = Path(folder)
        token = secrets.token_hex(24)
        token_file = root / 'token'
        token_file.write_text(token)
        token_file.chmod(0o600)
        env = dict(os.environ, CLAUDE_CODEX_TRACE='1', CLAUDE_CODEX_STATE_DIR=str(root / 'state'),
                   CLAUDE_CONFIG_DIR=str(root / 'claude'))
        marker = 'PUBLIC_IDLE_' + secrets.token_hex(8)
        with (root / 'trace').open('w') as log:
            p = subprocess.Popen([binary, '--serve', '--token-file', str(token_file), '--idle-timeout', '1',
                                  '--effort', 'low', '--timeout', '180'], cwd=root, env=env,
                                 stdout=subprocess.PIPE, stderr=log, text=True)
            try:
                banner = p.stdout.readline()
                assert 'listening at ' in banner, 'Bridge startup failed'
                url = urlsplit(banner.split('listening at ', 1)[1].split()[0])

                def post(body):
                    c = http.client.HTTPConnection(url.hostname, url.port, timeout=190)
                    try:
                        c.request('POST', '/v1/messages', json.dumps(body), {'x-api-key': token})
                        r = c.getresponse()
                        data = r.read()
                        assert r.status == 200, (r.status, data[:600])
                        return json.loads(data)
                    finally:
                        c.close()

                def checkpoint():
                    files = list((root / 'state').glob('*/*.json'))
                    assert len(files) == 1, files
                    return json.loads(files[0].read_text())

                def append(body, response, content):
                    body['messages'] += [{'role': 'assistant', 'content': response['content']},
                                         {'role': 'user', 'content': content}]

                body = {'model': 'default', 'max_tokens': 150, 'metadata': {'user_id': 'idle-' + secrets.token_hex(10)},
                        'system': 'Follow these synthetic test instructions exactly. All markers are public.',
                        'tools': [], 'messages': [{'role': 'user', 'content': 'Remember '+marker+'. Reply only READY.'}]}
                first = post(body)
                original = checkpoint()['thread_id']
                deadline = time.monotonic() + 20
                while time.monotonic() < deadline and 'event=unload ' not in (root / 'trace').read_text():
                    time.sleep(.2)
                assert 'event=unload ' in (root / 'trace').read_text(), 'Idle executor was retained'
                time.sleep(1)  # allow process-group shutdown and lease release
                assert checkpoint()['thread_id'] == original, 'Lost native checkpoint'
                children = Path(f'/proc/{p.pid}/task/{p.pid}/children')
                if children.exists():
                    assert not children.read_text().strip(), 'Native executor processes survived idle shutdown'
                append(body, first, 'Repeat only the public marker remembered earlier.')
                second = post(body)
                assert marker in str(second['content']), second
                assert checkpoint()['thread_id'] == original, 'Idle resume replaced native thread'
                body['tools'] = [{'name': 'Echo', 'description': 'Echo public text.', 'input_schema': {
                    'type': 'object', 'properties': {'text': {'type': 'string'}},
                    'required': ['text'], 'additionalProperties': False}}]
                append(body, second, 'Call Echo with text ONCE, then wait for its result.')
                third = post(body)
                tool = next(b for b in third['content'] if b['type'] == 'tool_use')
                assert tool['name'] == 'Echo' and tool['input']['text'] == 'ONCE', third
                counter = root / 'host-tool-executions'
                external = subprocess.Popen([sys.executable, '-c',
                    'import sys,time; from pathlib import Path; time.sleep(2); '
                    'Path(sys.argv[1]).write_text("ONCE\\n"); print("ONCE")',
                    str(counter)], stdout=subprocess.PIPE, text=True)
                before = (root / 'trace').read_text().count('event=unload ')
                time.sleep(3)
                assert (root / 'trace').read_text().count('event=unload ') > before, 'Pending host tool retained idle executor'
                assert external.wait(timeout=5) == 0, 'External Claude tool was interrupted'
                tool_result = external.stdout.read().strip()
                external.stdout.close()
                assert counter.read_text().strip() == 'ONCE' and tool_result == 'ONCE', 'Host tool failed or repeated'
                retry = post(body)
                assert retry['content'] == third['content'], 'Retry repeated or changed host tool invocation'
                append(body, third, [{'type': 'tool_result', 'tool_use_id': tool['id'], 'content': tool_result}])
                body['tools'] = []
                last = post(body)
                assert last['stop_reason'] == 'end_turn', last
                assert checkpoint()['thread_id'] == original, 'Pending-tool owner replaced'
                trace = (root / 'trace').read_text()
                assert trace.count('event=new ') == 1 and 'event=resume ' in trace, trace
                print(json.dumps({'idle_executor_exited': True, 'native_thread_preserved': True,
                                  'context_preserved': True, 'pending_tool_preserved': True, 'pending_executor_exited': True,
                                  'retry_preserved_tool_ids': True,
                                  'pending_result_delivered_once': True, 'external_host_tool_completed': True}))
            finally:
                p.terminate()
                try:
                    p.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    p.kill()
                    p.wait()
                p.stdout.close()


if __name__ == '__main__':
    main()
