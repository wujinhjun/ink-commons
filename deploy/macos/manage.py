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
DEFAULT_RELEASE_REPO = 'wujinhjun/ink-commons'
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


def bundle_revision(artifact):
    try:
        revision = (artifact / 'REVISION').read_text().strip()
    except (OSError, ValueError):
        raise DeploymentError('deployment bundle is missing a valid REVISION file') from None
    if not re.fullmatch(r'[0-9a-f]{40}', revision):
        raise DeploymentError('deployment bundle has an invalid revision')
    return revision


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


def validate_release_repo(repo):
    if not isinstance(repo, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+', repo):
        raise DeploymentError('release repository must be owner/repo')
    if repo.split('/')[1] in ('.', '..'):
        raise DeploymentError('invalid release repository name')
    return repo


def validate_service_path(path):
    if not isinstance(path, str) or any(ch in path for ch in ('\0', '\r', '\n')):
        raise DeploymentError('service PATH must contain only absolute directories')
    if not all(Path(directory).is_absolute() for directory in path.split(':')):
        raise DeploymentError('service PATH must contain only absolute directories')
    return path


def active_deployment(home, root, *, allow_legacy_entry=False):
    entry = root / 'ink'
    if not entry.exists() and not entry.is_symlink():
        return None
    if allow_legacy_entry and not entry.is_symlink() and entry.is_file() and os.access(entry, os.X_OK):
        return None
    try:
        binary = entry.resolve(strict=True)
        release = binary.parent
        if (not entry.is_symlink() or binary.name != 'ink' or not binary.is_file()
                or release.parent != root / 'releases'
                or not re.fullmatch(r'[0-9a-f]{40}', release.name)):
            raise ValueError
        settings = json.loads((release / 'deployment.json').read_text())
        if (not isinstance(settings, dict) or settings.get('home') != str(home)
                or settings.get('revision') != release.name):
            raise ValueError
    except (OSError, RuntimeError, ValueError):
        raise DeploymentError('active deployment metadata is invalid or unavailable') from None
    return release, settings


def stage(args, *, reuse=False):
    if os.geteuid() == 0:
        raise DeploymentError('stage must run as the service user, without sudo')
    if sys.version_info < (3, 11, 8):
        raise DeploymentError('Python 3.11.8+ required')
    artifact = Path(args.artifact_dir).expanduser() if args.artifact_dir else None
    bundled = artifact is not None and (artifact / 'REVISION').is_file()
    if bundled:
        revision = bundle_revision(artifact)
        if revision != args.revision:
            raise DeploymentError('deployment bundle revision does not match --revision')
    else:
        repo = Path(__file__).resolve().parents[2]
        revision = output(['git', '-C', repo, 'rev-parse', '--verify', '--end-of-options', args.revision + '^{commit}'])
    home = Path.home().resolve()
    extra_env = parse_env(args.env)
    root = home / '.local/lib/ink-commons'
    releases = root / 'releases'
    release = releases / revision
    if release.exists() and not reuse:
        raise DeploymentError('release already exists; select an unstaged revision or use set-release-repo to change the active source')
    config = json.loads(Path(args.config).expanduser().read_text())
    command_path = args.github_command or config.get('github_command')
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
    settings = {'revision': revision, 'home': str(home), 'python': sys.executable,
                'data': str(home / '.local/state/ink-commons'),
                'environment': extra_env}
    release_repo = getattr(args, 'release_repo', None)
    service_path = getattr(args, 'service_path', None)
    if release_repo is None or service_path is None:
        active = active_deployment(home, root, allow_legacy_entry=True)
        if active is not None:
            _, previous = active
            if release_repo is None and 'release_repo' in previous:
                release_repo = validate_release_repo(previous['release_repo'])
            if service_path is None and 'service_path' in previous:
                service_path = validate_service_path(previous['service_path'])
        elif (root / 'ink').exists() or releases.is_dir():
            print('No versioned Ink entry to inherit from. Omitted release repository and service PATH use defaults.',
                  file=sys.stderr)
    if release_repo is not None:
        settings['release_repo'] = validate_release_repo(release_repo)
    if service_path is not None:
        settings['service_path'] = validate_service_path(service_path)
    if getattr(args, 'credentials', None):
        settings['credentials'] = str(Path(args.credentials).expanduser().resolve())
    elif args.cc_config:
        if not args.project:
            raise DeploymentError('--cc-config requires an explicit --project')
        settings['cc_config'] = str(Path(args.cc_config).expanduser().resolve())
        settings['project'] = args.project
    env = credential_environment(settings)
    if config.get('cc_connect_url') and not env.get('INK_CC_CONNECT_TOKEN'):
        raise DeploymentError('configured cc-connect provider requires a webhook token')
    if release.exists():
        verify_staged_settings(release, config, settings)
        verify_artifact(artifact, release, revision, bundled)
        return release
    releases.mkdir(parents=True, exist_ok=True, mode=0o700)
    with tempfile.TemporaryDirectory(dir=releases) as temp:
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
        plist = LABEL + '.plist'
        if (entry.exists() or entry.is_symlink() or (SYSTEM_DIR / plist).exists()
                or (home / 'Library/LaunchAgents' / plist).exists()
                or loaded('system', LABEL) or loaded('gui/' + str(uid), LABEL)):
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


def upgrade():
    if os.geteuid() == 0:
        raise DeploymentError('upgrade must run as the service user, without sudo')
    root = Path.home().resolve() / '.local/lib/ink-commons'
    entry = root / 'ink'
    if not entry.is_symlink():
        raise DeploymentError('upgrade requires an existing Ink deployment')
    with deployment_lock(root):
        upgrade_system(root, entry)


def release_client(config, repo):
    prefix = config.get('github_read_args')
    if prefix is None:
        return [config['github_command'], '--profile', 'inspect', '-R', repo], []
    if not isinstance(prefix, list) or any(not isinstance(arg, str) or not arg or '\0' in arg for arg in prefix):
        raise DeploymentError('invalid github_read_args')
    return ([config['github_command'], *[arg.replace('{repo}', repo) for arg in prefix]],
            ['--repo', repo])


def set_release_repo(args):
    if os.geteuid() == 0:
        raise DeploymentError('set-release-repo must run as the service user, without sudo')
    repo = validate_release_repo(args.repo)
    home = Path.home().resolve()
    root = home / '.local/lib/ink-commons'
    with deployment_lock(root):
        active = active_deployment(home, root)
        if active is None:
            raise DeploymentError('set-release-repo requires an existing Ink deployment')
        release, settings = active
        config = json.loads((release / 'config.json').read_text())
        app, _ = release_client(config, repo)
        proxy_entries = [key + '=' + value for key, value in settings.get('environment', {}).items()]
        network_env = {**os.environ, **parse_env(proxy_entries)}
        tag = 'build-' + release.name
        try:
            published_tag = output([*app, 'api', f'repos/{repo}/releases/tags/{tag}',
                                    '--jq', '.tag_name'], env=network_env, stderr=subprocess.DEVNULL, timeout=30)
        except (OSError, subprocess.SubprocessError):
            raise DeploymentError('cannot read the installed revision release from the new repository') from None
        if published_tag != tag:
            raise DeploymentError('the new repository did not return the installed revision release')
        atomic(release / 'deployment.json', json.dumps({**settings, 'release_repo': repo}).encode())
    print('Release repository:', repo)


def verify_staged_settings(target, config, expected_settings):
    try:
        staged_config = json.loads((target / 'config.json').read_text())
        staged_settings = json.loads((target / 'deployment.json').read_text())
    except (OSError, ValueError):
        raise DeploymentError('staged release configuration is missing or invalid') from None
    if staged_config != config or staged_settings != expected_settings:
        raise DeploymentError('staged release configuration differs from the current deployment; '
                              'archive the staged release before retrying')


def upgrade_system(root, entry):
    releases = root / 'releases'
    current = entry.resolve().parent
    if current.parent != releases or not (current / 'deployment.json').is_file():
        raise DeploymentError('installed release is unavailable')
    settings = json.loads((current / 'deployment.json').read_text())
    config_path = current / 'config.json'
    config = json.loads(config_path.read_text())
    proxy_entries = [key + '=' + value for key, value in settings.get('environment', {}).items()]
    network_env = {**os.environ, **parse_env(proxy_entries)}
    repo = validate_release_repo(settings.get('release_repo', DEFAULT_RELEASE_REPO))
    app, release_scope = release_client(config, repo)
    revision = output([*app, 'api', f'repos/{repo}/git/ref/heads/main',
                       '--jq', '.object.sha'], env=network_env)
    if not re.fullmatch(r'[0-9a-f]{40}', revision):
        raise DeploymentError('GitHub did not return a valid main revision')
    target = releases / revision
    if (target == current and loaded('system', LABEL)
            and (SYSTEM_DIR / (LABEL + '.plist')).is_file()
            and not (Path(settings['home']) / 'Library/LaunchAgents' / (LABEL + '.plist')).exists()
            and not loaded('gui/' + str(os.geteuid()), LABEL)):
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
        verify_staged_settings(target, config, {**expected, 'revision': revision})

    assets = ('SHA256SUMS',) if reuse else (*BUNDLE_FILES, 'SHA256SUMS')
    with tempfile.TemporaryDirectory() as artifact:
        try:
            run([*app, 'release', 'download', 'build-' + revision, '--dir', artifact, *release_scope,
                 *[f'--pattern={name}' for name in assets]], env=network_env)
        except subprocess.CalledProcessError:
            raise DeploymentError('unable to download the main build; check the GitHub Actions publish job') from None
        if reuse:
            verify_artifact(Path(artifact), target, revision, bundled=True)
        else:
            if bundle_revision(Path(artifact)) != revision:
                raise DeploymentError('downloaded bundle does not match the main revision')
            stage(argparse.Namespace(
                revision=revision, artifact_dir=artifact, config=str(config_path),
                github_command=config['github_command'], cc_config=settings.get('cc_config'),
                project=settings.get('project'), credentials=settings.get('credentials'),
                env=proxy_entries, release_repo=settings.get('release_repo'),
                service_path=settings.get('service_path'),
            ))

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


def ink_plist(release, settings, user):
    data = Path(settings['data'])
    default_path = str(Path(settings['home']) / '.local/bin') + ':/opt/homebrew/bin:/usr/bin:/bin'
    env = {'HOME': settings['home'], 'PATH': validate_service_path(settings.get('service_path', default_path))}
    env.update(parse_env([key + '=' + value for key, value in settings.get('environment', {}).items()]))
    return {'Label': LABEL, 'UserName': user,
            'ProgramArguments': [settings['python'], str(release / 'runtime.py')],
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


def run_as_admin(args):
    # A snapshot or a health check can outlast the sudo timestamp.
    return run(['/usr/bin/sudo', '--', *args])


@contextmanager
def system_activation_lock():
    # The root-owned directory has a shared inode across service accounts.
    # Locking it avoids a user-writable or replaceable lock file.
    descriptor = os.open(SYSTEM_DIR, os.O_RDONLY | os.O_DIRECTORY)
    try:
        try:
            fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise DeploymentError('another system Ink activation is running') from None
        yield
    finally:
        os.close(descriptor)


def install_system_plist(path, source):
    run_as_admin(['/usr/bin/install', '-S', '-o', 'root', '-g', 'wheel', '-m', '0644', source, path])


def unload(domain, label, timeout=45):
    if not loaded(domain,label):
        return
    try:
        launchctl = run_as_admin if domain == 'system' else run
        launchctl(['/bin/launchctl', 'bootout', domain + '/' + label])
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


def restore_launchd_service(domain, path, was_loaded, was_disabled, *, settings_changed=False):
    restart = was_loaded and not loaded(domain, LABEL)
    launchctl = run_as_admin if domain == 'system' else run
    target = domain + '/' + LABEL
    try:
        if restart:
            if was_disabled:
                launchctl(['/bin/launchctl', 'enable', target])
            launchctl(['/bin/launchctl', 'bootstrap', domain, path])
    finally:
        # A loaded service can be disabled, but a disabled service cannot be bootstrapped.
        if was_disabled and (restart or settings_changed):
            launchctl(['/bin/launchctl', 'disable', target])


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
        raise DeploymentError('activate must run as the service user, without sudo')
    with system_activation_lock():
        activate_system(args, account)


def activate_system(args, account):
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
    gui = 'gui/' + str(account.pw_uid)
    path = SYSTEM_DIR / (LABEL + '.plist')
    agent_path = home / 'Library/LaunchAgents' / path.name
    config = json.loads((release / 'config.json').read_text())
    if not config.get('github_command') or not Path(config['github_command']).is_file():
        raise DeploymentError('release has no App CLI')
    if not (release / 'ink').is_file() or not Path(settings['python']).is_file():
        raise DeploymentError('release runtime unavailable')
    check_provider_endpoints(config)

    old_loaded = loaded('system', LABEL)
    agent_loaded = loaded(gui, LABEL)
    old_plist = path.read_bytes() if path.exists() else None
    agent_plist = agent_path.read_bytes() if agent_path.exists() else None
    if (old_loaded and old_plist is None) or (agent_loaded and agent_plist is None):
        raise DeploymentError('loaded Ink service has no restorable plist')
    if old_plist is not None and plistlib.loads(old_plist).get('UserName') != args.user:
        raise DeploymentError('system Ink service belongs to a different user')
    if old_loaded and agent_loaded:
        raise DeploymentError('both system and login Ink services are loaded')
    agent_mode = stat.S_IMODE(agent_path.stat().st_mode) if agent_plist is not None else 0o600
    old_disabled = disabled('system', LABEL)
    agent_disabled = agent_loaded and disabled(gui, LABEL)
    try:
        run(['/usr/bin/sudo', '-v'])
    except subprocess.CalledProcessError:
        raise DeploymentError('administrator authorization is required to activate the LaunchDaemon') from None
    backup = root / 'backups' / str(time.time_ns())
    backup.mkdir(parents=True, mode=0o700)
    print('Backup:', backup, flush=True)
    entry = root / 'ink'
    old_link = os.readlink(entry) if entry.is_symlink() else None
    if old_link is not None:
        (backup / 'ink').symlink_to(old_link)
    elif entry.exists():
        shutil.copy2(entry, backup / 'ink')
    system_backup = backup / 'system' / path.name
    if old_plist is not None:
        atomic(system_backup, old_plist)
    if agent_plist is not None:
        atomic(backup / 'login' / agent_path.name, agent_plist)
    candidate_plist = backup / 'candidate' / path.name
    atomic(candidate_plist, plistlib.dumps(ink_plist(release, settings, args.user)))
    gate = release / 'enabled'
    gate_was_enabled = gate.exists()
    snapshot = backup / 'state'
    candidate_started = False
    system_changed = False
    agent_removed = False
    phase = 'stop_old_service'
    try:
        if agent_plist is not None:
            agent_path.unlink()
            agent_removed = True
            unload(gui, LABEL)
        unload('system', LABEL)
        phase = 'snapshot'
        with open(data / 'state.json.lock', 'a') as lock:
            os.fchmod(lock.fileno(), 0o600)
            acquire_state_lock(lock)
            shutil.copytree(data, snapshot, ignore=shutil.ignore_patterns('ink.sock', 'state.json.lock', '*.log'))
        phase = 'start_candidate'
        gate.unlink(missing_ok=True)
        system_changed = True
        install_system_plist(path, candidate_plist)
        run_as_admin(['/bin/launchctl', 'enable', 'system/' + LABEL])
        candidate_started = True
        run_as_admin(['/bin/launchctl', 'bootstrap', 'system', path])
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
                unload('system', LABEL)
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
            if system_changed:
                if old_plist is None:
                    run_as_admin(['/bin/rm', '-f', path])
                else:
                    install_system_plist(path, system_backup)
            if gate_was_enabled:
                atomic(gate, b'ready\n')
            if agent_removed:
                atomic(agent_path, agent_plist, agent_mode)
            restore_launchd_service('system', path, old_loaded, old_disabled,
                                    settings_changed=system_changed)
            restore_launchd_service(gui, agent_path, agent_loaded, agent_disabled)
        except Exception as recovery_error:
            print(json.dumps({'deployment': 'rollback_incomplete', 'phase': phase,
                              'revision': release.name, 'backup': str(backup)}), flush=True)
            raise DeploymentError('recovery incomplete; preserve the backup and state') from recovery_error
        print(json.dumps({'deployment': 'rolled_back', 'phase': phase,
                          'revision': release.name, 'backup': str(backup)}), flush=True)
        raise
    print('Activated revision:', release.name, 'gate: open')


def activate_from_cli(args):
    if os.geteuid() == 0:
        raise DeploymentError('activate must run as the service user, without sudo')
    with deployment_lock(Path.home().resolve() / '.local/lib/ink-commons'):
        activate(args)


def status(args):
    account = pwd.getpwnam(args.user)
    home = Path(account.pw_dir).resolve()
    data = home / '.local/state/ink-commons'
    for path in (SYSTEM_DIR / (LABEL+'.plist'),home/'Library/LaunchAgents'/(LABEL+'.plist')):
        if path.is_file():
            args_list = plistlib.loads(path.read_bytes()).get('ProgramArguments',[])
            if args_list and args_list[-1].endswith('/runtime.py'):
                manifest = Path(args_list[-1]).parent/'deployment.json'
                if manifest.is_file():
                    data = Path(json.loads(manifest.read_text())['data'])
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
    try:
        report['runtime'] = health(data)
    except (OSError, ValueError, IndexError):
        report['runtime'] = {'status': 'unavailable'}
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
        prepare.add_argument('--config', default='~/.config/ink-commons/config.json')
        prepare.add_argument('--credentials', help='private JSON file for configured provider tokens')
        prepare.add_argument('--cc-config', help='explicit legacy TOML credential source')
        prepare.add_argument('--project', help='project in the legacy TOML credential source')
        prepare.add_argument('--github-command', help='absolute GitHub reader CLI path')
        prepare.add_argument('--release-repo', help='owner/repo for upgrades (inherits active setting, else ' + DEFAULT_RELEASE_REPO + ')')
        prepare.add_argument('--service-path', help='absolute LaunchDaemon PATH entries separated by colons (inherits active setting)')
    commands.add_parser('upgrade', help='download and activate the verified main build')
    source = commands.add_parser('set-release-repo', help='change the release source for the installed revision')
    source.add_argument('repo', metavar='owner/repo')
    apply = commands.add_parser('activate')
    apply.add_argument('--release', required=True)
    apply.add_argument('--user', required=True)
    inspect = commands.add_parser('status')
    inspect.add_argument('--user', required=True)
    args = parser.parse_args()
    if sys.version_info < (3,11,8):
        parser.error('Python 3.11.8+ required for all commands')
    if sys.platform != 'darwin':
        parser.error('macOS only')
    try:
        if args.action == 'upgrade':
            upgrade()
        else:
            {'stage': stage, 'deploy': deploy, 'activate': activate_from_cli,
             'set-release-repo': set_release_repo, 'status': status}[args.action](args)
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
