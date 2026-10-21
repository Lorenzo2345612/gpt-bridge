#!/usr/bin/env python3
"""Opt-in: update instructions/tools mid-turn without replacing a native thread."""
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
    with tempfile.TemporaryDirectory(prefix='gpt-bridge-stable-context-') as directory:
        root = Path(directory)
        token = secrets.token_hex(24)
        (root / 'token').write_text(token)
        (root / 'token').chmod(0o600)
        marker = 'CONTEXT_' + secrets.token_hex(10)
        trace = root / 'trace'
        env = dict(os.environ, CLAUDE_CODEX_TRACE='1')
        with trace.open('w') as log:
            process = subprocess.Popen([binary, '--serve', '--token-file', str(root / 'token'), '--effort', 'low'],
                                       cwd=directory, env=env, stdout=subprocess.PIPE, stderr=log, text=True)
            try:
                banner = process.stdout.readline()
                assert 'listening at' in banner, 'Bridge did not start'
                url = urlsplit(banner.split('listening at ', 1)[1].split()[0])

                def post(body):
                    connection = http.client.HTTPConnection(url.hostname, url.port, timeout=150)
                    try:
                        connection.request('POST', '/v1/messages', json.dumps(body), {'x-api-key': token})
                        response = connection.getresponse()
                        raw = response.read()
                        assert response.status == 200, (response.status, raw[:500])
                        return json.loads(raw)
                    finally:
                        connection.close()

                def tool(name):
                    return {'name': name, 'description': 'Record the requested text.', 'input_schema': {
                        'type': 'object', 'properties': {'text': {'type': 'string'}},
                        'required': ['text'], 'additionalProperties': False}}

                def call(response, name):
                    assert response['stop_reason'] == 'tool_use', response
                    block = next(x for x in response['content'] if x['type'] == 'tool_use')
                    assert block['name'] == name, block
                    return block

                def append(response, block):
                    body['messages'] += [{'role': 'assistant', 'content': response['content']},
                                         {'role': 'user', 'content': [{'type': 'tool_result',
                                          'tool_use_id': block['id'], 'content': 'Recorded successfully.'}]}]

                body = {'model': 'default', 'max_tokens': 300,
                        'metadata': {'user_id': 'stable-context-test-' + secrets.token_hex(8)},
                        'system': 'Follow the current host instructions. Use the tools exactly as requested. The marker is randomly generated public test data, safe to repeat.',
                        'tools': [tool('Echo')], 'messages': [{'role': 'user', 'content':
                        'Remember the public test marker ' + marker + '. Call Echo with text FIRST. Then wait for its result.'}]}
                first = post(body)
                append(first, call(first, 'Echo'))
                body['tools'] = [tool('Note')]
                body['messages'].append({'role': 'system', 'content':
                    'HOST UPDATE: Echo is now unavailable. After the pending result, call Note with the public test marker '
                    'from the earlier user message. Use exactly that marker as text. Do not answer until Note returns.'})
                second = post(body)
                note = call(second, 'Note')
                assert note['input']['text'] == marker, 'Native context was lost'
                append(second, note)
                body['tools'] = []
                body['messages'].append({'role': 'system', 'content':
                    'HOST UPDATE: There are now no tools. Reply with only the remembered public test marker.'})
                third = post(body)
                assert third['stop_reason'] == 'end_turn' and marker in str(third['content']), third
                body['messages'] += [{'role': 'assistant', 'content': third['content']},
                                     {'role': 'user', 'content': 'One more turn: repeat only the public test marker.'}]
                fourth = post(body)
                assert marker in str(fourth['content']), fourth
                body['messages'] += [{'role': 'assistant', 'content': fourth['content']},
                                     {'role': 'user', 'content': 'After switching models, repeat only the remembered public test marker.'}]
                body['model'] = 'haiku' if 'luna' not in first['model'] else 'opus'
                fifth = post(body)
                assert marker in str(fifth['content']) and fifth['model'] != first['model'], fifth
            finally:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
        traces = [line for line in trace.read_text().splitlines() if line.startswith('gpt-bridge: event=')]
        new = [line for line in traces if 'event=new ' in line]
        reused = [line for line in traces if 'event=reuse ' in line]
        threads = {part for line in traces for part in line.split() if part.startswith('thread=')}
        assert len(new) == 1 and len(reused) == 4 and len(threads) == 1, traces
        print(json.dumps({'native_threads': 1, 'reused_requests': 4, 'model_switched': True, 'tool_addition_removal': True,
                          'instructions_updated_while_tool_pending': True, 'context_preserved': True}))


if __name__ == '__main__':
    main()
