import importlib.util
import fcntl
import os
import hashlib
import io
import json
from pathlib import Path
import plistlib
import shutil
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
        self.write_bundle_for('c' * 40)

    def write_bundle_for(self, revision):
        self.revision = revision
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

    def test_deploys_from_the_published_example_without_local_configuration(self):
        self.write_bundle()
        self.config.unlink()
        self.args.config = str(self.artifact / 'config.example.json')
        self.args.cc_config = None
        self.args.project = None
        system = self.home / 'LaunchDaemons'
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

    def test_deploy_rejects_unloaded_system_plist_before_writing(self):
        system = self.home / 'LaunchDaemons'
        system.mkdir()
        old_plist = system / (m.LABEL + '.plist')
        old_plist.write_bytes(b'legacy plist')
        with patch.object(m, 'SYSTEM_DIR', system), \
             patch.object(m.Path, 'home', return_value=self.home), \
             patch.object(m, 'loaded', return_value=False), \
             patch.object(m, 'stage') as stage:
            with self.assertRaisesRegex(m.DeploymentError, 'already exists'):
                m.deploy(self.args)
        stage.assert_not_called()
        self.assertFalse((self.home / '.local/state/ink-commons').exists())
        self.assertEqual(old_plist.read_bytes(), b'legacy plist')

    def test_bundle_rejects_tampered_runtime_script(self):
        self.write_bundle()
        (self.artifact / 'runtime.py').write_bytes(b'tampered')
        with self.assertRaisesRegex(m.DeploymentError, 'checksum mismatch: runtime.py'):
            self.stage()
        self.assertEqual(list(self.releases.iterdir()), [])

    def stage_active_bundle(self):
        self.write_bundle()
        self.args.release_repo = 'old-owner/ink'
        self.args.service_path = '/custom/bin:/usr/bin:/bin'
        self.stage()
        release = self.releases / self.revision
        (self.releases.parent / 'ink').symlink_to(release / 'ink')
        return release

    def test_manual_stage_inherits_active_settings_and_honors_explicit_override(self):
        self.stage_active_bundle()
        self.args.release_repo = None
        self.args.service_path = None
        self.write_bundle_for('d' * 40)
        self.stage()
        settings = json.loads((self.releases / self.revision / 'deployment.json').read_text())
        self.assertEqual(settings['release_repo'], 'old-owner/ink')
        self.assertEqual(settings['service_path'], '/custom/bin:/usr/bin:/bin')
        self.args.release_repo = 'new-owner/fork'
        self.write_bundle_for('e' * 40)
        self.stage()
        settings = json.loads((self.releases / self.revision / 'deployment.json').read_text())
        self.assertEqual(settings['release_repo'], 'new-owner/fork')
        self.assertEqual(settings['service_path'], '/custom/bin:/usr/bin:/bin')

    def test_stage_rejects_unreadable_active_metadata_instead_of_using_defaults(self):
        active = self.stage_active_bundle()
        (active / 'deployment.json').write_text('{corrupt')
        self.args.release_repo = None
        self.args.service_path = None
        self.write_bundle_for('d' * 40)
        with self.assertRaisesRegex(m.DeploymentError, 'active deployment metadata'):
            self.stage()
        self.assertFalse((self.releases / self.revision).exists())

    def test_manual_stage_accepts_legacy_executable_entry_without_saved_settings(self):
        self.write_bundle()
        entry = self.releases.parent / 'ink'
        entry.parent.mkdir(parents=True)
        entry.write_text('#!/bin/sh\nexit 0\n')
        entry.chmod(0o700)
        before = entry.read_bytes()
        with patch.object(m.sys, 'stderr', new_callable=io.StringIO) as stderr:
            self.stage()
        self.assertIn('Omitted release repository and service PATH use defaults.', stderr.getvalue())
        settings = json.loads((self.releases / self.revision / 'deployment.json').read_text())
        self.assertNotIn('release_repo', settings)
        self.assertNotIn('service_path', settings)
        self.assertEqual(entry.read_bytes(), before)
        self.assertFalse(entry.is_symlink())

    def test_manual_stage_rejects_broken_versioned_entry(self):
        self.write_bundle()
        entry = self.releases.parent / 'ink'
        entry.parent.mkdir(parents=True)
        entry.symlink_to(self.releases / ('d' * 40) / 'ink')
        with self.assertRaisesRegex(m.DeploymentError, 'active deployment metadata'):
            self.stage()
        self.assertFalse((self.releases / self.revision).exists())

    def test_set_release_repo_switches_source_in_place_only_after_release_lookup(self):
        active = self.stage_active_bundle()
        manifest = active / 'deployment.json'
        entry = self.releases.parent / 'ink'
        before = manifest.read_bytes()
        binary = (active / 'ink').read_bytes()
        args = SimpleNamespace(repo='new-owner/fork')
        with patch.object(m.Path, 'home', return_value=self.home), \
             patch.object(m.os, 'geteuid', return_value=501), \
             patch.object(m, 'output', return_value='missing') as output, \
             patch.object(m, 'activate_staged') as activate:
            with self.assertRaisesRegex(m.DeploymentError, 'did not return the installed revision'):
                m.set_release_repo(args)
            self.assertEqual(manifest.read_bytes(), before)
            output.return_value = 'build-' + self.revision
            m.set_release_repo(args)
            activate.assert_not_called()
        self.assertIn(f'repos/new-owner/fork/releases/tags/build-{self.revision}', output.call_args.args[0])
        self.assertEqual(json.loads(manifest.read_text()), {**json.loads(before), 'release_repo': args.repo})
        self.assertEqual(entry.resolve(), active / 'ink')
        self.assertEqual((active / 'ink').read_bytes(), binary)
        self.assertEqual(manifest.stat().st_mode & 0o777, 0o600)

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
        (self.current / 'config.json').write_text(json.dumps({'github_command': '/test/github-app-cli'}))
        binary = b'current cc-connect'
        (self.current / 'cc-connect').write_bytes(binary)
        (self.current / 'deployment.json').write_text(json.dumps({
            'revision': self.current.name, 'home': str(self.root),
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
            if args[0] != '/test/github-app-cli':
                return real_run(args, **kwargs)
            self.assertEqual(args[5:8], ['release', 'download', 'build-' + revision])
            self.assertEqual(args[10:], ['--pattern=SHA256SUMS'])
            (Path(args[9]) / 'SHA256SUMS').write_text(manifest)

        return patch.object(m, 'run', side_effect=run)

    def test_downloads_main_and_reuses_current_configuration(self):
        def run(args, **kwargs):
            self.calls.append(args)
            if args[0] == '/test/github-app-cli':
                self.assertEqual(kwargs['env']['HTTPS_PROXY'], 'http://127.0.0.1:7897')
                self.assertEqual(kwargs['env']['NODE_OPTIONS'], '--use-env-proxy')
                self.assertEqual(kwargs['env']['PATH'], '/usr/bin:/bin')
                if args[5] == 'api':
                    return SimpleNamespace(stdout=self.target.name.encode())
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
            m.upgrade_system(self.root, self.entry)

        staged.assert_called_once()
        activate.assert_called_once_with(self.target)
        self.assertEqual(self.calls[0][:6], ['/test/github-app-cli', '--profile', 'inspect', '-R', m.DEFAULT_RELEASE_REPO, 'api'])
        self.assertEqual(self.calls[1][5:8], ['release', 'download', 'build-' + self.target.name])
        self.assertIn('--pattern=manage.py', self.calls[1])
        self.assertIn('--pattern=runtime.py', self.calls[1])

    def test_upgrade_uses_configured_repo_and_preserves_deployment_settings(self):
        config_file = self.current / 'config.json'
        config_file.write_text(json.dumps({
            'github_command': '/test/github-app-cli', 'github_read_args': [],
        }))

        settings_file = self.current / 'deployment.json'
        settings = json.loads(settings_file.read_text())
        settings.update(release_repo='fork-owner/ink-fork', service_path='/custom/bin:/usr/bin:/bin')
        settings_file.write_text(json.dumps(settings))

        def run(args, **kwargs):
            self.calls.append(args)
            if args[1] == 'api':
                return SimpleNamespace(stdout=self.target.name.encode())
            directory = Path(args[args.index('--dir') + 1])
            (directory / 'REVISION').write_text(self.target.name)

        with patch.object(m, 'run', side_effect=run), patch.object(m, 'stage') as staged, \
             patch.object(m, 'activate_staged'):
            m.upgrade_system(self.root, self.entry)

        self.assertEqual(self.calls[0][:3], [
            '/test/github-app-cli', 'api', 'repos/fork-owner/ink-fork/git/ref/heads/main',
        ])
        self.assertEqual(self.calls[1][1:4], ['release', 'download', 'build-' + self.target.name])
        self.assertEqual(self.calls[1][6:8], ['--repo', 'fork-owner/ink-fork'])
        staged.assert_called_once()
        self.assertEqual(staged.call_args.args[0].release_repo, 'fork-owner/ink-fork')
        self.assertEqual(staged.call_args.args[0].service_path, settings['service_path'])
        plist = m.ink_plist(self.current, {**settings, 'python': '/usr/bin/python3'}, 'operator')
        self.assertEqual(plist['EnvironmentVariables']['PATH'], settings['service_path'])

    def test_upgrade_rejects_invalid_release_repository_before_network(self):
        settings_file = self.current / 'deployment.json'
        settings = json.loads(settings_file.read_text())
        settings_file.write_text(json.dumps({**settings, 'release_repo': 'org/..'}))
        with patch.object(m, 'output') as output:
            with self.assertRaisesRegex(m.DeploymentError, 'invalid release repository'):
                m.upgrade_system(self.root, self.entry)
        output.assert_not_called()

    def test_unpublished_build_does_not_stage_or_activate(self):
        with patch.object(m, 'run', side_effect=subprocess.CalledProcessError(1, 'release download')) as run, \
             patch.object(m, 'output', return_value=self.target.name), \
             patch.object(m, 'stage') as staged:
            with self.assertRaisesRegex(m.DeploymentError, 'check the GitHub Actions publish job'):
                m.upgrade_system(self.root, self.entry)

        run.assert_called_once()
        staged.assert_not_called()

    def test_healthy_current_system_version_needs_no_download_or_sudo(self):
        (self.root / (m.LABEL + '.plist')).touch()
        with patch.object(m, 'SYSTEM_DIR', self.root), \
             patch.object(m, 'loaded', side_effect=lambda domain, label: domain == 'system'), \
             patch.object(m, 'run') as run, \
             patch.object(m, 'output', return_value=self.current.name), \
             patch.object(m, 'health', return_value={'revision': self.current.name, 'gate': 'open'}):
            m.upgrade_system(self.root, self.entry)

        run.assert_not_called()

    def test_healthy_current_login_version_still_migrates(self):
        manifest = self.make_published_manifest(self.current)
        with self.mock_manifest_download(manifest, self.current.name), \
             patch.object(m, 'loaded', side_effect=lambda domain, label: domain != 'system'), \
             patch.object(m, 'output', return_value=self.current.name), \
             patch.object(m, 'health', return_value={'revision': self.current.name, 'gate': 'open'}), \
             patch.object(m, 'activate_staged') as activate:
            m.upgrade_system(self.root, self.entry)

        activate.assert_called_once_with(self.current)

    def test_missing_health_socket_reactivates_installed_target(self):
        manifest = self.make_published_manifest(self.current)
        with self.mock_manifest_download(manifest, self.current.name), \
             patch.object(m, 'output', return_value=self.current.name), \
             patch.object(m, 'health', side_effect=FileNotFoundError), \
             patch.object(m, 'activate_staged') as activate:
            m.upgrade_system(self.root, self.entry)

        activate.assert_called_once_with(self.current)

    def test_reuses_matching_stage(self):
        self.make_staged_target()
        manifest = self.make_published_manifest(self.target)
        with self.mock_manifest_download(manifest, self.target.name), \
             patch.object(m, 'output', return_value=self.target.name), \
             patch.object(m, 'activate_staged') as activate:
            m.upgrade_system(self.root, self.entry)

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
                     patch.object(m, 'output', return_value=self.target.name), \
                     patch.object(m, 'activate_staged') as activate:
                    with self.assertRaisesRegex(m.DeploymentError, 'checksum mismatch: ' + name):
                        m.upgrade_system(self.root, self.entry)
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
                     patch.object(m, 'output', return_value=self.target.name):
                    with self.assertRaisesRegex(m.DeploymentError, diagnostic):
                        m.upgrade_system(self.root, self.entry)
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
        self.plist = self.system / (m.LABEL + '.plist')
        self.agent_plist = self.agents / self.plist.name
        self.jobs = {('gui/501', m.LABEL), ('gui/501', EXTERNAL_CC), ('gui/501',EXTERNAL_WATCHDOG)}
        self.disabled_jobs = set()
        self.account = SimpleNamespace(pw_uid=501, pw_gid=20, pw_dir=str(self.home))
        self.calls = []
        self.fail = False
        self.fail_sudo = False
        self.fail_bootstrap = False
        self.fail_stop = False
        self.transient_failures = 0
        self.self_checks = 0

    def run_fake(self, args, **kwargs):
        args = [str(x) for x in args]
        self.calls.append(args)
        if args == ['/usr/bin/sudo', '-v']:
            if self.fail_sudo:
                raise subprocess.CalledProcessError(1, args)
            return
        admin = args[:2] == ['/usr/bin/sudo', '--']
        if admin:
            args = args[2:]
        if args[0] == '/usr/bin/install':
            self.assertTrue(admin)
            self.assertEqual(args[1:-2], ['-S', '-o', 'root', '-g', 'wheel', '-m', '0644'])
            shutil.copyfile(args[-2], args[-1])
            return
        if args[0] == '/bin/rm':
            self.assertTrue(admin)
            Path(args[-1]).unlink(missing_ok=True)
            return
        if args[:2] == ['/bin/launchctl', 'bootout']:
            self.assertEqual(admin, args[2].startswith('system/'))
            domain, label = args[2].rsplit('/', 1)
            if self.fail_stop and (self.data / 'state.sqlite3').exists() and label == m.LABEL:
                raise RuntimeError('new daemon cannot be stopped')
            self.jobs.discard((domain, label))
        if args[0] == '/bin/launchctl' and args[1] in ('enable', 'disable'):
            domain, label = args[2].rsplit('/', 1)
            self.assertEqual(admin, domain == 'system')
            if args[1] == 'enable':
                self.disabled_jobs.discard((domain, label))
            else:
                self.disabled_jobs.add((domain, label))
        if args[:2] == ['/bin/launchctl', 'bootstrap']:
            domain, path = args[2:]
            self.assertEqual(admin, domain == 'system')
            label = plistlib.loads(Path(path).read_bytes())['Label']
            if (domain, label) in self.disabled_jobs:
                raise subprocess.CalledProcessError(1, args)
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
             patch.object(m, 'disabled', side_effect=lambda d,l: (d,l) in self.disabled_jobs), \
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

    def test_login_migration_preserves_cc_connect_and_watchdog(self):
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
        self.assertFalse(self.agent_plist.exists())
        self.assertNotIn(('gui/501', m.LABEL), self.jobs)
        self.assertIn(('system', m.LABEL), self.jobs)
        self.assertEqual(plistlib.loads(self.plist.read_bytes())['UserName'], 'service')
        stop = self.calls.index(['/bin/launchctl', 'bootout', 'gui/501/' + m.LABEL])
        start = self.calls.index(['/usr/bin/sudo', '--', '/bin/launchctl', 'bootstrap', 'system', str(self.plist)])
        self.assertLess(stop, start)

    def test_first_activation_needs_no_login_service_or_plist(self):
        self.agent_plist.unlink()
        self.jobs.clear()
        self.link.unlink()
        self.activate()
        self.assertEqual(self.jobs, {('system', m.LABEL)})
        plist = plistlib.loads(self.plist.read_bytes())
        self.assertEqual(plist['UserName'], 'service')
        self.assertTrue(plist['RunAtLoad'])
        self.assertTrue(plist['KeepAlive'])

    def test_root_or_a_different_service_account_is_refused(self):
        with self.assertRaisesRegex(m.DeploymentError, 'without sudo'):
            self.activate(uid=0)
        self.plist.write_bytes(plistlib.dumps({'UserName': 'another-user'}))
        with self.assertRaisesRegex(m.DeploymentError, 'belongs to a different user'):
            self.activate()
        self.assertEqual(self.calls, [])

    def test_missing_admin_authorization_keeps_legacy_service(self):
        self.fail_sudo = True
        with self.assertRaisesRegex(m.DeploymentError, 'administrator authorization'):
            self.activate()
        self.assertEqual(self.calls, [['/usr/bin/sudo', '-v']])
        self.assertEqual(self.agent_plist.read_bytes(), self.old_plist)
        self.assertIn(('gui/501', m.LABEL), self.jobs)

    def test_contended_system_lock_leaves_both_services_untouched(self):
        descriptor = os.open(self.system, os.O_RDONLY | os.O_DIRECTORY)
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(m.DeploymentError, 'another system Ink activation'):
                self.activate()
            self.assertEqual(self.calls, [])
            self.assertFalse(self.plist.exists())
            self.assertEqual(self.agent_plist.read_bytes(), self.old_plist)
        finally:
            os.close(descriptor)
        self.activate()
        self.assertIn(('system', m.LABEL), self.jobs)

    def test_expired_sudo_after_snapshot_can_reauthorize_during_rollback(self):
        self.agent_plist.unlink()
        self.jobs.remove(('gui/501', m.LABEL))
        self.jobs.add(('system', m.LABEL))
        old_daemon = plistlib.dumps({'Label': m.LABEL, 'ProgramArguments': ['old'], 'UserName': 'service'})
        self.plist.write_bytes(old_daemon)
        copytree = shutil.copytree
        run = self.run_fake
        expired = False
        prompts = []

        def slow_snapshot(source, dest, **kwargs):
            nonlocal expired
            result = copytree(source, dest, **kwargs)
            if Path(source) == self.data:
                self.assertNotIn(('system', m.LABEL), self.jobs)
                expired = True
            return result

        def run_with_expired_sudo(args, **kwargs):
            nonlocal expired
            if expired and str(args[0]) == '/usr/bin/sudo':
                self.assertEqual(args[1], '--')
                prompts.append(str(args[2]))
                if len(prompts) == 1:
                    raise subprocess.CalledProcessError(1, args)
                expired = False
                with self.assertRaisesRegex(m.DeploymentError, 'another system Ink activation'):
                    with m.system_activation_lock():
                        self.fail('rollback must still hold the system lock')
            return run(args, **kwargs)

        with patch.object(shutil, 'copytree', side_effect=slow_snapshot), \
             patch.object(self, 'run_fake', side_effect=run_with_expired_sudo):
            with self.assertRaises(subprocess.CalledProcessError):
                self.activate()

        self.assertEqual(prompts, ['/usr/bin/install', '/usr/bin/install'])
        self.assertEqual(self.plist.read_bytes(), old_daemon)
        self.assertIn(('system', m.LABEL), self.jobs)
        self.assertEqual(self.link.read_text(), 'old')
        self.assertEqual((self.data / 'state.json').read_bytes(), self.old_state)

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
        self.assertFalse(self.plist.exists())
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
        self.assertFalse(self.agent_plist.exists())

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

    def test_repeated_system_update(self):
        self.activate()
        self.activate()
        self.assertIn(('system', m.LABEL), self.jobs)
        self.assertTrue((self.release/'enabled').exists())

    def test_failed_system_update_restores_old_daemon_and_keeps_dormant_agent(self):
        self.jobs.discard(('gui/501', m.LABEL))
        self.jobs.add(('system', m.LABEL))
        old_daemon = plistlib.dumps({'Label': m.LABEL, 'ProgramArguments': ['old'], 'UserName': 'service'})
        self.plist.write_bytes(old_daemon)
        self.fail = True
        with self.assertRaises(m.DeploymentError):
            self.activate()
        self.assertEqual(self.plist.read_bytes(), old_daemon)
        self.assertEqual(self.agent_plist.read_bytes(), self.old_plist)
        self.assertNotIn(('gui/501', m.LABEL), self.jobs)
        self.assertIn(('system', m.LABEL), self.jobs)
        self.assertFalse((self.data / 'state.sqlite3').exists())

    def test_rollback_reloads_disabled_system_daemon_before_disabling(self):
        self.jobs.remove(('gui/501', m.LABEL))
        job = ('system', m.LABEL)
        self.jobs.add(job)
        self.disabled_jobs.add(job)
        old_daemon = plistlib.dumps({'Label': m.LABEL, 'ProgramArguments': ['old'], 'UserName': 'service'})
        self.plist.write_bytes(old_daemon)
        self.fail = True

        with self.assertRaisesRegex(m.DeploymentError, 'daemon App command exited'):
            self.activate()

        self.assertEqual(self.plist.read_bytes(), old_daemon)
        self.assertIn(job, self.jobs)
        self.assertIn(job, self.disabled_jobs)

    def test_rollback_reloads_disabled_legacy_agent_before_disabling(self):
        job = ('gui/501', m.LABEL)
        self.disabled_jobs.add(job)
        self.fail = True

        with self.assertRaisesRegex(m.DeploymentError, 'daemon App command exited'):
            self.activate()

        self.assertEqual(self.agent_plist.read_bytes(), self.old_plist)
        self.assertIn(job, self.jobs)
        self.assertIn(job, self.disabled_jobs)

    def test_only_non_secret_proxy_environment_is_allowed(self):
        env=m.parse_env(['HTTPS_PROXY=http://127.0.0.1:7897','NODE_OPTIONS=--use-env-proxy'])
        settings={**self.settings,'environment':env}
        self.assertEqual(m.ink_plist(self.release, settings, 'service')['EnvironmentVariables']['HTTPS_PROXY'],env['HTTPS_PROXY'])
        for entry in ('GH_TOKEN=secret','HTTPS_PROXY=http://user:secret@localhost:7897','NODE_OPTIONS=--require=/tmp/code.js'):
            with self.assertRaises(m.DeploymentError):
                m.parse_env([entry])

    def test_daemon_self_check_has_bounded_retries_before_gate(self):
        self.transient_failures=2
        self.activate()
        self.assertEqual(self.self_checks,3)
        self.assertTrue((self.release/'enabled').exists())

    def test_activation_waits_for_the_old_daemon_state_lock(self):
        with patch.object(m.fcntl, 'flock', side_effect=[None, BlockingIOError(), None]) as lock:
            self.activate()
        self.assertEqual(lock.call_count, 3)
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
        self.assertNotIn('--config-only', start.call_args.args[1])

    def test_runtime_forwards_explicit_config_only_mode(self):
        (self.release / 'deployment.json').write_text(json.dumps({'data': str(self.data)}))
        with patch.object(runtime, '__file__', str(self.release / 'runtime.py')), \
             patch.object(runtime.os, 'execve') as start:
            runtime.main(('--config-only',))
        start.assert_called_once()
        self.assertEqual(start.call_args.args[1][-3:], [
            '-poll-gate', str(self.release / 'enabled'), '--config-only',
        ])

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

    def test_no_proxy_rejects_non_host_text(self):
        self.assertEqual(m.parse_env(['NO_PROXY=localhost,127.0.0.1,.example.com,[::1]'])['NO_PROXY'],'localhost,127.0.0.1,.example.com,[::1]')
        for value in ('user:password@host','secret text','a'*300,'host,,other'):
            with self.assertRaises(m.DeploymentError):
                m.parse_env(['NO_PROXY='+value])


if __name__ == '__main__':
    unittest.main()
