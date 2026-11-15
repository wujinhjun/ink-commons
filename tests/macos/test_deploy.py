import importlib.util
import hashlib
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
        (self.release / 'cc-connect').write_bytes(b'pinned cc-connect')
        self.settings['cc_binary_sha256'] = hashlib.sha256(b'pinned cc-connect').hexdigest()
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
        (self.agents / (m.LABEL + '.plist')).chmod(0o600)
        (self.agents / (m.WATCHDOG + '.plist')).write_bytes(plistlib.dumps({'Label':m.WATCHDOG,'ProgramArguments':['watchdog']}))
        cc = {'Label': m.CC_LABEL, 'ProgramArguments': [sys.executable],
              'EnvironmentVariables': {'PATH': '/usr/bin:/bin'}}
        (self.agents / (m.CC_LABEL + '.plist')).write_bytes(plistlib.dumps(cc))
        self.system = self.home / 'system'
        self.system.mkdir()
        self.jobs = {('gui/501', m.LABEL), ('gui/501', m.CC_LABEL), ('gui/501',m.WATCHDOG)}
        self.account = SimpleNamespace(pw_uid=501, pw_gid=20, pw_dir=str(self.home))
        self.calls = []
        self.fail = False
        self.fail_bootstrap = False
        self.fail_stop = False
        self.transient_failures = 0
        self.self_checks = 0

    def run_fake(self, args, **kwargs):
        args = [str(x) for x in args]
        self.calls.append(args)
        if args[:2] == ['/bin/launchctl', 'bootout']:
            domain, label = args[2].rsplit('/', 1)
            if self.fail_stop and domain == 'system' and label == m.LABEL:
                raise RuntimeError('new daemon cannot be stopped')
            self.jobs.discard((domain, label))
        if args[:2] == ['/bin/launchctl', 'bootstrap']:
            domain, path = args[2:]
            label = plistlib.loads(Path(path).read_bytes())['Label']
            self.jobs.add((domain, label))
            if label == m.LABEL and domain == 'system':
                # 模拟新版迁移，只能在 gate 未打开的条件下启动。
                self.assertFalse((self.release / 'enabled').exists())
                (self.data / 'state.sqlite3').write_bytes(b'new database')
                if self.fail_bootstrap:
                    raise RuntimeError('bootstrap partially succeeded')
        if args[0] != '/bin/launchctl' and self.fail:
            raise RuntimeError('App unavailable')
        return SimpleNamespace(returncode=0)

    def activate(self, migrating=True):
        args = SimpleNamespace(user='service', release=str(self.release), system=True,
                               cc_connect_agent=str(self.agents / (m.CC_LABEL + '.plist')) if migrating else None)
        with patch.object(m, 'SYSTEM_DIR', self.system), \
             patch.object(m.pwd, 'getpwnam', return_value=self.account), \
             patch.object(m.os, 'geteuid', return_value=0), \
             patch.object(m.os, 'chown'), patch.object(m.os, 'lchown'), \
             patch.object(m.os, 'fchown'), \
             patch.object(m, 'loaded', side_effect=lambda d,l: (d,l) in self.jobs), \
             patch.object(m, 'run', side_effect=self.run_fake), \
             patch.object(m, 'validate_state_directory'), \
             patch.object(m, 'disabled', return_value=False), \
             patch.object(m, 'cc_listener_healthy', return_value=True), \
             patch.object(m, 'local_get', side_effect=self.self_check), \
             patch.object(m.time, 'sleep'), \
             patch.object(m, 'health', return_value={'revision': 'a'*40,'gate':'closed'}), \
             patch.object(m.socket, 'create_connection', return_value=MagicMock()):
            return m.activate(args)

    def self_check(self,*args,**kwargs):
        self.self_checks += 1
        self.assertFalse((self.release/'enabled').exists())
        if self.self_checks <= self.transient_failures:
            raise m.DeploymentError('temporary upstream failure')
        if self.fail:
            raise m.DeploymentError('daemon App command exited (status 1)')
        return {'status':'ok'}

    def test_system_migration_and_version_gate(self):
        self.activate()
        self.assertFalse((self.agents / (m.LABEL + '.plist')).exists())
        self.assertFalse((self.agents / (m.CC_LABEL + '.plist')).exists())
        self.assertFalse((self.agents / (m.WATCHDOG+'.plist')).exists())
        stops=[a[2] for a in self.calls if a[:2]==['/bin/launchctl','bootout']]
        self.assertLess(stops.index('gui/501/'+m.WATCHDOG),stops.index('gui/501/'+m.CC_LABEL))
        self.assertTrue((self.release / 'enabled').exists())
        self.assertEqual(self.link.resolve(), self.release / 'ink')
        ink = plistlib.loads((self.system / (m.LABEL + '.plist')).read_bytes())
        self.assertEqual(ink['UserName'], 'service')
        cc=plistlib.loads((self.system / (m.CC_LABEL+'.plist')).read_bytes())
        self.assertEqual(cc['ProgramArguments'][0],str(self.release/'cc-connect'))
        self.assertTrue(ink['KeepAlive'])
        self.assertEqual(ink['EnvironmentVariables']['HOME'], str(self.home))
        self.assertEqual({label for domain,label in self.jobs if domain=='system'}, {m.LABEL,m.CC_LABEL})

    def test_failed_app_check_restores_state_jobs_and_binary(self):
        self.fail = True
        with self.assertRaises(m.DeploymentError):
            self.activate()
        self.assertFalse((self.release / 'enabled').exists())
        self.assertFalse((self.data / 'state.sqlite3').exists())
        self.assertEqual((self.data / 'state.json').read_bytes(), self.old_state)
        self.assertEqual((self.agents / (m.LABEL + '.plist')).read_bytes(), self.old_plist)
        self.assertEqual(self.link.read_text(), 'old')
        self.assertFalse(any(domain=='system' for domain,label in self.jobs))
        self.assertEqual(len(self.jobs), 3)
        self.assertEqual((self.agents / (m.LABEL+'.plist')).stat().st_mode & 0o777,0o600)

    def test_partial_bootstrap_is_unloaded_before_rollback(self):
        self.fail_bootstrap = True
        with self.assertRaises(RuntimeError):
            self.activate()
        self.assertFalse(any(domain == 'system' for domain, label in self.jobs))
        self.assertFalse((self.data / 'state.sqlite3').exists())
        self.assertEqual(self.link.read_text(), 'old')

    def test_cannot_stop_new_daemon_never_overwrites_its_database(self):
        self.fail = True
        self.fail_stop = True
        with self.assertRaises(m.DeploymentError):
            self.activate()
        self.assertEqual((self.data / 'state.sqlite3').read_bytes(), b'new database')
        self.assertFalse((self.release / 'enabled').exists())
        self.assertIn(('system', m.LABEL), self.jobs)

    def test_private_state_permissions_are_required(self):
        m.validate_state_directory(self.data, self.home, self.data.stat().st_uid)
        self.data.chmod(0o755)
        with self.assertRaises(m.DeploymentError):
            m.validate_state_directory(self.data, self.home, self.data.stat().st_uid)

    def test_rollback_preserves_original_entrypoint_symlink(self):
        target=self.link.with_name('old-binary')
        self.link.rename(target)
        self.link.symlink_to(target)
        self.fail=True
        with self.assertRaises(m.DeploymentError):
            self.activate()
        self.assertTrue(self.link.is_symlink())
        self.assertEqual(self.link.readlink(),target)

    def test_subsequent_system_update_reuses_installed_cc_config(self):
        self.activate()
        self.activate(migrating=False)
        self.assertTrue((self.release/'enabled').exists())

    def test_only_non_secret_proxy_environment_is_allowed(self):
        env=m.parse_env(['HTTPS_PROXY=http://127.0.0.1:7897','NODE_OPTIONS=--use-env-proxy'])
        settings={**self.settings,'environment':env}
        self.assertEqual(m.ink_plist(self.release,settings,'service',True)['EnvironmentVariables']['HTTPS_PROXY'],env['HTTPS_PROXY'])
        for entry in ('GH_TOKEN=secret','HTTPS_PROXY=http://user:secret@localhost:7897','NODE_OPTIONS=--require=/tmp/code.js'):
            with self.assertRaises(m.DeploymentError):
                m.parse_env([entry])

    def test_daemon_self_check_has_bounded_retries_before_gate(self):
        self.transient_failures=2
        self.activate()
        self.assertEqual(self.self_checks,3)
        self.assertTrue((self.release/'enabled').exists())

    def test_activation_waits_for_the_old_daemon_state_lock(self):
        with patch.object(m.fcntl,'flock',side_effect=[BlockingIOError(),None]) as lock:
            self.activate()
        self.assertEqual(lock.call_count,2)
        self.assertTrue((self.release/'enabled').exists())
        self.assertEqual({label for domain,label in self.jobs if domain=='system'},{m.LABEL,m.CC_LABEL})

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

