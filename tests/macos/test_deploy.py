import importlib.util
import json
from pathlib import Path
import plistlib
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch, MagicMock

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('manage', ROOT / 'deploy/macos/manage.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
spec = importlib.util.spec_from_file_location('runtime', ROOT / 'deploy/macos/runtime.py')
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class DeployTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name).resolve()
        self.release = self.home / '.local/lib/ink-commons/releases' / ('a' * 40)
        self.release.mkdir(parents=True)
        self.data = self.home / '.local/state/ink-commons'
        self.data.mkdir(parents=True, mode=0o700)
        (self.data / 'state.json').write_text('{"work":{},"pushes":[]}')
        self.old_state = (self.data / 'state.json').read_bytes()
        self.settings = {'home': str(self.home), 'data': str(self.data),
                         'python': sys.executable, 'revision': 'a' * 40}
        (self.release / 'deployment.json').write_text(json.dumps(self.settings))
        (self.release / 'ink').write_text('new')
        (self.release / 'config.json').write_text(json.dumps({
            'github_command': sys.executable, 'cc_connect_url': 'http://127.0.0.1:9111/hook',
            'repositories': [{'repo': 'acme/project'}]}))
        self.link = self.home / '.local/lib/ink-commons/ink'
        self.link.write_text('old')
        self.agents = self.home / 'Library/LaunchAgents'
        self.agents.mkdir(parents=True)
        self.old_plist = plistlib.dumps({'Label': m.LABEL, 'ProgramArguments': ['old']})
        (self.agents / (m.LABEL + '.plist')).write_bytes(self.old_plist)
        cc = {'Label': m.CC_LABEL, 'ProgramArguments': [sys.executable],
              'EnvironmentVariables': {'PATH': '/usr/bin:/bin'}}
        (self.agents / (m.CC_LABEL + '.plist')).write_bytes(plistlib.dumps(cc))
        self.system = self.home / 'system'
        self.system.mkdir()
        self.jobs = {('gui/501', m.LABEL), ('gui/501', m.CC_LABEL)}
        self.account = SimpleNamespace(pw_uid=501, pw_gid=20, pw_dir=str(self.home))
        self.calls = []
        self.fail = False

    def run_fake(self, args, **kwargs):
        args = [str(x) for x in args]
        self.calls.append(args)
        if args[:2] == ['/bin/launchctl', 'bootout']:
            domain, label = args[2].rsplit('/', 1)
            self.jobs.discard((domain, label))
        if args[:2] == ['/bin/launchctl', 'bootstrap']:
            domain, path = args[2:]
            label = plistlib.loads(Path(path).read_bytes())['Label']
            self.jobs.add((domain, label))
            if label == m.LABEL and domain == 'system':
                # 模拟新版迁移，只能在 gate 未打开的条件下启动。
                self.assertFalse((self.release / 'enabled').exists())
                (self.data / 'state.sqlite3').write_bytes(b'new database')
        if args[0] != '/bin/launchctl' and self.fail:
            raise RuntimeError('App unavailable')
        return SimpleNamespace(returncode=0)

    def activate(self):
        args = SimpleNamespace(user='service', release=str(self.release), system=True,
                               cc_connect_agent=str(self.agents / (m.CC_LABEL + '.plist')))
        with patch.object(m, 'SYSTEM_DIR', self.system), \
             patch.object(m.pwd, 'getpwnam', return_value=self.account), \
             patch.object(m.os, 'geteuid', return_value=0), \
             patch.object(m.os, 'chown'), patch.object(m.os, 'lchown'), \
             patch.object(m.os, 'fchown'), \
             patch.object(m, 'loaded', side_effect=lambda d,l: (d,l) in self.jobs), \
             patch.object(m, 'run', side_effect=self.run_fake), \
             patch.object(m, 'validate_state_directory'), \
             patch.object(m, 'health', return_value={'revision': 'a'*40}), \
             patch.object(m.socket, 'create_connection', return_value=MagicMock()):
            return m.activate(args)

    def test_system_migration_and_version_gate(self):
        self.activate()
        self.assertFalse((self.agents / (m.LABEL + '.plist')).exists())
        self.assertFalse((self.agents / (m.CC_LABEL + '.plist')).exists())
        self.assertTrue((self.release / 'enabled').exists())
        self.assertEqual(self.link.resolve(), self.release / 'ink')
        ink = plistlib.loads((self.system / (m.LABEL + '.plist')).read_bytes())
        self.assertEqual(ink['UserName'], 'service')
        self.assertTrue(ink['KeepAlive'])
        self.assertEqual(ink['EnvironmentVariables']['HOME'], str(self.home))
        self.assertEqual({label for domain,label in self.jobs if domain=='system'}, {m.LABEL,m.CC_LABEL})

    def test_failed_app_check_restores_state_jobs_and_binary(self):
        self.fail = True
        with self.assertRaises(RuntimeError):
            self.activate()
        self.assertFalse((self.release / 'enabled').exists())
        self.assertFalse((self.data / 'state.sqlite3').exists())
        self.assertEqual((self.data / 'state.json').read_bytes(), self.old_state)
        self.assertEqual((self.agents / (m.LABEL + '.plist')).read_bytes(), self.old_plist)
        self.assertEqual(self.link.read_text(), 'old')
        self.assertFalse(any(domain=='system' for domain,label in self.jobs))
        self.assertEqual(len(self.jobs), 2)

    def test_runtime_credentials_never_inherit_personal_tokens(self):
        cc = self.home / 'config.toml'
        cc.write_text('[webhook]\ntoken="webhook"\n[[projects]]\nname="review"\n'
                      '[[projects.platforms]]\ntype="discord"\n[projects.platforms.options]\ntoken="discord"\n')
        with patch.dict('os.environ', {'GH_TOKEN':'personal','GITHUB_TOKEN':'personal','INK_GITHUB_TOKEN':'old'}):
            env = runtime.environment(cc, 'review')
        self.assertNotIn('GH_TOKEN', env)
        self.assertNotIn('INK_GITHUB_TOKEN', env)
        self.assertEqual(env['INK_CC_CONNECT_TOKEN'], 'webhook')
        self.assertEqual(env['INK_DISCORD_TOKEN'], 'discord')


if __name__ == '__main__':
    unittest.main()
