#!/usr/bin/env python3
"""版本化构建与 launchd 迁移。stage 不改服务；activate 才停止、备份和切换。"""
import argparse
import fcntl
import io
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
CC_LABEL = 'com.cc-connect.service'
SYSTEM_DIR = Path('/Library/LaunchDaemons')


def run(args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, **kwargs)


def output(args):
    return run(args, stdout=subprocess.PIPE).stdout.decode().strip()


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


def stage(args):
    if os.geteuid() == 0:
        raise ValueError('stage must run as the service user, without sudo')
    if sys.version_info < (3, 11):
        raise ValueError('Python 3.11+ required')
    repo = Path(__file__).resolve().parents[2]
    revision = output(['git', '-C', repo, 'rev-parse', '--verify', args.revision + '^{commit}'])
    home = Path.home().resolve()
    root = home / '.local/lib/ink-commons/releases'
    release = root / revision
    if release.exists():
        raise ValueError('release already exists; reuse it or explicitly archive it before restaging')
    config = json.loads(Path(args.config).expanduser().read_text())
    command = Path(args.github_command).expanduser().resolve()
    if not command.is_file() or not os.access(command, os.X_OK):
        raise ValueError('App CLI is unavailable')
    config['github_command'] = str(command)
    # 凭据和路径检查先于构建；永远不输出 cc-connect 生成配置。
    cc_config = Path(args.cc_config).expanduser().resolve()
    sys.path.insert(0, str(Path(__file__).parent))
    from runtime import environment
    environment(cc_config, args.project)
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with tempfile.TemporaryDirectory(dir=root) as temp:
        temp = Path(temp)
        source = temp / 'source'
        source.mkdir()
        archive = run(['git', '-C', repo, 'archive', revision], stdout=subprocess.PIPE).stdout
        with tarfile.open(fileobj=io.BytesIO(archive)) as bundle:
            bundle.extractall(source, filter='data')
        candidate = temp / 'release'
        candidate.mkdir(mode=0o700)
        run(['go', 'build', '-trimpath', '-ldflags',
             '-X ink-commons/internal/ink.BuildRevision=' + revision,
             '-o', candidate / 'ink', './cmd/ink'], cwd=source)
        for name in ('runtime.py', 'manage.py'):
            shutil.copy2(source / 'deploy/macos' / name, candidate / name)
        atomic(candidate / 'config.json', json.dumps(config).encode())
        settings = {'revision': revision, 'home': str(home), 'python': sys.executable,
                    'data': str(home / '.local/state/ink-commons'),
                    'cc_config': str(cc_config), 'project': args.project}
        atomic(candidate / 'deployment.json', json.dumps(settings).encode())
        os.rename(candidate, release)
    print('Staged:', release)
    print('No running service was changed.')


def ink_plist(release, settings, user, system):
    data = Path(settings['data'])
    env = {'HOME': settings['home'],
           'PATH': str(Path(settings['home']) / '.local/bin') + ':/opt/homebrew/bin:/usr/bin:/bin'}
    result = {'Label': LABEL, 'ProgramArguments': [settings['python'], str(release / 'runtime.py')],
              'RunAtLoad': True, 'KeepAlive': True, 'ThrottleInterval': 30,
              'ExitTimeOut': 45, 'EnvironmentVariables': env,
              'StandardOutPath': str(data / 'stdout.log'),
              'StandardErrorPath': str(data / 'stderr.log')}
    if system:
        result['UserName'] = user
    return result


def health(data):
    with socket.socket(socket.AF_UNIX) as client:
        client.settimeout(2)
        client.connect(str(Path(data) / 'ink.sock'))
        client.sendall(b'GET /healthz HTTP/1.0\r\nHost: ink\r\n\r\n')
        chunks = []
        while True:
            part = client.recv(65536)
            if not part:
                break
            chunks.append(part)
    return json.loads(b''.join(chunks).split(b'\r\n\r\n', 1)[1])