class MacOSOutputTest(unittest.TestCase):
    def test_unload_waits_for_asynchronous_bootout(self):
        with patch.object(m,'loaded',side_effect=[True,True,True,False]), patch.object(m,'run') as run, patch.object(m.time,'sleep') as sleep:
            m.unload('gui/501',m.LABEL)
        run.assert_called_once_with(['/bin/launchctl','bootout','gui/501/'+m.LABEL])
        self.assertEqual(sleep.call_count,2)

    def test_unload_timeout_refuses_to_continue(self):
        with patch.object(m,'loaded',return_value=True), patch.object(m,'run'), patch.object(m.time,'monotonic',side_effect=[0,46]):
            with self.assertRaises(m.DeploymentError):
                m.unload('gui/501',m.LABEL)

    def test_unload_already_absent_is_idempotent(self):
        with patch.object(m,'loaded',return_value=False), patch.object(m,'run') as run:
            m.unload('gui/501',m.LABEL)
        run.assert_not_called()

    def test_disabled_supports_current_macos_and_boolean_formats(self):
        sample=b'''disabled services = {\n"com.apple.ManagedClientAgent.enrollagent" => disabled\n"com.cc-connect.service" => enabled\n"legacy.disabled" => true\n"legacy.enabled" => false\n}'''
        with patch.object(m.subprocess,'run',return_value=SimpleNamespace(returncode=0,stdout=sample)):
            self.assertTrue(m.disabled('gui/501','com.apple.ManagedClientAgent.enrollagent'))
            self.assertFalse(m.disabled('gui/501',m.CC_LABEL))
            self.assertTrue(m.disabled('gui/501','legacy.disabled'))
            self.assertFalse(m.disabled('gui/501','legacy.enabled'))
            self.assertFalse(m.disabled('gui/501','unknown'))

    def test_ps_finds_absolute_binary_paths_and_duplicate_instances(self):
        sample=b'''50420 /Users/user/.local/share/cc-connect/builds/current/cc-connect\n60420 /Users/user/.local/lib/ink-commons/releases/abc/cc-connect\n44 /bin/sh\n55 /tmp/cc-connect-helper\n'''
        with patch.object(m.subprocess,'run',return_value=SimpleNamespace(returncode=0,stdout=sample)):
            self.assertEqual(m.cc_process_pids(),[50420,60420])

    def test_lock_retries_a_slow_release_and_has_deadline(self):
        stream=MagicMock()
        with patch.object(m.fcntl,'flock',side_effect=[BlockingIOError(),None]) as call, patch.object(m.time,'sleep'):
            m.acquire_state_lock(stream)
            self.assertEqual(call.call_count,2)
        with patch.object(m.fcntl,'flock',side_effect=BlockingIOError()), patch.object(m.time,'monotonic',side_effect=[0,6]):
            with self.assertRaises(m.DeploymentError):
                m.acquire_state_lock(stream)

    def test_no_proxy_rejects_non_host_text(self):
        self.assertEqual(m.parse_env(['NO_PROXY=localhost,127.0.0.1,.example.com,[::1]'])['NO_PROXY'],'localhost,127.0.0.1,.example.com,[::1]')
        for value in ('user:password@host','secret text','a'*300,'host,,other'):
            with self.assertRaises(m.DeploymentError):
                m.parse_env(['NO_PROXY='+value])


if __name__ == '__main__':
    unittest.main()
