#!/usr/bin/env python3
"""Opt-in AskUserQuestion transport test, including a delayed human UI answer."""
import argparse
import http.client
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
from urllib.parse import urlsplit
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--delay', type=float, default=2)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    binary = os.environ.get('CLAUDE_CODEX_BINARY', str(root/'dist/claude-codex'))
    token = 'question-smoke-local-token-123456789'
    with tempfile.TemporaryDirectory(prefix='gpt-bridge-question-') as directory:
        path = Path(directory)/'token'
        path.write_text(token)
        path.chmod(0o600)
        p = subprocess.Popen([binary, '--serve', '--token-file', str(path), '--effort', 'low'],
                             cwd=directory, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            banner = p.stdout.readline()
            assert 'listening at ' in banner, (banner, p.stderr.read()[-500:])
            url = urlsplit(banner.split('listening at ', 1)[1].split()[0])

            def post(body):
                c = http.client.HTTPConnection(url.hostname, url.port, timeout=190)
                c.request('POST', '/v1/messages', json.dumps(body), {'x-api-key': token})
                response = c.getresponse()
                data = json.loads(response.read())
                c.close()
                assert response.status == 200, (response.status, data)
                return data

            for cancelled in [False, True]:
                body = {'model': 'default', 'max_tokens': 200,
                        'system': 'Ask exactly once using AskUserQuestion. Wait for the human reply. If answered, reply only with the code word. If cancelled, reply only CANCELLED and take no further action.',
                        'tools': [{'name': 'AskUserQuestion', 'description': 'Ask the human a question; the human may take minutes to answer. The result is the human reply.',
                                   'input_schema': {'type': 'object', 'properties': {'question': {'type': 'string'}},
                                                    'required': ['question'], 'additionalProperties': False}}],
                        'messages': [{'role': 'user', 'content': 'Use AskUserQuestion to ask me for a code word. Wait for its result before responding.'}]}
                first = post(body)
                assert first['stop_reason'] == 'tool_use', first
                tool = next(x for x in first['content'] if x['type'] == 'tool_use')
                assert tool['name'] == 'AskUserQuestion', tool
                time.sleep(0 if cancelled else args.delay)
                # Generate only AFTER the question; the model cannot precompute it.
                word = 'HUMAN_'+uuid.uuid4().hex[:12]
                answer = ('The user cancelled the question; do not proceed.' if cancelled else
                          f'Your questions have been answered: "Code word?"="{word}". You can now continue with these answers in mind.')
                body['messages'] += [{'role': 'assistant', 'content': first['content']},
                                     {'role': 'user', 'content': [{'type': 'tool_result', 'tool_use_id': tool['id'],
                                                                'content': answer, 'is_error': cancelled}]}]
                last = post(body)
                text = ''.join(x.get('text', '') for x in last['content']).strip()
                expected = 'CANCELLED' if cancelled else word
                assert last['stop_reason'] == 'end_turn' and text == expected, last
                print(json.dumps({'ok': True, 'case': 'cancellation' if cancelled else 'human_answer',
                                  'delay_seconds': 0 if cancelled else args.delay, 'reply_matches': True}), flush=True)
        finally:
            p.terminate()
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()


if __name__ == '__main__':
    main()