def loaded(domain, label):
    return subprocess.run(['/bin/launchctl', 'print', domain + '/' + label],
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0


def unload(domain, label):
    if loaded(domain, label):
        run(['/bin/launchctl', 'bootout', domain + '/' + label])
    if loaded(domain, label):
        raise ValueError('service did not stop: ' + label)


def validate_state_directory(data, home, uid):
    if data != home / '.local/state/ink-commons' or not data.is_dir() or data.is_symlink():
        raise ValueError('existing private state directory required')
    if data.stat().st_uid != uid or data.stat().st_mode & 0o077:
        raise ValueError('state directory must be private and owned by service user')


def activate(args):
    account = pwd.getpwnam(args.user)
    if account.pw_uid == 0:
        raise ValueError('service must run as a non-root user')
    if args.system != (os.geteuid() == 0):
        raise ValueError('system activation requires sudo; user activation must run without sudo')
    if not args.system and os.geteuid() != account.pw_uid:
        raise ValueError('user activation must run as the service user')
    home = Path(account.pw_dir).resolve()
    release = Path(args.release).resolve()
    if release.parent != home / '.local/lib/ink-commons/releases':
        raise ValueError('release must be in the service user release directory')
    settings = json.loads((release / 'deployment.json').read_text())
    if settings['home'] != str(home):
        raise ValueError('release belongs to a different home')
    data = Path(settings['data'])
    validate_state_directory(data, home, account.pw_uid)
    if not args.system and loaded('system', LABEL):
        raise ValueError('system service exists; use system activation')
    if args.system and not args.cc_connect_agent and not (SYSTEM_DIR / (CC_LABEL + '.plist')).exists():
        raise ValueError('first system migration requires --cc-connect-agent')
    gui = 'gui/' + str(account.pw_uid)
    domain = 'system' if args.system else gui
    destination = SYSTEM_DIR if args.system else home / 'Library/LaunchAgents'
    jobs = [(LABEL, ink_plist(release, settings, args.user, args.system))]
    if args.system or args.cc_connect_agent:
        if not args.system:
            raise ValueError('--cc-connect-agent is only supported for system migration')
        cc_path = (Path(args.cc_connect_agent).resolve() if args.cc_connect_agent
                   else SYSTEM_DIR / (CC_LABEL + '.plist'))
        if args.cc_connect_agent and cc_path != home / 'Library/LaunchAgents' / (CC_LABEL + '.plist'):
            raise ValueError('expected the service user cc-connect LaunchAgent')
        cc = plistlib.loads(cc_path.read_bytes())
        if cc.get('Label') != CC_LABEL or not Path(cc['ProgramArguments'][0]).is_file():
            raise ValueError('invalid cc-connect agent')
        cc['UserName'] = args.user
        cc['RunAtLoad'] = True
        cc['KeepAlive'] = True
        cc.setdefault('EnvironmentVariables', {})['HOME'] = str(home)
        cc.pop('LimitLoadToSessionType', None)
        jobs.insert(0, (CC_LABEL, cc))
    # 在停服务前验证全部输入。stage 产生的配置仍可能被人工改坏。
    config = json.loads((release / 'config.json').read_text())
    if not config.get('github_command') or not Path(config['github_command']).is_file():
        raise ValueError('release has no App CLI')
    if not (release / 'ink').is_file() or not Path(settings['python']).is_file():
        raise ValueError('release runtime unavailable')
    backup = home / '.local/lib/ink-commons/backups' / str(time.time_ns())
    backup.mkdir(parents=True, mode=0o700)
    previous = []
    old_link = home / '.local/lib/ink-commons/ink'
    old_link_exists = old_link.exists() or old_link.is_symlink()
    active = []
    snapshot_ready = False
    gate = release / 'enabled'
    gate_was_enabled = gate.exists()
    try:
        for label, _ in jobs:
            for job_domain, directory in dict([(gui, home / 'Library/LaunchAgents'),
                                                (domain, destination)]).items():
                path = directory / (label + '.plist')
                content = path.read_bytes() if path.exists() else None
                previous.append((job_domain, path, content, loaded(job_domain, label)))
                if content is not None:
                    atomic(backup / (job_domain.replace('/', '-') + '-' + path.name), content)
                unload(job_domain, label)
        # 旧进程退出并释放锁之后才能复制 SQLite；不复制 socket 和锁文件。
        with open(data / 'state.json.lock', 'a') as lock:
            os.fchmod(lock.fileno(), 0o600)
            if args.system:
                os.fchown(lock.fileno(), account.pw_uid, account.pw_gid)
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            snapshot = backup / 'state'
            shutil.copytree(data, snapshot, ignore=shutil.ignore_patterns('ink.sock', 'state.json.lock', '*.log'))
            snapshot_ready = True
        if old_link_exists:
            shutil.copy2(old_link, backup / 'ink', follow_symlinks=True)
        gate.unlink(missing_ok=True)
        for label, job in jobs:
            path = destination / (label + '.plist')
            atomic(path, plistlib.dumps(job), 0o644)
            if args.system:
                os.chown(path, 0, 0)
                agent = home / 'Library/LaunchAgents' / path.name
                if agent.exists():
                    agent.unlink()  # 备份已记录；避免下次 GUI 登录又启动第二份。
            run(['/bin/launchctl', 'enable', domain + '/' + label])
            active.append((domain, label))
            run(['/bin/launchctl', 'bootstrap', domain, path])
        for _ in range(30):
            try:
                report = health(data)
                if report.get('revision') == settings['revision']:
                    break
            except (OSError, ValueError, IndexError):
                pass
            time.sleep(1)
        else:
            raise ValueError('running revision did not match staged revision')
        # 健康检查不代表链路成功：还必须用 App 读取 GitHub，并检查投递端端口。
        if args.system:
            cc_url = config.get('cc_connect_url', '')
            from urllib.parse import urlparse
            url = urlparse(cc_url)
            if url.hostname not in ('127.0.0.1', 'localhost') or not url.port:
                raise ValueError('system deployment requires a loopback cc-connect endpoint')
            for attempt in range(30):
                try:
                    with socket.create_connection((url.hostname, url.port), timeout=2):
                        break
                except OSError:
                    if attempt == 29:
                        raise
                    time.sleep(1)
        for entry in config['repositories']:
            repo = entry['repo']
            check = [config['github_command'], '--profile', 'inspect', '-R', repo, 'api', 'repos/' + repo]
            env = ink_plist(release, settings, args.user, args.system)['EnvironmentVariables']
            if args.system:
                check = ['/usr/bin/sudo', '-H', '-u', args.user, '--', '/usr/bin/env',
                         'HOME=' + env['HOME'], 'PATH=' + env['PATH']] + check
            run(check, env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=45)
        descriptor, temp_name = tempfile.mkstemp(prefix='ink.', dir=old_link.parent)
        os.close(descriptor)
        temp_link = Path(temp_name)
        try:
            temp_link.unlink()
            temp_link.symlink_to(release / 'ink')
            os.replace(temp_link, old_link)
        finally:
            temp_link.unlink(missing_ok=True)
        if args.system:
            os.lchown(old_link, account.pw_uid, account.pw_gid)
        # 最后一步才允许轮询及外部派发；之后不再进入回滚分支。
        atomic(gate, b'ready\n')
    except Exception:
        # 先停新进程，才能恢复迁移前状态；停止失败时绝不覆盖仍在用的数据库。
        for job_domain, label in reversed(active):
            unload(job_domain, label)
        gate.unlink(missing_ok=True)
        if gate_was_enabled:
            atomic(gate, b'ready\n')
        if snapshot_ready:
            for item in data.iterdir():
                if item.name not in ('ink.sock', 'state.json.lock') and not item.name.endswith('.log'):
                    if item.is_dir():
                        shutil.rmtree(item)
                    else:
                        item.unlink()
            shutil.copytree(backup / 'state', data, dirs_exist_ok=True)
            if args.system:
                for item in data.rglob('*'):
                    if item.is_file():
                        os.chown(item, account.pw_uid, account.pw_gid)
        if not old_link_exists:
            old_link.unlink(missing_ok=True)
        if old_link_exists and (backup / 'ink').exists():
            old_link.unlink(missing_ok=True)
            shutil.copy2(backup / 'ink', old_link)
            if args.system:
                os.chown(old_link, account.pw_uid, account.pw_gid)
        for job_domain, path, content, was_loaded in previous:
            if content is None:
                path.unlink(missing_ok=True)
            else:
                atomic(path, content, 0o644)
                if job_domain == gui and args.system:
                    os.chown(path, account.pw_uid, account.pw_gid)
        for job_domain, path, content, was_loaded in previous:
            label = path.stem
            if was_loaded and not loaded(job_domain, label):
                run(['/bin/launchctl', 'bootstrap', job_domain, path])
        raise

    print('Activated revision:', settings['revision'])
    print('Backup:', backup)


def status(args):
    account = pwd.getpwnam(args.user)
    data = Path(account.pw_dir) / '.local/state/ink-commons'
    report = {'user': args.user, 'services': {}}
    for label in (LABEL, CC_LABEL):
        report['services'][label] = {
            'system': loaded('system', label),
            'login': loaded('gui/' + str(account.pw_uid), label)}
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
    build.add_argument('--config', default='~/.config/ink-commons/config.json')
    build.add_argument('--cc-config', default='~/.cc-connect/config.toml')
    build.add_argument('--project', default='yu-xing-yi-codex')
    build.add_argument('--github-command', default='~/.local/bin/yuxingyi')
    apply = commands.add_parser('activate')
    apply.add_argument('--release', required=True)
    apply.add_argument('--user', required=True)
    apply.add_argument('--system', action='store_true')
    apply.add_argument('--cc-connect-agent')
    inspect = commands.add_parser('status')
    inspect.add_argument('--user', required=True)
    args = parser.parse_args()
    if sys.platform != 'darwin':
        parser.error('macOS only')
    try:
        {'stage': stage, 'activate': activate, 'status': status}[args.action](args)
    except Exception as error:
        # 不转发 subprocess stdout / stderr 或配置解析原文。
        print('Deployment failed:', type(error).__name__, file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
