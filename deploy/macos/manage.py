#!/usr/bin/env python3
"""版本化构建与 launchd 迁移。stage 不改服务；activate 才停止、备份和切换。"""
import argparse
from contextlib import contextmanager
import fcntl
import io
import hashlib
import re
import stat
import json
import os
from pathlib import Path
import plistlib
import pwd
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time

LABEL = 'com.ink-commons.daemon'
SYSTEM_DIR = Path('/Library/LaunchDaemons')
REPOSITORY = 'wujinhjun/ink-commons'
BUNDLE_FILES = ('ink', 'manage.py', 'runtime.py', 'config.example.json', 'REVISION')


class DeploymentError(Exception):
    """只包含脚本编写的安全诊断，不包含配置原文或子进程输出。"""


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, **kwargs)


def output(args, **kwargs):
    return run(args, stdout=subprocess.PIPE, **kwargs).stdout.decode().strip()


def atomic(path, content, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, name = tempfile.mkstemp(prefix=path.name + '.', dir=path.parent)
    try:
        with os.fdopen(descriptor, 'wb') as stream:
            os.fchmod(stream.fileno(), mode)
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
    finally:
        Path(name).unlink(missing_ok=True)


def parse_env(entries):
    from urllib.parse import urlsplit
    allowed = {'HTTP_PROXY','HTTPS_PROXY','ALL_PROXY','NO_PROXY','NODE_OPTIONS'}
    result = {}
    for entry in entries:
        key, separator, value = entry.partition('=')
        if not separator or key not in allowed or '\n' in value or '\r' in value:
            raise DeploymentError('only non-secret proxy environment is supported')
        if key == 'NODE_OPTIONS' and value != '--use-env-proxy':
            raise DeploymentError('only NODE_OPTIONS=--use-env-proxy is supported')
        if key == 'NO_PROXY':
            if len(value)>2048 or any(not re.fullmatch(r'(?:\*|\*?\.?[A-Za-z0-9][A-Za-z0-9.-]{0,252}|\[[0-9a-fA-F:]+\])(?::[0-9]{1,5})?',host) or any(len(label)>63 for label in host.split('.')) for host in value.split(',')):
                raise DeploymentError('NO_PROXY must be a bounded comma-separated host list without spaces or credentials')
        if key.endswith('_PROXY') and key != 'NO_PROXY':
            parsed = urlsplit(value)
            if parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.scheme not in ('http','https','socks5','socks5h') or not parsed.hostname:
                raise DeploymentError('proxy must be a URL without credentials, query or fragment')
        result[key] = value
    return result


def disabled(domain,label):
    result = subprocess.run(['/bin/launchctl','print-disabled',domain],capture_output=True)
    if result.returncode:
        return False  # 尚不存在的 GUI domain 没有已加载服务。
    pattern = r'"' + re.escape(label) + r'"\s*=>\s*(?:true|disabled)\b'
    return re.search(pattern,result.stdout.decode()) is not None


def acquire_state_lock(stream,timeout=5):
    deadline=time.monotonic()+timeout
    while True:
        try:
            fcntl.flock(stream.fileno(),fcntl.LOCK_EX|fcntl.LOCK_NB)
            return
        except BlockingIOError:
            if time.monotonic() >= deadline:
                raise DeploymentError('state lock remains held; refusing to copy or overwrite the database')
            time.sleep(0.1)


def read_profile(path):
    if not path:
        return {}
    profile = json.loads(Path(path).expanduser().read_text())
    fields = {'repository', 'service_user', 'python',
              'github_command', 'path', 'credentials', 'cc_version_file',
              'environment', 'retention', 'project'}
    if not isinstance(profile, dict) or profile.keys() - fields:
        raise DeploymentError('unknown deployment profile setting')
    for key in fields - {'path', 'environment', 'retention'}:
        if key in profile and (not isinstance(profile[key], str) or not profile[key]):
            raise DeploymentError('invalid deployment profile field: ' + key)
    if 'repository' in profile and not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*', profile['repository'], re.ASCII):
        raise DeploymentError('deployment repository must be owner/repo')
    if 'service_user' in profile and not re.fullmatch(r'[a-zA-Z0-9_.-]+', profile['service_user']):
        raise DeploymentError('invalid deployment profile service user')
    for key in ('python', 'github_command', 'credentials', 'cc_version_file'):
        if key in profile and not Path(profile[key]).is_absolute():
            raise DeploymentError('deployment profile paths must be absolute: ' + key)
    if 'path' in profile and (not isinstance(profile['path'], list) or not profile['path'] or
                             any(not isinstance(item, str) or ':' in item or not Path(item).is_absolute()
                                 for item in profile['path'])):
        raise DeploymentError('deployment profile path must be a list of absolute directories')
    environment = profile.get('environment', {})
    if not isinstance(environment, dict) or any(not isinstance(value, str) for value in environment.values()):
        raise DeploymentError('invalid deployment profile environment')
    parse_env([key + '=' + value for key, value in environment.items()])
    retention = profile.get('retention', {})
    if not isinstance(retention, dict) or retention.keys() - {'releases', 'backups', 'logs', 'stderr_bytes'}:
        raise DeploymentError('invalid retention settings')
    if any(type(value) is not int or value < 1 for value in retention.values()):
        raise DeploymentError('retention values must be positive integers')
    return profile


def bundle_revision(artifact):
    try:
        revision = (artifact / 'REVISION').read_text().strip()
    except (OSError, ValueError):
        raise DeploymentError('deployment bundle is missing a valid REVISION file') from None
    if not re.fullmatch(r'[0-9a-f]{40}', revision):
        raise DeploymentError('deployment bundle has an invalid revision')
    return revision


def verify_signature(artifact, allowed_signers):
    if not allowed_signers:
        return
    manifest = artifact / 'SHA256SUMS'
    try:
        with manifest.open('rb') as stream:
            run(['/usr/bin/ssh-keygen', '-Y', 'verify', '-f', allowed_signers,
                 '-I', 'ink-release', '-n', 'ink-commons', '-s', artifact / 'SHA256SUMS.sig'],
                stdin=stream, capture_output=True)
    except (OSError, subprocess.CalledProcessError):
        raise DeploymentError('release checksum signature verification failed') from None


def verify_artifact(artifact, release, revision, bundled):
    names = BUNDLE_FILES if bundled else ('ink',)
    checksums = {}
    for line in (artifact / 'SHA256SUMS').read_text().splitlines():
        match = re.fullmatch(r'([0-9a-f]{64})  (.+)', line)
        if match is None or match[2] not in names or match[2] in checksums:
            raise DeploymentError('invalid ink artifact checksum file')
        checksums[match[2]] = match[1]
    if set(checksums) != set(names):
        raise DeploymentError('incomplete ink artifact checksum file')
    for name in names:
        target = release / name
        if not target.is_file() or target.is_symlink():
            raise DeploymentError('ink artifact file is missing or invalid: ' + name)
        with target.open('rb') as stream:
            if hashlib.file_digest(stream, 'sha256').hexdigest() != checksums[name]:
                raise DeploymentError('ink artifact checksum mismatch: ' + name)
    if bundled and bundle_revision(release) != revision:
        raise DeploymentError('deployment bundle revision does not match --revision')
    verify_revision(release / 'ink', revision)


def verify_revision(binary, revision):
    try:
        result = run([binary, 'version'], capture_output=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        raise DeploymentError('ink artifact version could not be read on this Mac') from None
    if result.stdout.strip() != revision.encode():
        raise DeploymentError('ink artifact revision does not match --revision')


def config_drift(data, declaration):
    if not declaration:
        return {'status': 'not_configured'}
    expected = json.loads(Path(declaration).read_text())
    if not isinstance(expected, dict) or expected.get('schema') != 1 or not all(
            isinstance(expected.get(key), dict) for key in ('global', 'repositories')):
        raise DeploymentError('invalid runtime configuration declaration')
    try:
        actual = local_get(data, '/v1/config/export', timeout=2)
        if not isinstance(actual, dict) or actual.get('schema') != 1:
            return {'status': 'unavailable'}
    except (OSError, ValueError, IndexError, DeploymentError):
        return {'status': 'unavailable'}
    scopes = sorted(set(expected['repositories']) | set(actual['repositories']))
    changed = [scope for scope in scopes
               if expected['repositories'].get(scope) != actual['repositories'].get(scope)]
    if expected['global'] != actual['global']:
        changed.insert(0, 'global')
    return {'status': 'changed' if changed else 'match', 'scopes': changed}


def stage(args, *, reuse=False):
    if os.geteuid() == 0:
        raise DeploymentError('stage must run as the service user, without sudo')
    if sys.version_info < (3, 11, 8):
        raise DeploymentError('Python 3.11.8+ required')
    artifact = Path(args.artifact_dir).expanduser() if args.artifact_dir else None
    bundled = artifact is not None and (artifact / 'REVISION').is_file()
    allowed_signers = getattr(args, 'allowed_signers', None)
    if allowed_signers:
        allowed_signers = str(Path(allowed_signers).expanduser().resolve())
        if not bundled:
            raise DeploymentError('signature verification requires a complete release bundle')
        verify_signature(artifact, allowed_signers)
    if bundled:
        revision = bundle_revision(artifact)
        if revision != args.revision:
            raise DeploymentError('deployment bundle revision does not match --revision')
    else:
        repo = Path(__file__).resolve().parents[2]
        revision = output(['git', '-C', repo, 'rev-parse', '--verify', '--end-of-options', args.revision + '^{commit}'])
    home = Path.home().resolve()
    profile_path = getattr(args, 'profile', None)
    profile = read_profile(profile_path)
    if 'service_user' in profile and profile['service_user'] != pwd.getpwuid(os.geteuid()).pw_name:
        raise DeploymentError('profile service_user must match the deployment user')
    extra_env = {**profile.get('environment', {}), **parse_env(args.env)}
    root = home / '.local/lib/ink-commons/releases'
    release = root / revision
    if release.exists() and not reuse:
        raise DeploymentError('release already exists; reuse it or explicitly archive it before restaging')
    config_source = Path(args.config).expanduser().resolve()
    config = json.loads(config_source.read_text())
    legacy = sorted(config.keys() & {'repositories', 'repository_defaults', 'execution'})
    if legacy:
        print('Legacy SQLite first-import fields:', ', '.join(legacy))
    command_path = args.github_command or profile.get('github_command') or config.get('github_command')
    if not command_path:
        raise DeploymentError('configure an absolute GitHub reader CLI path')
    command = Path(command_path).expanduser()
    if not command.is_absolute():
        raise DeploymentError('configure an absolute GitHub reader CLI path')
    command = command.resolve()
    if not command.is_file() or not os.access(command, os.X_OK):
        raise DeploymentError('App CLI is unavailable')
    config['github_command'] = str(command)
    # 凭据和路径检查先于构建；永远不输出 cc-connect 生成配置。
    sys.path.insert(0, str(Path(__file__).parent))
    from runtime import credential_environment
    python = profile.get('python', sys.executable)
    if not Path(python).is_file() or not os.access(python, os.X_OK):
        raise DeploymentError('deployment Python is unavailable')
    settings = {'revision': revision, 'home': str(home), 'python': python,
                'data': str(home / '.local/state/ink-commons'),
                'environment': extra_env,
                'config_source': str(config_source)}
    if profile_path:
        settings['profile_path'] = str(Path(profile_path).expanduser().resolve())
        settings['profile'] = profile
    declaration = getattr(args, 'runtime_declaration', None)
    if declaration:
        settings['runtime_declaration'] = str(Path(declaration).expanduser().resolve())
        print(json.dumps({'config_drift': config_drift(settings['data'], settings['runtime_declaration'])}))
    credentials = getattr(args, 'credentials', None) or profile.get('credentials')
    if credentials:
        settings['credentials'] = str(Path(credentials).expanduser().resolve())
    elif args.cc_config:
        project = args.project or profile.get('project')
        if not project:
            raise DeploymentError('--cc-config requires an explicit --project')
        settings['cc_config'] = str(Path(args.cc_config).expanduser().resolve())
        settings['project'] = project
    if allowed_signers:
        settings['allowed_signers'] = allowed_signers
    env = credential_environment(settings)
    if config.get('cc_connect_url') and not env.get('INK_CC_CONNECT_TOKEN'):
        raise DeploymentError('configured cc-connect provider requires a webhook token')
    if release.exists():
        verify_staged_settings(release, config, settings)
        verify_artifact(artifact, release, revision, bundled)
        return release
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with tempfile.TemporaryDirectory(dir=root) as temp:
        temp = Path(temp)
        candidate = temp / 'release'
        candidate.mkdir(mode=0o700)
        if artifact is not None:
            for name in (BUNDLE_FILES if bundled else ('ink',)):
                shutil.copyfile(artifact / name, candidate / name)
            (candidate / 'ink').chmod(0o700)
            verify_artifact(artifact, candidate, revision, bundled)
            if not bundled:
                for name in ('runtime.py', 'manage.py'):
                    script = run(['git', '-C', repo, 'show', revision + ':deploy/macos/' + name],
                                 stdout=subprocess.PIPE).stdout
                    (candidate / name).write_bytes(script)
        else:
            source = temp / 'source'
            source.mkdir()
            archive = run(['git', '-C', repo, 'archive', revision], stdout=subprocess.PIPE).stdout
            with tarfile.open(fileobj=io.BytesIO(archive)) as bundle:
                bundle.extractall(source, filter='data')
            run(['go', 'build', '-trimpath', '-ldflags',
                 '-X ink-commons/internal/ink.BuildRevision=' + revision,
                 '-o', candidate / 'ink', './cmd/ink'], cwd=source)
            for name in ('runtime.py', 'manage.py'):
                shutil.copy2(source / 'deploy/macos' / name, candidate / name)
        atomic(candidate / 'config.json', json.dumps(config).encode())
        atomic(candidate / 'deployment.json', json.dumps(settings).encode())
        os.rename(candidate, release)
    print('Staged:', release)
    print('No running service was changed.')
    return release


def deploy(args):
    uid = os.geteuid()
    if uid == 0:
        raise DeploymentError('deploy must run as the service user, without sudo')
    home = Path.home().resolve()
    root = home / '.local/lib/ink-commons'
    with deployment_lock(root):
        entry = root / 'ink'
        gui = 'gui/' + str(uid)
        if entry.exists() or entry.is_symlink() or loaded('system', LABEL) or loaded(gui, LABEL):
            raise DeploymentError('an Ink deployment already exists; use upgrade')
        artifact = Path(__file__).resolve().parent
        args.revision = bundle_revision(artifact)
        args.artifact_dir = str(artifact)
        data = home / '.local/state/ink-commons'
        data.mkdir(parents=True, exist_ok=True, mode=0o700)
        validate_state_directory(data, home, uid)
        release = stage(args, reuse=True)
        activate_staged(release)


@contextmanager
def deployment_lock(root):
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with (root / 'upgrade.lock').open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise DeploymentError('another deployment is running') from None
        yield


def upgrade(args):
    if os.geteuid() == 0:
        raise DeploymentError('upgrade must run as the service user, without sudo')
    root = Path.home().resolve() / '.local/lib/ink-commons'
    entry = root / 'ink'
    if loaded('system', LABEL) or (SYSTEM_DIR / (LABEL + '.plist')).exists():
        raise DeploymentError('legacy system deployment detected; migrate to the login service before upgrading')
    if not entry.is_symlink():
        raise DeploymentError('upgrade requires an existing login deployment')
    if args.dry_run:
        upgrade_system(root, entry, tag=args.tag, channel=args.channel, dry_run=True)
        return
    with deployment_lock(root):
        upgrade_system(root, entry, tag=args.tag, channel=args.channel, apply=args.apply)


def verify_staged_settings(target, config, expected_settings):
    try:
        staged_config = json.loads((target / 'config.json').read_text())
        staged_settings = json.loads((target / 'deployment.json').read_text())
    except (OSError, ValueError):
        raise DeploymentError('staged release configuration is missing or invalid') from None
    if staged_config != config or staged_settings != expected_settings:
        raise DeploymentError('staged release configuration differs from the current deployment; '
                              'archive the staged release before retrying')


def select_release(app, network_env, tag, channel, repository=REPOSITORY):
    if tag is None:
        if channel == 'preview':
            tag = output([*app, 'api', f'repos/{repository}/releases',
                          '--jq', '[.[] | select(.prerelease and (.draft | not))][0].tag_name // ""'],
                         env=network_env)
        else:
            tag = output([*app, 'api', f'repos/{repository}/releases/latest',
                          '--jq', '.tag_name'], env=network_env)
    match = re.fullmatch(r'build-([0-9a-f]{40})', tag)
    if match is None:
        raise DeploymentError('no deployment release found; specify a published build-<SHA> tag')
    return tag, match[1]


def upgrade_system(root, entry, *, tag=None, channel=None, dry_run=False, apply=False):
    releases = root / 'releases'
    current = entry.resolve().parent
    if current.parent != releases or not (current / 'deployment.json').is_file():
        raise DeploymentError('installed release is unavailable')
    settings = json.loads((current / 'deployment.json').read_text())
    active_config_path = current / 'config.json'
    config = json.loads(active_config_path.read_text())
    config_path = Path(settings.get('config_source') or active_config_path)
    proxy_entries = [key + '=' + value for key, value in settings.get('environment', {}).items()]
    network_env = {**os.environ, **parse_env(proxy_entries)}
    profile = settings.get('profile', {})
    repository = profile.get('repository', REPOSITORY)
    if profile.get('path'):
        network_env['PATH'] = ':'.join(profile['path'])
    app = [config['github_command'], '--profile', 'inspect', '-R', repository]
    tag, revision = select_release(app, network_env, tag, channel, repository)
    if dry_run:
        print(json.dumps({'current': current.name, 'tag': tag, 'revision': revision,
                          'staged': (releases / revision).is_dir()}))
        return
    target = releases / revision
    if target == current:
        try:
            running = health(settings['data'])
        except (OSError, ValueError, IndexError, DeploymentError):
            running = {}
        if isinstance(running, dict) and running.get('revision') == revision and running.get('gate') == 'open':
            print('Already running:', revision)
            return

    reuse = target.exists()
    if reuse and target != current:
        expected = {key: value for key, value in settings.items() if key != 'cc_binary_sha256'}
        desired_config = json.loads(config_path.read_text())
        desired_config['github_command'] = config['github_command']
        verify_staged_settings(target, desired_config,
                               {**expected, 'config_source': str(config_path), 'revision': revision})
        if settings.get('runtime_declaration'):
            print(json.dumps({'config_drift': config_drift(settings['data'], settings['runtime_declaration'])}))

    assets = ('SHA256SUMS',) if reuse else (*BUNDLE_FILES, 'SHA256SUMS')
    allowed_signers = settings.get('allowed_signers')
    if allowed_signers:
        assets += ('SHA256SUMS.sig',)
    with tempfile.TemporaryDirectory() as artifact:
        try:
            run([*app, 'release', 'download', tag, '--dir', artifact,
                 *[f'--pattern={name}' for name in assets]], env=network_env)
        except subprocess.CalledProcessError:
            raise DeploymentError('unable to download the main build; check the GitHub Actions publish job') from None
        if reuse:
            verify_signature(Path(artifact), allowed_signers)
            verify_artifact(Path(artifact), target, revision, bundled=True)
        else:
            if bundle_revision(Path(artifact)) != revision:
                raise DeploymentError('downloaded bundle does not match the main revision')
            stage(argparse.Namespace(
                revision=revision, artifact_dir=artifact, config=str(config_path),
                github_command=config['github_command'], cc_config=settings.get('cc_config'),
                project=settings.get('project'), credentials=settings.get('credentials'),
                env=proxy_entries, allowed_signers=allowed_signers,
                runtime_declaration=settings.get('runtime_declaration'), profile=settings.get('profile_path'),
            ))

    if not apply:
        print('Prepared:', target)
        return
    try:
        activate_staged(target)
    except (OSError, subprocess.CalledProcessError):
        if entry.resolve().parent == current:
            try:
                run([current / 'ink', 'init'])
            except (OSError, subprocess.CalledProcessError):
                print('Unable to restore the previous agent skill', file=sys.stderr)
        raise


def activate_staged(release):
    activate(argparse.Namespace(user=pwd.getpwuid(os.geteuid()).pw_name, release=release))


def ink_plist(release, settings):
    data = Path(settings['data'])
    env = {'HOME': settings['home'], 'PATH': os.defpath}
    if settings.get('profile', {}).get('path'):
        env['PATH'] = ':'.join(settings['profile']['path'])
    env.update(parse_env([key + '=' + value for key, value in settings.get('environment', {}).items()]))
    return {'Label': LABEL, 'ProgramArguments': [settings['python'], str(release / 'runtime.py')],
            'RunAtLoad': True, 'KeepAlive': True, 'ThrottleInterval': 30,
            'ExitTimeOut': 45, 'EnvironmentVariables': env,
            'StandardOutPath': str(data / 'stdout.log'),
            'StandardErrorPath': str(data / 'stderr.log')}


def check_provider_endpoints(config):
    from urllib.parse import urlsplit
    for key in ('cc_connect_url', 'http_provider_url'):
        if not config.get(key):
            continue
        url = urlsplit(config[key])
        if url.scheme not in ('http', 'https') or not url.hostname or url.username or url.password:
            raise DeploymentError('invalid configured provider endpoint: ' + key)
        try:
            with socket.create_connection((url.hostname, url.port or (443 if url.scheme == 'https' else 80)), timeout=2):
                pass
        except OSError:
            raise DeploymentError('configured provider endpoint unavailable: ' + key) from None


def local_get(data, path, timeout=25):
    with socket.socket(socket.AF_UNIX) as client:
        client.settimeout(timeout)
        client.connect(str(Path(data) / 'ink.sock'))
        client.sendall(('GET ' + path + ' HTTP/1.0\r\nHost: ink\r\n\r\n').encode())
        chunks = []
        while True:
            part = client.recv(65536)
            if not part:
                break
            chunks.append(part)
    head,body = b''.join(chunks).split(b'\r\n\r\n', 1)
    report = json.loads(body)
    if head.split(b' ',2)[1] != b'200':
        raise DeploymentError('daemon self-check failed: repository=' + report.get('repository','unknown') + '; ' + report.get('error','unavailable'))
    return report


def health(data):
    return local_get(data, '/healthz', timeout=2)


def loaded(domain, label):
    return subprocess.run(['/bin/launchctl', 'print', domain + '/' + label],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def unload(domain, label, timeout=45):
    if not loaded(domain,label):
        return
    try:
        run(['/bin/launchctl','bootout',domain+'/'+label])
    except subprocess.CalledProcessError:
        # 服务也可能在状态读取与 bootout 之间自行退出。
        if not loaded(domain,label):
            return
        raise
    # bootout 返回时 job 仍可能处于退出阶段；等待移除后才能快照或 bootstrap。
    deadline=time.monotonic()+timeout
    while loaded(domain,label):
        if time.monotonic() >= deadline:
            raise DeploymentError('service did not stop before deadline: '+label)
        time.sleep(0.1)


def validate_state_directory(data, home, uid):
    if data != home / '.local/state/ink-commons' or not data.is_dir() or data.is_symlink():
        raise DeploymentError('existing private state directory required')
    if data.stat().st_uid != uid or data.stat().st_mode & 0o077:
        raise DeploymentError('state directory must be private and owned by service user')


def switch_entry(entry, target):
    with tempfile.TemporaryDirectory(dir=entry.parent) as temporary:
        link = Path(temporary) / 'ink'
        link.symlink_to(target)
        os.replace(link, entry)


def activate(args):
    account = pwd.getpwnam(args.user)
    if os.geteuid() == 0 or os.geteuid() != account.pw_uid:
        raise DeploymentError('activate must run as the non-root login service user')
    home = Path(account.pw_dir).resolve()
    root = home / '.local/lib/ink-commons'
    release = Path(args.release).resolve()
    if release.parent != root / 'releases':
        raise DeploymentError('release must be in the service user release directory')
    settings = json.loads((release / 'deployment.json').read_text())
    if settings['home'] != str(home) or settings.get('revision') != release.name:
        raise DeploymentError('release home or revision differs from the deployment manifest')
    data = Path(settings['data'])
    validate_state_directory(data, home, account.pw_uid)
    if loaded('system', LABEL) or (SYSTEM_DIR / (LABEL + '.plist')).exists():
        raise DeploymentError('legacy system service must be migrated separately')
    domain = 'gui/' + str(account.pw_uid)
    run(['/bin/launchctl', 'print', domain], stdout=subprocess.DEVNULL)
    path = home / 'Library/LaunchAgents' / (LABEL + '.plist')
    config = json.loads((release / 'config.json').read_text())
    if not config.get('github_command') or not Path(config['github_command']).is_file():
        raise DeploymentError('release has no App CLI')
    if not (release / 'ink').is_file() or not Path(settings['python']).is_file():
        raise DeploymentError('release runtime unavailable')
    check_provider_endpoints(config)

    old_loaded = loaded(domain, LABEL)
    old_plist = path.read_bytes() if path.exists() else None
    if old_loaded and old_plist is None:
        raise DeploymentError('loaded Ink service has no restorable plist')
    old_mode = stat.S_IMODE(path.stat().st_mode) if old_plist is not None else 0o600
    old_disabled = disabled(domain, LABEL)
    backup = root / 'backups' / str(time.time_ns())
    backup.mkdir(parents=True, mode=0o700)
    print('Backup:', backup, flush=True)
    entry = root / 'ink'
    old_link = os.readlink(entry) if entry.is_symlink() else None
    if old_link is not None:
        (backup / 'ink').symlink_to(old_link)
    elif entry.exists():
        shutil.copy2(entry, backup / 'ink')
    if old_plist is not None:
        atomic(backup / path.name, old_plist)
    gate = release / 'enabled'
    gate_was_enabled = gate.exists()
    snapshot = backup / 'state'
    candidate_started = False
    phase = 'stop_old_service'
    try:
        unload(domain, LABEL)
        rotate_stderr(data, settings.get('profile', {}).get('retention', {}).get('stderr_bytes', 8 << 20))
        phase = 'snapshot'
        with open(data / 'state.json.lock', 'a') as lock:
            os.fchmod(lock.fileno(), 0o600)
            acquire_state_lock(lock)
            shutil.copytree(data, snapshot, ignore=shutil.ignore_patterns('ink.sock', 'state.json.lock', '*.log'))
        phase = 'start_candidate'
        gate.unlink(missing_ok=True)
        atomic(path, plistlib.dumps(ink_plist(release, settings)))
        run(['/bin/launchctl', 'enable', domain + '/' + LABEL])
        candidate_started = True
        run(['/bin/launchctl', 'bootstrap', domain, path])
        phase = 'verify_revision'
        for _ in range(30):
            try:
                report = health(data)
                if report.get('revision') == settings['revision'] and report.get('gate') == 'closed':
                    break
            except (OSError, ValueError, IndexError):
                pass
            time.sleep(1)
        else:
            raise DeploymentError('running revision did not match staged revision')
        check_provider_endpoints(config)
        phase = 'daemon_self_check'
        for attempt in range(3):
            try:
                local_get(data, '/v1/self-check')
                break
            except (OSError, DeploymentError):
                if attempt == 2:
                    raise
                time.sleep(1)
        switch_entry(entry, release / 'ink')
        atomic(gate, b'ready\n')
    except (Exception, KeyboardInterrupt):
        try:
            if candidate_started:
                gate.unlink(missing_ok=True)
                unload(domain, LABEL)
                with open(data / 'state.json.lock', 'a') as lock:
                    acquire_state_lock(lock)
                    for item in data.iterdir():
                        if item.name not in ('ink.sock', 'state.json.lock') and not item.name.endswith('.log'):
                            if item.is_dir() and not item.is_symlink():
                                shutil.rmtree(item)
                            else:
                                item.unlink()
                    shutil.copytree(snapshot, data, dirs_exist_ok=True)
                if old_link is not None:
                    switch_entry(entry, old_link)
                elif (backup / 'ink').is_file():
                    atomic(entry, (backup / 'ink').read_bytes(), stat.S_IMODE((backup / 'ink').stat().st_mode))
                else:
                    entry.unlink(missing_ok=True)
            if old_plist is None:
                path.unlink(missing_ok=True)
            else:
                atomic(path, old_plist, old_mode)
            run(['/bin/launchctl', 'disable' if old_disabled else 'enable', domain + '/' + LABEL])
            if gate_was_enabled:
                atomic(gate, b'ready\n')
            if old_loaded and not loaded(domain, LABEL):
                run(['/bin/launchctl', 'bootstrap', domain, path])
        except Exception as recovery_error:
            print(json.dumps({'deployment': 'rollback_incomplete', 'phase': phase,
                              'revision': release.name, 'backup': str(backup)}), flush=True)
            raise DeploymentError('recovery incomplete; preserve the backup and state') from recovery_error
        print(json.dumps({'deployment': 'rolled_back', 'phase': phase,
                          'revision': release.name, 'backup': str(backup)}), flush=True)
        raise
    print('Activated revision:', release.name, 'gate: open')


def repository_status(data, entry, checkouts):
    try:
        repos = local_get(data, '/v1/repositories', timeout=2)
        work = local_get(data, '/v1/work', timeout=2)
    except (OSError, ValueError, IndexError, DeploymentError):
        return None
    linked = {item['repo'] for item in work if item.get('linked')}
    result = []
    for repo in sorted(item['repo'] for item in repos):
        hook = 'unknown'
        if repo in checkouts and entry.is_file():
            try:
                checkout = checkouts[repo]
                origin = output(['git', '-C', checkout, 'remote', 'get-url', 'origin'], stderr=subprocess.DEVNULL)
                if re.fullmatch(r'(?:https://github\.com/|git@github\.com:)' + re.escape(repo)
                                + r'(?:\.git)?/?', origin, re.IGNORECASE):
                    hook = output([entry, 'hook', 'status', '--checkout', checkout], stderr=subprocess.DEVNULL)
            except (OSError, subprocess.CalledProcessError):
                pass
        result.append({'repo': repo, 'hook': hook, 'linked_work': repo in linked})
    return result


def rotate_stderr(data, limit):
    log = data / 'stderr.log'
    if log.is_file() and not log.is_symlink() and log.stat().st_size >= limit:
        log.rename(data / ('stderr.log.' + str(time.time_ns())))


def retention_plan(home, root, retention, include_legacy):
    protected = set()
    entry = root / 'ink'
    if entry.is_symlink():
        active = entry.resolve()
        protected.add(active)
        manifest = active.parent / 'deployment.json'
        if manifest.is_file():
            settings = json.loads(manifest.read_text())
            for key in ('config_source', 'credentials', 'cc_config', 'profile_path',
                        'runtime_declaration', 'allowed_signers', 'python'):
                value = settings.get(key)
                if value:
                    protected.add(Path(value).resolve())
    for directory in (SYSTEM_DIR, home / 'Library/LaunchAgents'):
        for path in directory.glob('*.plist'):
            # Read structured paths only, and exclude any release referenced by a service.
            try:
                job = plistlib.loads(path.read_bytes())
                paths = [job.get('Program'), *job.get('ProgramArguments', [])]
            except (OSError, ValueError, plistlib.InvalidFileException):
                raise DeploymentError('cannot inspect a launchd plist for retention: ' + path.name) from None
            protected.update(Path(item).resolve() for item in paths
                             if isinstance(item, str) and Path(item).is_absolute())

    def eligible(entries, pattern, keep):
        candidates = sorted((path for path in entries if re.fullmatch(pattern, path.name)
                             and not path.is_symlink()),
                            key=lambda path: (path.stat().st_mtime_ns, path.name), reverse=True)
        return [path for path in candidates[keep:]
                if not any(path.resolve() == protected_path or path.resolve() in protected_path.parents
                           for protected_path in protected)]

    plan = eligible((root / 'releases').glob('*'), r'[0-9a-f]{40}', retention.get('releases', 5))
    plan += eligible((root / 'backups').glob('*'), r'[0-9]+', retention.get('backups', 5))
    plan += eligible((home / '.local/state/ink-commons').glob('stderr.log.*'),
                     r'stderr\.log\.[0-9]+', retention.get('logs', 5))
    if include_legacy:
        plan += eligible(root.glob('*'), r'start|activate-[a-zA-Z0-9._-]+\.command|staging-archive\..+', 0)
        plan += eligible((root / 'deploy').glob('*'), r'[0-9a-f]{40}', 0)
    return plan


def maintain(args):
    if os.geteuid() == 0:
        raise DeploymentError('maintenance must run as the deployment user')
    home = Path.home().resolve()
    root = home / '.local/lib/ink-commons'
    profile = read_profile(args.profile)
    if not profile and (root / 'ink').is_symlink():
        settings_path = (root / 'ink').resolve().parent / 'deployment.json'
        profile = json.loads(settings_path.read_text()).get('profile', {})
    retention = profile.get('retention', {})
    if not args.apply:
        print(json.dumps({'archive': list(map(str, retention_plan(home, root, retention, args.include_legacy)))}, indent=2))
        return
    with deployment_lock(root):
        plan = retention_plan(home, root, retention, args.include_legacy)
        if plan:
            archive = root / 'archive' / str(time.time_ns())
            for source in plan:
                target = archive / source.relative_to(home)
                target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
                source.rename(target)
            print('Archived:', archive)


def status(args):
    account = pwd.getpwnam(args.user)
    home = Path(account.pw_dir).resolve()
    data = home / '.local/state/ink-commons'
    active_settings = {}
    for path in (SYSTEM_DIR / (LABEL+'.plist'),home/'Library/LaunchAgents'/(LABEL+'.plist')):
        if path.is_file():
            args_list = plistlib.loads(path.read_bytes()).get('ProgramArguments',[])
            if args_list and args_list[-1].endswith('/runtime.py'):
                manifest = Path(args_list[-1]).parent/'deployment.json'
                if manifest.is_file():
                    active_settings = json.loads(manifest.read_text())
                    data = Path(active_settings['data'])
                    break
    config_path=home/'.config/ink-commons/config.json'
    if 'manifest' in locals() and manifest.is_file():
        config_path=manifest.parent/'config.json'
    config = json.loads(config_path.read_text()) if config_path.is_file() else {}
    report = {'user': args.user, 'services': {LABEL: {
        'system': loaded('system', LABEL),
        'login': loaded('gui/' + str(account.pw_uid), LABEL),
    }}, 'providers': {
        'cc-connect': bool(config.get('cc_connect_url')),
        'http': bool(config.get('http_provider_url')),
    }}
    checkouts = {}
    for mapping in getattr(args, 'checkout', []):
        repo, separator, checkout = mapping.partition('=')
        if not separator or not repo or not checkout:
            raise DeploymentError('checkout must have the form OWNER/REPO=DIR')
        checkouts[repo.lower()] = str(Path(checkout).expanduser().resolve())
    report['repositories'] = repository_status(data, home / '.local/lib/ink-commons/ink', checkouts)
    try:
        report['runtime'] = health(data)
    except (OSError, ValueError, IndexError):
        report['runtime'] = {'status': 'unavailable'}
    if 'manifest' in locals() and manifest.is_file():
        active_settings = json.loads(manifest.read_text())
        try:
            report['config_drift'] = config_drift(data, active_settings.get('runtime_declaration'))
        except (OSError, ValueError, DeploymentError):
            report['config_drift'] = {'status': 'invalid_declaration'}
    report['config_sha256'] = hashlib.sha256(config_path.read_bytes()).hexdigest() if config_path.is_file() else None
    profile = read_profile(getattr(args, 'profile', None)) or active_settings.get('profile', {})
    cc_version = None
    if profile.get('cc_version_file'):
        try:
            meta = json.loads(Path(profile['cc_version_file']).read_text())
            if isinstance(meta, dict):
                cc_version = {key: meta[key] for key in ('version', 'commit')
                              if isinstance(meta.get(key), str) and re.fullmatch(r'[a-zA-Z0-9.+_-]{1,100}', meta[key])}
        except (OSError, ValueError):
            pass
    if cc_version and report['providers']['cc-connect']:
        report['cc_connect_version'] = cc_version
    print(json.dumps(report, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='action', required=True)
    build = commands.add_parser('stage')
    build.add_argument('--revision', required=True)
    build.add_argument('--artifact-dir', help='extracted GitHub Actions artifact directory')
    install = commands.add_parser('deploy', help='install from a GitHub Release bundle')
    for prepare in (build, install):
        prepare.add_argument('--env', action='append', default=[], help='non-secret proxy environment KEY=VALUE')
        prepare.add_argument('--runtime-declaration', help='SQLite policy declaration for read-only drift reporting')
        prepare.add_argument('--config', default='~/.config/ink-commons/config.json')
        prepare.add_argument('--profile', help='validated deployment profile JSON')
        prepare.add_argument('--allowed-signers', help='OpenSSH allowed_signers trust file for release signatures')
        prepare.add_argument('--credentials', help='private JSON file for configured provider tokens')
        prepare.add_argument('--cc-config', help='explicit legacy TOML credential source')
        prepare.add_argument('--project', help='project in the legacy TOML credential source')
        prepare.add_argument('--github-command', help='absolute GitHub reader CLI path')
    update = commands.add_parser('upgrade', help='prepare a published release')
    selection = update.add_mutually_exclusive_group()
    selection.add_argument('--tag', help='published build-<SHA> tag')
    selection.add_argument('--channel', choices=('stable', 'preview'))
    update.add_argument('--dry-run', action='store_true')
    update.add_argument('--apply', action='store_true', help='explicitly activate the prepared release')
    rollback = commands.add_parser('rollback', help='select a prior release and preserve current SQLite state')
    rollback.add_argument('--tag', required=True)
    rollback.add_argument('--dry-run', action='store_true')
    rollback.set_defaults(apply=True, channel=None)
    apply = commands.add_parser('activate')
    apply.add_argument('--release', required=True)
    apply.add_argument('--user', required=True)
    inspect = commands.add_parser('status')
    inspect.add_argument('--user', required=True)
    inspect.add_argument('--profile')
    maintenance = commands.add_parser('maintain', help='plan or archive expired deployment files')
    maintenance.add_argument('--profile')
    maintenance.add_argument('--include-legacy', action='store_true')
    maintenance.add_argument('--apply', action='store_true')
    inspect.add_argument('--checkout', action='append', default=[], help='OWNER/REPO=DIR for managed hook inspection')
    args = parser.parse_args()
    if sys.version_info < (3,11,8):
        parser.error('Python 3.11.8+ required for all commands')
    if sys.platform != 'darwin':
        parser.error('macOS only')
    try:
        if args.action in ('upgrade', 'rollback'):
            upgrade(args)
        else:
            {'stage': stage, 'deploy': deploy, 'activate': activate, 'status': status, 'maintain': maintain}[args.action](args)
    except DeploymentError as error:
        print('Deployment failed:', str(error), file=sys.stderr)
        return 1
    except Exception as error:
        # 不转发 subprocess stdout / stderr 或配置解析原文。
        print('Deployment failed:', type(error).__name__, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
