#!/usr/bin/env python3
"""版本化构建与 launchd 迁移。stage 不改服务；activate 才停止、备份和切换。"""
import argparse
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
CC_LABEL = 'com.cc-connect.service'
WATCHDOG = 'com.personal-infra.discord-watchdog'
SYSTEM_DIR = Path('/Library/LaunchDaemons')
REPOSITORY = 'wujinhjun/ink-commons'
UPGRADE_REF = 'refs/remotes/ink-upgrade/main'


class DeploymentError(Exception):
    """只包含脚本编写的安全诊断，不包含配置原文或子进程输出。"""


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


def existing_cc_binary(home):
    for path in (home / 'Library/LaunchAgents' / (CC_LABEL+'.plist'), SYSTEM_DIR / (CC_LABEL+'.plist')):
        if path.is_file():
            binary=Path(plistlib.loads(path.read_bytes())['ProgramArguments'][0])
            if not binary.is_file():
                raise DeploymentError('installed cc-connect binary is missing; specify --cc-connect-binary')
            return binary
    raise DeploymentError('cc-connect service missing; specify --cc-connect-binary')


def disabled(domain,label):
    result = subprocess.run(['/bin/launchctl','print-disabled',domain],capture_output=True)
    if result.returncode:
        return False  # 尚不存在的 GUI domain 没有已加载服务。
    pattern = r'"' + re.escape(label) + r'"\s*=>\s*(?:true|disabled)\b'
    return re.search(pattern,result.stdout.decode()) is not None


def listener_pids(port):
    result = subprocess.run(['/usr/sbin/lsof','-nP','-t','-iTCP:'+str(port),'-sTCP:LISTEN'],capture_output=True)
    return sorted({int(value) for value in result.stdout.split() if value.isdigit()})


def cc_listener_healthy(domain,port):
    result = subprocess.run(['/bin/launchctl','print',domain+'/'+CC_LABEL],capture_output=True)
    match = re.search(r'\bpid = (\d+)',result.stdout.decode())
    return bool(match) and listener_pids(port) == [int(match.group(1))]


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


def cc_process_pids():
    result=subprocess.run(['/bin/ps','-ww','-Ao','pid=,comm='],capture_output=True)
    pids=[]
    for line in result.stdout.decode().splitlines():
        fields=line.strip().split(None,1)
        if len(fields)==2 and fields[0].isdigit() and Path(fields[1]).name=='cc-connect':
            pids.append(int(fields[0]))
    return sorted(set(pids))


def stage_artifact(artifact, target, revision):
    checksum = (artifact / 'SHA256SUMS').read_text()
    expected = re.fullmatch(r'([0-9a-f]{64})  ink\n?', checksum)
    if expected is None:
        raise DeploymentError('invalid ink artifact checksum file')
    shutil.copyfile(artifact / 'ink', target)
    with target.open('rb') as binary:
        actual = hashlib.file_digest(binary, 'sha256').hexdigest()
    if actual != expected[1]:
        raise DeploymentError('ink artifact checksum mismatch')
    target.chmod(0o700)
    verify_revision(target, revision)


