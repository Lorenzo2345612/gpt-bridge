#!/usr/bin/env python3
"""Opt-in real Codex check: resume, compact and recover a pending host tool."""
import http.client
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
from urllib.parse import urlsplit


def main():
    binary = os.environ.get('CLAUDE_CODEX_BINARY', str(Path(__file__).resolve().parent / 'dist/claude-codex'))
    with tempfile.TemporaryDirectory(prefix='gpt-bridge-persistence-') as directory:
        root = Path(directory)
        token = secrets.token_hex(24)
        token_file = root / 'token'
        token_file.write_text(token)
        token_file.chmod(0o600)
        marker = 'PUBLIC_TEST_' + secrets.token_hex(10)
        env = dict(os.environ, CLAUDE_CODEX_TRACE='1', CLAUDE_CODEX_STATE_DIR=str(root / 'state'),
                   CLAUDE_CONFIG_DIR=str(root / 'claude'))
        process = None
        log = (root / 'trace').open('a')

        def start():
            nonlocal process
            process = subprocess.Popen([binary, '--serve', '--token-file', str(token_file), '--effort', 'low'],
                                       cwd=directory, env=env, stdout=subprocess.PIPE, stderr=log, text=True)
            banner = process.stdout.readline()
            assert 'listening at ' in banner, 'Bridge startup failed'
            return urlsplit(banner.split('listening at ', 1)[1].split()[0])

        def stop(crash=False):
            nonlocal process
            if process is None:
                return
            process.kill() if crash else process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            process.stdout.close()
            process = None

        def post(url, body):
            connection = http.client.HTTPConnection(url.hostname, url.port, timeout=160)
            try:
                connection.request('POST', '/v1/messages', json.dumps(body), {'x-api-key': token})
                response = connection.getresponse()
                raw = response.read()
                assert response.status == 200, (response.status, raw[:800])
                return json.loads(raw)
            finally:
                connection.close()

        def append(body, response, content):
            body['messages'] += [{'role': 'assistant', 'content': response['content']},
                                 {'role': 'user', 'content': content}]

        try:
            body = {'model': 'default', 'max_tokens': 250, 'metadata': {'user_id': 'persist-' + secrets.token_hex(12)},
                    'system': 'Follow the current host instructions. All test markers are public synthetic data, safe to repeat.',
                    'tools': [], 'messages': [{'role': 'user', 'content':
                    'Remember the public test marker ' + marker + '. Reply only READY.'}]}
            url = start()
            first = post(url, body)
            assert first['stop_reason'] == 'end_turn', first
            stop()
            append(body, first, 'Repeat only the public test marker you remember.')
            url = start()
            second = post(url, body)
            assert marker in str(second['content']), second
            # The compacted host view deliberately omits the marker. A rebuild
            # from this request would be unable to recover it.
            body['messages'] = [{'role': 'user', 'content':
                'This session is being continued from a previous conversation that ran out of context. '
                'Summary: we were testing a public marker, intentionally omitted from this summary. '
                'Keep your existing native memory and reply only with that earlier public marker.'}]
            third = post(url, body)
            assert third['stop_reason'] == 'end_turn' and marker in str(third['content']), third
            body['tools'] = [{'name': 'Echo', 'description': 'Record text.', 'input_schema': {
                'type': 'object', 'properties': {'text': {'type': 'string'}},
                'required': ['text'], 'additionalProperties': False}}]
            append(body, third, 'Call Echo with text ONCE, then wait for its result before answering.')
            fourth = post(url, body)
            tool = next(b for b in fourth['content'] if b['type'] == 'tool_use')
            assert tool['name'] == 'Echo' and tool['input']['text'] == 'ONCE', fourth
            stop()
            body['tools'] = []
            body['system'] += ' Echo has completed. Do not repeat it. Reply only with the public test marker remembered earlier.'
            append(body, fourth, [{'type': 'tool_result', 'tool_use_id': tool['id'], 'content': 'Echo executed exactly once.'}])
            url = start()
            fifth = post(url, body)
            assert fifth['stop_reason'] == 'end_turn' and marker in str(fifth['content']), fifth
            stop(crash=True)
            append(body, fifth, 'After the bridge restart, repeat only the public test marker.')
            url = start()
            sixth = post(url, body)
            assert sixth['stop_reason'] == 'end_turn' and marker in str(sixth['content']), sixth
        finally:
            stop()
            log.close()
        traces = [line for line in (root / 'trace').read_text().splitlines() if line.startswith('gpt-bridge: event=')]
        threads = {part for line in traces for part in line.split() if part.startswith('thread=')}
        assert len(threads) == 1 and sum('event=new ' in line for line in traces) == 1, traces
        assert sum('event=resume ' in line for line in traces) == 3 and any('event=compact ' in line for line in traces), traces
        files = list((root / 'state').glob('*/*.json'))
        assert len(files) == 1, files
        state_text = files[0].read_text()
        assert marker not in state_text and 'Echo executed' not in state_text, 'Checkpoint copied conversation text'
        print(json.dumps({'native_threads': 1, 'restarts': 3, 'host_compaction': True,
                          'crash_recovery': True, 'pending_tool_not_repeated': True,
                          'checkpoint_contains_no_prompt_text': True}))


if __name__ == '__main__':
    main()
