import importlib.util
import hashlib
import json
from pathlib import Path
import plistlib
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch, MagicMock

EXTERNAL_CC = 'org.example.cc'
EXTERNAL_WATCHDOG = 'org.example.watchdog'

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('manage', ROOT / 'deploy/macos/manage.py')
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
spec = importlib.util.spec_from_file_location('runtime', ROOT / 'deploy/macos/runtime.py')
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class ArtifactStageTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.home = Path(temp.name).resolve()
        self.revision = m.output(['git', '-C', ROOT, 'rev-parse', 'HEAD'])
        self.artifact = self.home / 'artifact'
        self.artifact.mkdir()
        self.config = self.home / 'config.json'
        self.config.write_text('{}')
        self.cc_binary = self.home / 'cc-connect'
        self.cc_binary.write_bytes(b'local cc-connect')
        self.releases = self.home / '.local/lib/ink-commons/releases'
        self.args = SimpleNamespace(revision=self.revision, artifact_dir=str(self.artifact),
                                    env=[], config=str(self.config), github_command=sys.executable,
                                    cc_config=str(self.home / 'config.toml'), project='test',
                                    cc_connect_binary=str(self.cc_binary))

    def write_artifact(self, revision):
        binary = self.artifact / 'ink'
        binary.write_text(f"#!/bin/sh\nprintf '%s\\n' '{revision}'\n")
        binary.chmod(0o644)  # GitHub Actions 下载的 ZIP 会移除执行权限。
        digest = hashlib.sha256(binary.read_bytes()).hexdigest()
        (self.artifact / 'SHA256SUMS').write_text(f'{digest}  ink\n')

    def stage(self):
        real_run = m.run

        def run_without_go(args, **kwargs):
            self.assertNotEqual(args[0], 'go')
            return real_run(args, **kwargs)

        with patch.object(m.Path, 'home', return_value=self.home), \
             patch.object(m.os, 'geteuid', return_value=501), \
             patch.object(m, 'run', side_effect=run_without_go), \
             patch.object(sys, 'path', [str(ROOT / 'deploy/macos'), *sys.path]), \
             patch('runtime.environment'):
            m.stage(self.args)

    def test_stages_verified_artifact_and_matching_scripts_without_go(self):
        self.write_artifact(self.revision)
        self.stage()
        release = self.releases / self.revision
        self.assertEqual(m.output([release / 'ink', 'version']), self.revision)
        self.assertFalse((release / 'cc-connect').exists())
        for script in ('runtime.py', 'manage.py'):
            expected = m.run(['git', '-C', ROOT, 'show', f'{self.revision}:deploy/macos/{script}'],
                             capture_output=True).stdout
            self.assertEqual((release / script).read_bytes(), expected)
        with self.assertRaisesRegex(m.DeploymentError, 'release already exists'):
            self.stage()

    def test_signature_rejects_changed_manifest(self):
        trust = self.home / 'allowed_signers'
        key = self.home / 'test-key'
        m.run(['/usr/bin/ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', key])
        trust.write_text('ink-release ' + key.with_suffix('.pub').read_text())
        manifest = self.artifact / 'SHA256SUMS'
        manifest.write_bytes(b'approved manifest')
        m.run(['/usr/bin/ssh-keygen', '-Y', 'sign', '-f', key, '-n', 'ink-commons', manifest],
              capture_output=True)
        m.verify_signature(self.artifact, trust)
        manifest.write_bytes(b'changed manifest')
        with self.assertRaisesRegex(m.DeploymentError, 'signature verification failed'):
            m.verify_signature(self.artifact, trust)

    def test_rejects_tampered_binary(self):
        self.write_artifact(self.revision)
        (self.artifact / 'ink').write_bytes(b'tampered')
        with self.assertRaisesRegex(m.DeploymentError, 'checksum mismatch'):
            self.stage()
        self.assertEqual(list(self.releases.iterdir()), [])

    def test_rejects_binary_for_a_different_revision(self):
        self.write_artifact('0' * 40)
        with self.assertRaisesRegex(m.DeploymentError, 'revision does not match'):
            self.stage()
        self.assertEqual(list(self.releases.iterdir()), [])


    def write_bundle(self):
        self.revision = 'c' * 40
        self.args.revision = self.revision
        self.write_artifact(self.revision)
        (self.artifact / 'REVISION').write_text(self.revision + '\n')
        for name in ('manage.py', 'runtime.py'):
            (self.artifact / name).write_bytes((ROOT / 'deploy/macos' / name).read_bytes())
        (self.artifact / 'config.example.json').write_text('{}')
        (self.artifact / 'SHA256SUMS').write_text(''.join(
            hashlib.sha256((self.artifact / name).read_bytes()).hexdigest() + f'  {name}\n'
            for name in m.BUNDLE_FILES
        ))

    def test_deploys_release_bundle_without_a_git_checkout(self):
        self.write_bundle()
        system = self.home / 'LaunchDaemons'
        gui = self.home / 'Library/LaunchAgents'
        gui.mkdir(parents=True)
        cc_agent = gui / (EXTERNAL_CC + '.plist')
        cc_agent.write_bytes(plistlib.dumps({'ProgramArguments': [str(self.cc_binary)]}))
        self.args.cc_connect_binary = None
        real_run = m.run

        def run(args, **kwargs):
            self.assertNotIn(args[0], ('git', 'go', 'sudo'))
            return real_run(args, **kwargs)

        release = self.releases / self.revision
        with patch.object(m, '__file__', str(self.artifact / 'manage.py')), \
             patch.object(m, 'SYSTEM_DIR', system), \
             patch.object(m.Path, 'home', return_value=self.home), \
             patch.object(m, 'loaded', return_value=False), \
             patch.object(m, 'run', side_effect=run), \
             patch.object(m, 'activate_staged') as activate, \
             patch.object(sys, 'path', [str(self.artifact), *sys.path]), \
             patch('runtime.environment'):
            m.deploy(self.args)
            activate.assert_called_once_with(release)
            self.assertEqual((release / 'manage.py').read_bytes(), (self.artifact / 'manage.py').read_bytes())
            self.assertEqual((release / 'runtime.py').read_bytes(), (self.artifact / 'runtime.py').read_bytes())
            self.assertEqual((self.home / '.local/state/ink-commons').stat().st_mode & 0o777, 0o700)

            activate.reset_mock()
            (release / 'runtime.py').write_bytes(b'changed after staging')
            with self.assertRaisesRegex(m.DeploymentError, 'checksum mismatch: runtime.py'):
                m.deploy(self.args)
            activate.assert_not_called()

    def test_fresh_deploy_runs_full_login_activation_in_temporary_home(self):
        self.write_bundle()
        release = self.releases / self.revision
        self.config.write_text('{}')
        profile = self.home / 'profile.json'
        profile.write_text(json.dumps({'repository': 'other/ink',
                                      'path': ['/usr/bin', '/bin']}))
        self.args.profile = str(profile)
        self.args.cc_config = None
        self.args.project = None
        launched = []
        actual_run = m.run
        account = SimpleNamespace(pw_uid=m.os.geteuid(), pw_dir=str(self.home), pw_name='test-user')

        def run(args, **kwargs):
            self.assertNotIn(args[0], ('git', 'go', 'sudo'))
            if args[0] == '/bin/launchctl':
                launched.append(args)
                return SimpleNamespace(returncode=0)
            return actual_run(args, **kwargs)

        with patch.object(m, '__file__', str(self.artifact / 'manage.py')), \
             patch.object(m, 'SYSTEM_DIR', self.home / 'LaunchDaemons'), \
             patch.object(m.pwd, 'getpwuid', return_value=account), \
             patch.object(m.pwd, 'getpwnam', return_value=account), \
             patch.object(m.Path, 'home', return_value=self.home), \
             patch.object(m, 'loaded', return_value=False), \
             patch.object(m, 'disabled', return_value=False), \
             patch.object(m, 'run', side_effect=run), \
             patch.object(m.socket, 'create_connection') as connect, \
             patch.object(m, 'health', return_value={'revision': self.revision, 'gate': 'closed'}), \
             patch.object(m, 'local_get', return_value={'status': 'ok'}), \
             patch.object(sys, 'path', [str(self.artifact), *sys.path]):
            m.deploy(self.args)
        connect.assert_not_called()
        self.assertTrue(any(call[1:3] == ['bootstrap', 'gui/' + str(account.pw_uid)] for call in launched))
        self.assertTrue((release / 'enabled').exists())
        self.assertEqual(json.loads((release / 'deployment.json').read_text())['profile']['repository'],
                         'other/ink')

    def test_bundle_rejects_tampered_runtime_script(self):
        self.write_bundle()
        (self.artifact / 'runtime.py').write_bytes(b'tampered')
        with self.assertRaisesRegex(m.DeploymentError, 'checksum mismatch: runtime.py'):
            self.stage()
        self.assertEqual(list(self.releases.iterdir()), [])


class UpgradeTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name).resolve()
        self.current = self.root / 'releases' / ('a' * 40)
        self.target = self.root / 'releases' / ('b' * 40)
        self.current.mkdir(parents=True)
        self.entry = self.root / 'ink'
        self.entry.symlink_to(self.current / 'ink')
        (self.current / 'config.json').write_text(json.dumps({'github_command': '/test/yuxingyi'}))
        binary = b'current cc-connect'
        (self.current / 'cc-connect').write_bytes(binary)
        (self.current / 'deployment.json').write_text(json.dumps({
            'revision': self.current.name,
            'cc_config': '/test/cc.toml', 'data': '/test/state', 'project': 'existing-project',
            'environment': {'HTTPS_PROXY': 'http://127.0.0.1:7897', 'NODE_OPTIONS': '--use-env-proxy'},
            'cc_binary_sha256': hashlib.sha256(binary).hexdigest(),
        }))
        self.calls = []

    def make_staged_target(self):
        self.target.mkdir()
        for name in ('config.json', 'cc-connect'):
            (self.target / name).write_bytes((self.current / name).read_bytes())
        settings = json.loads((self.current / 'deployment.json').read_text())
        settings['revision'] = self.target.name
        settings['config_source'] = str(self.current / 'config.json')
        settings.pop('cc_binary_sha256', None)
        (self.target / 'deployment.json').write_text(json.dumps(settings))

    def make_published_manifest(self, release):
        (release / 'ink').write_text(f"#!/bin/sh\nprintf '%s\\n' '{release.name}'\n")
        (release / 'ink').chmod(0o700)
        (release / 'REVISION').write_text(release.name + '\n')
        for name in ('manage.py', 'runtime.py', 'config.example.json'):
            (release / name).write_text(f'published {name}\n')
        return ''.join(
            hashlib.sha256((release / name).read_bytes()).hexdigest() + f'  {name}\n'
            for name in m.BUNDLE_FILES
        )

    def mock_manifest_download(self, manifest, revision):
        real_run = m.run

        def run(args, **kwargs):
            if args[0] != '/test/yuxingyi':
                return real_run(args, **kwargs)
            self.assertEqual(args[5:8], ['release', 'download', 'build-' + revision])
            self.assertEqual(args[10:], ['--pattern=SHA256SUMS'])
            (Path(args[9]) / 'SHA256SUMS').write_text(manifest)

        return patch.object(m, 'run', side_effect=run)

    def test_downloads_main_and_reuses_current_configuration(self):
        def run(args, **kwargs):
            self.calls.append(args)
            if args[0] == '/test/yuxingyi':
                self.assertEqual(kwargs['env']['HTTPS_PROXY'], 'http://127.0.0.1:7897')
                self.assertEqual(kwargs['env']['NODE_OPTIONS'], '--use-env-proxy')
                self.assertEqual(kwargs['env']['PATH'], '/usr/bin:/bin')
                if args[5] == 'api':
                    return SimpleNamespace(stdout=('build-' + self.target.name).encode())
                directory = Path(args[args.index('--dir') + 1])
                (directory / 'REVISION').write_text(self.target.name)

        def stage(args):
            self.assertTrue(Path(args.artifact_dir).is_dir())
            self.assertEqual(args.revision, self.target.name)
            self.assertEqual(args.config, str(self.current / 'config.json'))
            self.assertEqual(args.cc_config, '/test/cc.toml')
            self.assertEqual(args.project, 'existing-project')
            self.assertEqual(args.env, ['HTTPS_PROXY=http://127.0.0.1:7897', 'NODE_OPTIONS=--use-env-proxy'])

        with patch.dict(m.os.environ, {'PATH': '/usr/bin:/bin'}, clear=True), \
             patch.object(m, 'run', side_effect=run), \
             patch.object(m, 'stage', side_effect=stage) as staged, \
             patch.object(m, 'activate_staged') as activate:
            m.upgrade_system(self.root, self.entry, apply=True)

        staged.assert_called_once()
        activate.assert_called_once_with(self.target)
        self.assertEqual(self.calls[0][:6], ['/test/yuxingyi', '--profile', 'inspect', '-R', m.REPOSITORY, 'api'])
        self.assertEqual(self.calls[1][5:8], ['release', 'download', 'build-' + self.target.name])
        self.assertIn('--pattern=manage.py', self.calls[1])
        self.assertIn('--pattern=runtime.py', self.calls[1])

    def test_dry_run_selects_published_release_without_download_or_activation(self):
        with patch.object(m, 'output', return_value='build-' + self.target.name) as query, \
             patch.object(m, 'run') as run, patch.object(m, 'activate_staged') as activate:
            m.upgrade_system(self.root, self.entry, channel='stable', dry_run=True)
        self.assertIn('repos/' + m.REPOSITORY + '/releases/latest', query.call_args.args[0])
        run.assert_not_called()
        activate.assert_not_called()

    def test_prepare_without_apply_does_not_activate(self):
        self.make_staged_target()
        manifest = self.make_published_manifest(self.target)
        tag = 'build-' + self.target.name
        with self.mock_manifest_download(manifest, self.target.name), \
             patch.object(m, 'activate_staged') as activate:
            m.upgrade_system(self.root, self.entry, tag=tag)
        activate.assert_not_called()

    def test_unpublished_build_does_not_stage_or_activate(self):
        with patch.object(m, 'run', side_effect=subprocess.CalledProcessError(1, 'release download')) as run, \
             patch.object(m, 'select_release', return_value=('build-' + self.target.name, self.target.name)), \
             patch.object(m, 'stage') as staged:
            with self.assertRaisesRegex(m.DeploymentError, 'check the GitHub Actions publish job'):
                m.upgrade_system(self.root, self.entry, apply=True)

        run.assert_called_once()
        staged.assert_not_called()

    def test_healthy_current_version_needs_no_download_or_sudo(self):
        with patch.object(m, 'run') as run, \
             patch.object(m, 'select_release', return_value=('build-' + self.current.name, self.current.name)), \
             patch.object(m, 'health', return_value={'revision': self.current.name, 'gate': 'open'}):
            m.upgrade_system(self.root, self.entry, apply=True)

        run.assert_not_called()

    def test_missing_health_socket_reactivates_installed_target(self):
        manifest = self.make_published_manifest(self.current)
        with self.mock_manifest_download(manifest, self.current.name), \
             patch.object(m, 'select_release', return_value=('build-' + self.current.name, self.current.name)), \
             patch.object(m, 'health', side_effect=FileNotFoundError), \
             patch.object(m, 'activate_staged') as activate:
            m.upgrade_system(self.root, self.entry, apply=True)

        activate.assert_called_once_with(self.current)

    def test_reuses_matching_stage(self):
        self.make_staged_target()
        manifest = self.make_published_manifest(self.target)
        with self.mock_manifest_download(manifest, self.target.name), \
             patch.object(m, 'select_release', return_value=('build-' + self.target.name, self.target.name)), \
             patch.object(m, 'activate_staged') as activate:
            m.upgrade_system(self.root, self.entry, apply=True)

        activate.assert_called_once_with(self.target)

    def test_rejects_tampered_staged_scripts_using_published_manifest(self):
        self.make_staged_target()
        manifest = self.make_published_manifest(self.target)
        for name in ('manage.py', 'runtime.py'):
            with self.subTest(file=name):
                script = self.target / name
                original = script.read_bytes()
                script.write_bytes(b'changed after staging')
                with self.mock_manifest_download(manifest, self.target.name), \
                     patch.object(m, 'select_release', return_value=('build-' + self.target.name, self.target.name)), \
                     patch.object(m, 'activate_staged') as activate:
                    with self.assertRaisesRegex(m.DeploymentError, 'checksum mismatch: ' + name):
                        m.upgrade_system(self.root, self.entry, apply=True)
                activate.assert_not_called()
                script.write_bytes(original)

    def test_rejects_staged_config_and_environment_differences(self):
        self.make_staged_target()
        mismatches = (
            ('config.json', b'{}', 'configuration differs'),
            ('deployment.json', (self.target / 'deployment.json').read_bytes().replace(
                b'http://127.0.0.1:7897', b'http://127.0.0.1:9801'), 'configuration differs'),
        )
        for name, content, diagnostic in mismatches:
            with self.subTest(file=name):
                file = self.target / name
                original = file.read_bytes()
                file.write_bytes(content)
                with patch.object(m, 'run') as run, \
                     patch.object(m, 'select_release', return_value=('build-' + self.target.name, self.target.name)):
                    with self.assertRaisesRegex(m.DeploymentError, diagnostic):
                        m.upgrade_system(self.root, self.entry, apply=True)
                run.assert_not_called()
                file.write_bytes(original)


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
        (self.agents / (m.LABEL + '.plist')).chmod(0o600)
        (self.agents / (EXTERNAL_WATCHDOG + '.plist')).write_bytes(plistlib.dumps({'Label':EXTERNAL_WATCHDOG,'ProgramArguments':['watchdog']}))
        cc = {'Label': EXTERNAL_CC, 'ProgramArguments': [sys.executable],
              'EnvironmentVariables': {'PATH': '/usr/bin:/bin'}}
        (self.agents / (EXTERNAL_CC + '.plist')).write_bytes(plistlib.dumps(cc))
        self.system = self.home / 'system'
        self.system.mkdir()
        self.jobs = {('gui/501', m.LABEL), ('gui/501', EXTERNAL_CC), ('gui/501',EXTERNAL_WATCHDOG)}
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
            if self.fail_stop and (self.data / 'state.sqlite3').exists() and label == m.LABEL:
                raise RuntimeError('new daemon cannot be stopped')
            self.jobs.discard((domain, label))
        if args[:2] == ['/bin/launchctl', 'bootstrap']:
            domain, path = args[2:]
            label = plistlib.loads(Path(path).read_bytes())['Label']
            self.jobs.add((domain, label))
            if label == m.LABEL and plistlib.loads(Path(path).read_bytes())['ProgramArguments'] != ['old']:
                # 模拟新版迁移，只能在 gate 未打开的条件下启动。
                self.assertFalse((self.release / 'enabled').exists())
                (self.data / 'state.sqlite3').write_bytes(b'new database')
                if self.fail_bootstrap:
                    raise RuntimeError('bootstrap partially succeeded')
        if args[0] != '/bin/launchctl' and self.fail:
            raise RuntimeError('App unavailable')
        return SimpleNamespace(returncode=0)

    def activate(self, uid=501):
        args = SimpleNamespace(user='service', release=str(self.release))
        with patch.object(m, 'SYSTEM_DIR', self.system), \
             patch.object(m.pwd, 'getpwnam', return_value=self.account), \
             patch.object(m.os, 'geteuid', return_value=uid), \
             patch.object(m, 'loaded', side_effect=lambda d,l: (d,l) in self.jobs), \
             patch.object(m, 'run', side_effect=self.run_fake), \
             patch.object(m, 'validate_state_directory'), \
             patch.object(m, 'disabled', return_value=False), \
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

    def test_login_upgrade_preserves_cc_connect_and_watchdog(self):
        external = {name: (self.agents / (name + '.plist')).read_bytes()
                    for name in (EXTERNAL_CC, EXTERNAL_WATCHDOG)}
        self.activate()
        for name, content in external.items():
            self.assertEqual((self.agents / (name + '.plist')).read_bytes(), content)
            self.assertIn(('gui/501', name), self.jobs)
            self.assertFalse(any(name in ' '.join(call) for call in self.calls))
        self.assertTrue((self.release / 'enabled').exists())
        self.assertEqual(self.link.resolve(), self.release / 'ink')
        self.assertFalse((self.release / 'cc-connect').exists())
        self.assertTrue((self.agents / (m.LABEL + '.plist')).is_file())

    def test_root_or_legacy_system_refused_before_launchd_write(self):
        with self.assertRaisesRegex(m.DeploymentError, 'non-root'):
            self.activate(uid=0)
        self.jobs.add(('system', m.LABEL))
        with self.assertRaisesRegex(m.DeploymentError, 'legacy system service'):
            self.activate()
        self.assertEqual(self.calls, [])

    def test_cc_connect_preflight_and_post_check(self):
        for stop_phase in (1, 2):
            with self.subTest(stop_phase=stop_phase):
                self.calls.clear()
                with patch.object(m, 'check_provider_endpoints',
                                  side_effect=[None] * (stop_phase - 1) + [m.DeploymentError('offline')]):
                    with self.assertRaisesRegex(m.DeploymentError, 'offline'):
                        self.activate()
                self.assertEqual(any('bootout' in call for call in self.calls), stop_phase == 2)
                self.assertFalse((self.release / 'enabled').exists())

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
        self.assertIn(('gui/501', m.LABEL), self.jobs)

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

    def test_repeated_login_update(self):
        self.activate()
        self.activate()
        self.assertTrue((self.release/'enabled').exists())

    def test_only_non_secret_proxy_environment_is_allowed(self):
        env=m.parse_env(['HTTPS_PROXY=http://127.0.0.1:7897','NODE_OPTIONS=--use-env-proxy'])
        settings={**self.settings,'environment':env}
        self.assertEqual(m.ink_plist(self.release, settings)['EnvironmentVariables']['HTTPS_PROXY'],env['HTTPS_PROXY'])
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

    def test_runtime_starts_with_independent_http_provider_credentials(self):
        credentials = self.home / 'providers.json'
        credentials.write_text(json.dumps({'http_provider_token': 'fixture'}))
        credentials.chmod(0o600)
        (self.release / 'deployment.json').write_text(json.dumps({
            'credentials': str(credentials), 'data': str(self.data),
        }))
        with patch.object(runtime, '__file__', str(self.release / 'runtime.py')), \
             patch.object(runtime.os, 'execve') as start:
            runtime.main()
        self.assertEqual(start.call_args.args[2]['INK_HTTP_PROVIDER_TOKEN'], 'fixture')

    def test_no_provider_means_no_external_service_probe(self):
        with patch.object(m.socket, 'create_connection') as connect:
            m.check_provider_endpoints({})
        connect.assert_not_called()


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
            self.assertFalse(m.disabled('gui/501',EXTERNAL_CC))
            self.assertTrue(m.disabled('gui/501','legacy.disabled'))
            self.assertFalse(m.disabled('gui/501','legacy.enabled'))
            self.assertFalse(m.disabled('gui/501','unknown'))

    def test_lock_retries_a_slow_release_and_has_deadline(self):
        stream=MagicMock()
        with patch.object(m.fcntl,'flock',side_effect=[BlockingIOError(),None]) as call, patch.object(m.time,'sleep'):
            m.acquire_state_lock(stream)
            self.assertEqual(call.call_count,2)
        with patch.object(m.fcntl,'flock',side_effect=BlockingIOError()), patch.object(m.time,'monotonic',side_effect=[0,6]):
            with self.assertRaises(m.DeploymentError):
                m.acquire_state_lock(stream)

    def test_config_drift_reports_changed_scopes_without_values(self):
        with tempfile.TemporaryDirectory() as tmp:
            declaration = Path(tmp) / 'policy.json'
            declaration.write_text(json.dumps({'schema': 1, 'global': {},
                                               'repositories': {'acme/demo': {'reviewer_login': 'old'}}}))
            with patch.object(m, 'local_get', return_value={'schema': 1, 'global': {},
                        'repositories': {'acme/demo': {'reviewer_login': 'new'}}}):
                self.assertEqual(m.config_drift(tmp, declaration),
                                 {'status': 'changed', 'scopes': ['acme/demo']})

    def test_retention_protects_service_and_active_release(self):
        with tempfile.TemporaryDirectory() as tmp:
            home = Path(tmp).resolve()
            root = home / '.local/lib/ink-commons'
            releases = root / 'releases'
            paths = [releases / (letter * 40) for letter in ('a', 'b', 'c', 'd', 'e')]
            for stamp, path in enumerate(paths, 1):
                path.mkdir(parents=True)
                m.os.utime(path, (stamp, stamp))
            (root / 'ink').symlink_to(paths[0] / 'ink')
            (paths[0] / 'deployment.json').write_text(json.dumps({
                'config_source': str(paths[2] / 'config.json'),
            }))
            m.os.utime(paths[0], (1, 1))
            system = home / 'LaunchDaemons'
            system.mkdir()
            (system / 'cc.plist').write_bytes(plistlib.dumps({'ProgramArguments': [str(paths[1] / 'cc')]}))
            legacy = root / 'start'
            legacy.write_text('legacy')
            with patch.object(m, 'SYSTEM_DIR', system):
                plan = m.retention_plan(home, root, {'releases': 1}, include_legacy=True)
            self.assertEqual(set(plan), {paths[3], legacy})
            self.assertTrue(all(path.is_dir() for path in paths))

    def test_rotate_stderr_leaves_small_logs_and_preserves_old_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            data = Path(tmp)
            log = data / 'stderr.log'
            log.write_bytes(b'test')
            m.rotate_stderr(data, 5)
            self.assertTrue(log.is_file())
            m.rotate_stderr(data, 4)
            self.assertFalse(log.exists())
            self.assertEqual(next(data.glob('stderr.log.*')).read_bytes(), b'test')

    def test_no_proxy_rejects_non_host_text(self):
        self.assertEqual(m.parse_env(['NO_PROXY=localhost,127.0.0.1,.example.com,[::1]'])['NO_PROXY'],'localhost,127.0.0.1,.example.com,[::1]')
        for value in ('user:password@host','secret text','a'*300,'host,,other'):
            with self.assertRaises(m.DeploymentError):
                m.parse_env(['NO_PROXY='+value])


if __name__ == '__main__':
    unittest.main()