def verify_revision(binary, revision):
    try:
        result = run([binary, 'version'], capture_output=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        raise DeploymentError('ink artifact version could not be read on this Mac') from None
    if result.stdout.strip() != revision.encode():
        raise DeploymentError('ink artifact revision does not match --revision')


def stage(args):
    if os.geteuid() == 0:
        raise DeploymentError('stage must run as the service user, without sudo')
    if sys.version_info < (3, 11, 8):
        raise DeploymentError('Python 3.11.8+ required')
    repo = Path(__file__).resolve().parents[2]
    revision = output(['git', '-C', repo, 'rev-parse', '--verify', '--end-of-options', args.revision + '^{commit}'])
    home = Path.home().resolve()
    extra_env = parse_env(args.env)
    root = home / '.local/lib/ink-commons/releases'
    release = root / revision
    if release.exists():
        raise DeploymentError('release already exists; reuse it or explicitly archive it before restaging')
    config = json.loads(Path(args.config).expanduser().read_text())
    command = Path(args.github_command).expanduser().resolve()
    if not command.is_file() or not os.access(command, os.X_OK):
        raise DeploymentError('App CLI is unavailable')
    config['github_command'] = str(command)
    # 凭据和路径检查先于构建；永远不输出 cc-connect 生成配置。
    cc_config = Path(args.cc_config).expanduser().resolve()
    sys.path.insert(0, str(Path(__file__).parent))
    from runtime import environment
    environment(cc_config, args.project)
    root.mkdir(parents=True, exist_ok=True, mode=0o700)
    with tempfile.TemporaryDirectory(dir=root) as temp:
        temp = Path(temp)
        candidate = temp / 'release'
        candidate.mkdir(mode=0o700)
        if args.artifact_dir:
            stage_artifact(Path(args.artifact_dir).expanduser(), candidate / 'ink', revision)
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
        cc_source = Path(args.cc_connect_binary).expanduser().resolve() if args.cc_connect_binary else existing_cc_binary(home)
        if not cc_source.is_file():
            raise DeploymentError('cc-connect source binary is missing')
        if 'releases' in cc_source.parts and not args.cc_connect_binary:
            raise DeploymentError('cc-connect is already release-pinned; explicitly select --cc-connect-binary to reuse or upgrade it')
        print('cc-connect source:',cc_source,'sha256:',hashlib.sha256(cc_source.read_bytes()).hexdigest()[:16])
        shutil.copy2(cc_source, candidate / 'cc-connect')
        atomic(candidate / 'config.json', json.dumps(config).encode())
        settings = {'revision': revision, 'home': str(home), 'python': sys.executable,
                    'data': str(home / '.local/state/ink-commons'),
                    'cc_config': str(cc_config), 'project': args.project, 'environment': extra_env,
                    'cc_binary_sha256': hashlib.sha256((candidate / 'cc-connect').read_bytes()).hexdigest()}
        atomic(candidate / 'deployment.json', json.dumps(settings).encode())
        os.rename(candidate, release)
    print('Staged:', release)
    print('No running service was changed.')


def upgrade():
    if os.geteuid() == 0:
        raise DeploymentError('upgrade must run as the service user, without sudo')
    root = Path.home().resolve() / '.local/lib/ink-commons'
    entry = root / 'ink'
    if not entry.is_symlink() or not loaded('system', LABEL):
        raise DeploymentError('upgrade requires an existing system deployment')
    with (root / 'upgrade.lock').open('a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise DeploymentError('another upgrade is running') from None
        upgrade_system(root, entry)


def verify_staged_upgrade(current, target, config, settings):
    try:
        staged_config = json.loads((target / 'config.json').read_text())
        staged_settings = json.loads((target / 'deployment.json').read_text())
    except (OSError, ValueError):
        raise DeploymentError('staged release configuration is missing or invalid') from None
    expected_settings = {**settings, 'revision': target.name}
    if staged_config != config or staged_settings != expected_settings:
        raise DeploymentError('staged release configuration differs from the current deployment; '
                              'archive the staged release before retrying')
    expected_cc = settings.get('cc_binary_sha256')
    for release in (current, target):
        binary = release / 'cc-connect'
        if not binary.is_file() or hashlib.sha256(binary.read_bytes()).hexdigest() != expected_cc:
            raise DeploymentError('cc-connect binary does not match the current deployment checksum')


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
    repo = Path(__file__).resolve().parents[2]
    app = [config['github_command'], '--profile', 'inspect', '-R', REPOSITORY]
    run([*app, 'git', 'fetch', '--no-tags', f'https://github.com/{REPOSITORY}.git',
         'refs/heads/main:' + UPGRADE_REF], cwd=repo, env=network_env)
    revision = output(['git', '-C', repo, 'rev-parse', '--verify', UPGRADE_REF + '^{commit}'])
    target = releases / revision
    if target == current:
        try:
            running = health(settings['data'])
        except (OSError, ValueError, IndexError, DeploymentError):
            running = {}
        if isinstance(running, dict) and running.get('revision') == revision and running.get('gate') == 'open':
            print('Already running:', revision)
            return

    if not target.exists():
        with tempfile.TemporaryDirectory() as artifact:
            try:
                run([*app, 'release', 'download', 'build-' + revision, '--dir', artifact,
                     '--pattern', 'ink', '--pattern', 'SHA256SUMS'], env=network_env)
            except subprocess.CalledProcessError:
                raise DeploymentError('unable to download the main build; check the GitHub Actions publish job') from None
            stage(argparse.Namespace(
                revision=revision, artifact_dir=artifact, config=str(config_path),
                github_command=config['github_command'], cc_config=settings['cc_config'],
                project=settings['project'], cc_connect_binary=str(current / 'cc-connect'),
                env=proxy_entries,
            ))
    else:
        if target != current:
            verify_staged_upgrade(current, target, config, settings)
        verify_revision(target / 'ink', revision)

    # Ask for sudo before changing the skill link. The existing activate command
    # owns service validation and rollback.
    run(['sudo', '-v'])
    try:
        run([target / 'ink', 'init'])
        run([target / 'ink', 'init', '--check'])
        run(['sudo', sys.executable, target / 'manage.py', 'activate', '--system',
             '--user', pwd.getpwuid(os.geteuid()).pw_name, '--release', target])
    except (OSError, subprocess.CalledProcessError):
        if entry.resolve().parent == current:
            try:
                run([current / 'ink', 'init'])
            except (OSError, subprocess.CalledProcessError):
                print('Unable to restore the previous agent skill', file=sys.stderr)
        raise


def ink_plist(release, settings, user, system):
    data = Path(settings['data'])
    env = {'HOME': settings['home'],
           'PATH': str(Path(settings['home']) / '.local/bin') + ':/opt/homebrew/bin:/usr/bin:/bin'}
    env.update(parse_env([key + '=' + value for key,value in settings.get('environment', {}).items()]))
    result = {'Label': LABEL, 'ProgramArguments': [settings['python'], str(release / 'runtime.py')],
              'RunAtLoad': True, 'KeepAlive': True, 'ThrottleInterval': 30,
              'ExitTimeOut': 45, 'EnvironmentVariables': env,
              'StandardOutPath': str(data / 'stdout.log'),
              'StandardErrorPath': str(data / 'stderr.log')}
    if system:
        result['UserName'] = user
    return result


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


def activate(args):
    account = pwd.getpwnam(args.user)
    if account.pw_uid == 0:
        raise DeploymentError('service must run as a non-root user')
    if args.system != (os.geteuid() == 0):
        raise DeploymentError('system activation requires sudo; user activation must run without sudo')
    if not args.system and os.geteuid() != account.pw_uid:
        raise DeploymentError('user activation must run as the service user')
    home = Path(account.pw_dir).resolve()
    release = Path(args.release).resolve()
    if release.parent != home / '.local/lib/ink-commons/releases':
        raise DeploymentError('release must be in the service user release directory')
    settings = json.loads((release / 'deployment.json').read_text())
    if settings['home'] != str(home):
        raise DeploymentError('release belongs to a different home')
    if settings.get('revision') != release.name:
        raise DeploymentError('release name and revision differ')
    data = Path(settings['data'])
    validate_state_directory(data, home, account.pw_uid)
    if not args.system and loaded('system', LABEL):
        raise DeploymentError('system service exists; use system activation')
    if args.system and not args.cc_connect_agent and not (SYSTEM_DIR / (CC_LABEL + '.plist')).exists():
        raise DeploymentError('first system migration requires --cc-connect-agent')
    gui = 'gui/' + str(account.pw_uid)
    domain = 'system' if args.system else gui
    destination = SYSTEM_DIR if args.system else home / 'Library/LaunchAgents'
    jobs = [(LABEL, ink_plist(release, settings, args.user, args.system))]
    if args.system or args.cc_connect_agent:
        if not args.system:
            raise DeploymentError('--cc-connect-agent is only supported for system migration')
        cc_path = (Path(args.cc_connect_agent).resolve() if args.cc_connect_agent
                   else SYSTEM_DIR / (CC_LABEL + '.plist'))
        if args.cc_connect_agent and cc_path != home / 'Library/LaunchAgents' / (CC_LABEL + '.plist'):
            raise DeploymentError('expected the service user cc-connect LaunchAgent')
        cc = plistlib.loads(cc_path.read_bytes())
        if cc.get('Label') != CC_LABEL or not Path(cc['ProgramArguments'][0]).is_file():
            raise DeploymentError('invalid cc-connect agent')
        cc_original_binary = Path(cc['ProgramArguments'][0])
        bundled = release / 'cc-connect'
        if not bundled.is_file() or hashlib.sha256(bundled.read_bytes()).hexdigest() != settings.get('cc_binary_sha256'):
            raise DeploymentError('staged cc-connect binary is unavailable or changed')
        cc['ProgramArguments'][0] = str(bundled)
        cc['UserName'] = args.user
        cc['RunAtLoad'] = True
        cc['KeepAlive'] = True
        cc.setdefault('EnvironmentVariables', {})['HOME'] = str(home)
        cc.pop('LimitLoadToSessionType', None)
        jobs.insert(0, (CC_LABEL, cc))
    # 在停服务前验证全部输入。stage 产生的配置仍可能被人工改坏。
    config = json.loads((release / 'config.json').read_text())
    if not config.get('github_command') or not Path(config['github_command']).is_file():
        raise DeploymentError('release has no App CLI')
    if not (release / 'ink').is_file() or not Path(settings['python']).is_file():
        raise DeploymentError('release runtime unavailable')
    backup = home / '.local/lib/ink-commons/backups' / str(time.time_ns())
    backup.mkdir(parents=True, mode=0o700)
    print('Backup:', backup, flush=True)
    previous = []
    old_link = home / '.local/lib/ink-commons/ink'
    old_link_exists = old_link.exists() or old_link.is_symlink()
    old_link_target = os.readlink(old_link) if old_link.is_symlink() else None
    active = []
    snapshot_ready = False
    gate = release / 'enabled'
    gate_was_enabled = gate.exists()
    phase = 'capture'
    try:
        for label in [WATCHDOG] + [label for label,_ in jobs]:
            domains = [(gui, home / 'Library/LaunchAgents')]
            if label != WATCHDOG:
                domains = list(dict(domains + [(domain,destination)]).items())
            elif not args.system:
                continue
            for job_domain, directory in domains:
                path = directory / (label + '.plist')
                content = path.read_bytes() if path.exists() else None
                metadata = path.stat() if content is not None else None
                was_loaded = loaded(job_domain,label)
                if was_loaded and content is None:
                    raise DeploymentError('loaded service has no restorable plist: ' + label)
                previous.append((job_domain,path,content,was_loaded,metadata,disabled(job_domain,label)))
                if content is not None:
                    atomic(backup / (job_domain.replace('/', '-') + '-' + path.name),content)
        atomic(backup / 'metadata.json',json.dumps([{'domain':d,'path':str(p),'loaded':l,'disabled':x,
               'mode':stat.S_IMODE(m.st_mode) if m else None,'uid':m.st_uid if m else None,'gid':m.st_gid if m else None}
               for d,p,c,l,m,x in previous]).encode())
        if args.system:
            shutil.copy2(cc_original_binary,backup / 'cc-connect')
        phase = 'stop_old_services'
        # 先摘掉所有 GUI 入口并停 watchdog，随后才停 cc-connect，消除被重新拉起的窗口。
        for job_domain,path,content,was_loaded,metadata,was_disabled in previous:
            if args.system and job_domain == gui and content is not None:
                path.unlink()
            if path.stem == WATCHDOG and args.system:
                run(['/bin/launchctl','disable',job_domain+'/'+WATCHDOG])
            unload(job_domain,path.stem)
        phase = 'snapshot'
        # 旧进程退出并释放锁之后才能复制 SQLite；不复制 socket 和锁文件。
        with open(data / 'state.json.lock', 'a') as lock:
            os.fchmod(lock.fileno(), 0o600)
            if args.system:
                os.fchown(lock.fileno(), account.pw_uid, account.pw_gid)
            acquire_state_lock(lock)
            snapshot = backup / 'state'
            shutil.copytree(data, snapshot, ignore=shutil.ignore_patterns('ink.sock', 'state.json.lock', '*.log'))
            snapshot_ready = True
        if old_link_exists:
            shutil.copy2(old_link, backup / 'ink', follow_symlinks=True)
        gate.unlink(missing_ok=True)
        phase = 'start_candidates'
        for label, job in jobs:
            path = destination / (label + '.plist')
            atomic(path, plistlib.dumps(job), 0o644)
            if args.system:
                os.chown(path, 0, 0)
            run(['/bin/launchctl', 'enable', domain + '/' + label])
            active.append((domain, label))
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
        # 健康检查不代表链路成功：还必须用 App 读取 GitHub，并检查投递端端口。
        if args.system:
            cc_url = config.get('cc_connect_url', '')
            from urllib.parse import urlparse
            url = urlparse(cc_url)
            if url.hostname not in ('127.0.0.1', 'localhost') or not url.port:
                raise DeploymentError('system deployment requires a loopback cc-connect endpoint')
            for attempt in range(30):
                try:
                    with socket.create_connection((url.hostname, url.port), timeout=2):
                        break
                except OSError:
                    if attempt == 29:
                        raise
                    time.sleep(1)
            if not cc_listener_healthy(domain,url.port):
                raise DeploymentError('cc-connect listener does not belong to the system service')
        phase = 'daemon_self_check'
        for attempt in range(3):
            try:
                local_get(data,'/v1/self-check')
                break
            except (OSError,DeploymentError) as error:
                if attempt == 2:
                    raise
                time.sleep(1)
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
    except (Exception,KeyboardInterrupt):
        restored = {'state':False,'binary':False,'services':False}
        try:
            # 先停新进程，才能恢复迁移前状态；停止失败时绝不覆盖仍在用的数据库。
            gate.unlink(missing_ok=True)
            for job_domain, label in reversed(active):
                unload(job_domain, label)
            if snapshot_ready:
                with open(data/'state.json.lock','a') as recovery_lock:
                    acquire_state_lock(recovery_lock)
                    for item in data.iterdir():
                        if item.name not in ('ink.sock', 'state.json.lock') and not item.name.endswith('.log'):
                            if item.is_dir():
                                shutil.rmtree(item)
                            else:
                                item.unlink()
                    shutil.copytree(backup / 'state', data, dirs_exist_ok=True)
                    if args.system:
                        for item in data.rglob('*'):
                            os.lchown(item, account.pw_uid, account.pw_gid)
            restored['state'] = snapshot_ready
            if not old_link_exists:
                old_link.unlink(missing_ok=True)
            if old_link_exists and (backup / 'ink').exists():
                old_link.unlink(missing_ok=True)
                if old_link_target is not None:
                    old_link.symlink_to(old_link_target)
                else:
                    shutil.copy2(backup / 'ink', old_link)
                if args.system:
                    os.lchown(old_link, account.pw_uid, account.pw_gid)
            restored['binary'] = True
            for job_domain, path, content, was_loaded, metadata, was_disabled in previous:
                if content is None:
                    path.unlink(missing_ok=True)
                else:
                    atomic(path, content, stat.S_IMODE(metadata.st_mode))
                    if args.system:
                        os.chown(path, metadata.st_uid, metadata.st_gid)
                run(['/bin/launchctl','disable' if was_disabled else 'enable',job_domain+'/'+path.stem])
            for job_domain, path, content, was_loaded, metadata, was_disabled in sorted(previous,key=lambda item:item[1].stem==WATCHDOG):
                label = path.stem
                if was_loaded and not loaded(job_domain, label):
                    run(['/bin/launchctl', 'bootstrap', job_domain, path])

            if gate_was_enabled:
                atomic(gate,b'ready\n')
            restored['services'] = True
        except Exception as recovery_error:
            print(json.dumps({'deployment':'rollback_incomplete','phase':phase,'recovery':'incomplete',
                  'revision':settings['revision'],'backup':str(backup),
                  'gate':'open' if gate.exists() else 'closed','restored':restored}),flush=True)
            raise DeploymentError('recovery incomplete; do not overwrite state while the new process may still run') from recovery_error
        print(json.dumps({'deployment':'rolled_back','phase':phase,'revision':settings['revision'],
              'backup':str(backup),'gate':'open' if gate.exists() else 'closed',
              'restored':restored}),flush=True)
        raise
    print('Activated revision:', settings['revision'], 'gate:', 'open' if gate.exists() else 'closed')
    print('Backup:', backup)


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
    from urllib.parse import urlparse
    endpoint=json.loads(config_path.read_text()).get('cc_connect_url','') if config_path.is_file() else ''
    port=urlparse(endpoint).port or 9111
    binaries={}
    for name,path in [('system',SYSTEM_DIR/(CC_LABEL+'.plist')),('login',home/'Library/LaunchAgents'/(CC_LABEL+'.plist'))]:
        if path.is_file():
            binary=Path(plistlib.loads(path.read_bytes())['ProgramArguments'][0])
            binaries[name]={'path':str(binary),'exists':binary.is_file()}
    report = {'user': args.user, 'services': {}}
    for label in (LABEL, CC_LABEL, WATCHDOG):
        report['services'][label] = {
            'system': loaded('system', label),
            'login': loaded('gui/' + str(account.pw_uid), label)}
    try:
        report['runtime'] = health(data)
    except (OSError, ValueError, IndexError):
        report['runtime'] = {'status': 'unavailable'}
    report['cc_connect']={'process_pids':cc_process_pids(),'port':port,
                          'listener_pids':listener_pids(port),'binaries':binaries}
    print(json.dumps(report, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest='action', required=True)
    build = commands.add_parser('stage')
    build.add_argument('--revision', required=True)
    build.add_argument('--artifact-dir', help='extracted GitHub Actions artifact directory')
    build.add_argument('--env', action='append', default=[], help='non-secret proxy environment KEY=VALUE')
    build.add_argument('--cc-connect-binary')
    build.add_argument('--config', default='~/.config/ink-commons/config.json')
    build.add_argument('--cc-config', default='~/.cc-connect/config.toml')
    build.add_argument('--project', default='yu-xing-yi-codex')
    build.add_argument('--github-command', default='~/.local/bin/yuxingyi')
    commands.add_parser('upgrade', help='download and activate the verified main build')
    apply = commands.add_parser('activate')
    apply.add_argument('--release', required=True)
    apply.add_argument('--user', required=True)
    apply.add_argument('--system', action='store_true')
    apply.add_argument('--cc-connect-agent')
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
            {'stage': stage, 'activate': activate, 'status': status}[args.action](args)
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
