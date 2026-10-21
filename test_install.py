"""Installer integration tests in temporary homes; no account or Go build needed."""
import os
from pathlib import Path
import pty
import select
import subprocess
import tempfile
import time
import unittest


class InstallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='gpt bridge installer ')
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.rc = self.home/'.bashrc'
        self.rc.write_text("alias yolo='original-native-command'\n")
        self.binary = self.home/'test binary'
        self.binary.write_text('#!/usr/bin/env bash\nif [[ "${1:-}" == --bridge-version ]]; then echo test-version; exit; fi\nprintf "YOLO=%s\\nWINDOW=%s\\n" "${CLAUDE_YOLO:-}" "${CLAUDE_CODE_AUTO_COMPACT_WINDOW:-}"\nprintf "%s\\n" "$@"\n')
        self.binary.chmod(0o755)
        self.env = dict(os.environ, HOME=str(self.home), SHELL='/bin/bash', XDG_CONFIG_HOME=str(self.home/'.config'))
        self.env.pop('CLAUDE_YOLO', None)
        self.env.pop('CLAUDE_CODE_AUTO_COMPACT_WINDOW', None)
        self.args = ['bash', str(Path(__file__).with_name('install.sh')), '--prefix', str(self.home/'local prefix'), '--binary', str(self.binary)]

    def install(self, *args):
        return subprocess.run([*self.args, *args], env=self.env, input='', text=True, capture_output=True)

    def test_custom_aliases_execute_with_spaces_and_preserve_existing_alias(self):
        result = self.install('--alias', 'my-gpt', '--yolo-alias', 'my-gpt-yolo')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("alias yolo='original-native-command'", self.rc.read_text())
        def run(name):
            p = subprocess.run(['bash', '--noprofile', '--norc', '-c', 'shopt -s expand_aliases; source "$1"; eval "$2 --probe"', 'test', str(self.rc), name], env=self.env, capture_output=True, text=True)
            self.assertEqual(p.returncode, 0, p.stderr)
            return p.stdout
        normal, yolo = run('my-gpt'), run('my-gpt-yolo')
        self.assertIn('YOLO=\nWINDOW=\n--probe', normal)
        self.assertIn('YOLO=1\nWINDOW=600000\n--dangerously-skip-permissions\n--probe', yolo)
        again = self.install('--alias', 'my-gpt', '--yolo-alias', 'my-gpt-yolo')
        self.assertEqual(again.returncode, 0, again.stderr)
        self.assertEqual(self.rc.read_text().count('# gpt-bridge aliases'), 1)

    def test_conflicting_and_invalid_aliases_do_not_modify_shell(self):
        original = self.rc.read_text()
        for name in ['yolo', 'bad;touch file', 'same']:
            p = self.install('--alias', name, '--yolo-alias', 'same')
            self.assertNotEqual(p.returncode, 0)
            self.assertEqual(self.rc.read_text(), original)
        self.assertFalse((self.home/'.config/gpt-bridge/aliases.sh').exists())

    def test_unattended_no_aliases_and_skip_both(self):
        original = self.rc.read_text()
        self.assertNotEqual(self.install().returncode, 0)
        for flags in [('--no-aliases',), ('--alias', '-', '--yolo-alias', '-')]:
            p = self.install(*flags)
            self.assertEqual(p.returncode, 0, p.stderr)
            self.assertEqual(self.rc.read_text(), original)

    def test_interactive_prompts_accept_custom_names(self):
        master, slave = pty.openpty()
        process = subprocess.Popen(self.args, env=self.env, stdin=slave, stdout=slave, stderr=slave)
        os.close(slave)
        output = b''
        answered = set()
        deadline = time.monotonic()+10
        try:
            while process.poll() is None and time.monotonic()<deadline:
                ready, _, _ = select.select([master], [], [], .1)
                if ready:
                    try:
                        output += os.read(master, 65536)
                    except OSError:
                        break
                for prompt, answer in [(b'Normal alias', b'interactive-gpt\n'), (b'YOLO alias (', b'interactive-yolo\n')]:
                    if prompt in output and prompt not in answered:
                        os.write(master, answer)
                        answered.add(prompt)
            self.assertEqual(process.wait(timeout=2), 0, output.decode(errors='replace'))
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
            os.close(master)
        self.assertEqual(len(answered), 2)
        aliases = (self.home/'.config/gpt-bridge/aliases.sh').read_text()
        self.assertIn('alias interactive-gpt=', aliases)
        self.assertIn('alias interactive-yolo=', aliases)


if __name__ == '__main__':
    unittest.main()
