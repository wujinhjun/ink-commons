#!/usr/bin/env python3
"""launchd 的非交互入口；凭据只从本机配置读入子进程环境。"""
import json
import os
from pathlib import Path
import sys
import stat
import tomllib


def credential_environment(settings):
    if not settings.get('credentials'):
        return environment(Path(settings['cc_config']), settings['project'])
    path = Path(settings['credentials'])
    with path.open('rb') as stream:
        mode = os.fstat(stream.fileno())
        if not stat.S_ISREG(mode.st_mode) or mode.st_uid != os.geteuid() or mode.st_mode & 0o077:
            raise ValueError('credential file must be private and owned by the service user')
        tokens = json.load(stream)
    return token_environment(tokens['cc_connect_token'], tokens['discord_token'])


def environment(cc_config, project):
    with open(cc_config, 'rb') as stream:
        cc = tomllib.load(stream)
    selected = next(p for p in cc['projects'] if p['name'] == project)
    discord = next(p for p in selected['platforms'] if p['type'] == 'discord')
    return token_environment(cc['webhook']['token'], discord['options']['token'])


def token_environment(cc_token, discord_token):
    result = os.environ.copy()
    # 禁止继承旧启动器或个人 gh 环境；GitHub 由配置中的 App CLI 自行认证。
    for key in ('INK_GITHUB_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN'):
        result.pop(key, None)
    result['INK_CC_CONNECT_TOKEN'] = cc_token
    result['INK_DISCORD_TOKEN'] = discord_token
    if not all(isinstance(token, str) and token for token in (cc_token, discord_token)):
        raise ValueError('empty runtime credential')
    return result


def main():
    try:
        release = Path(__file__).resolve().parent
        settings = json.loads((release / 'deployment.json').read_text())
        env = environment(settings['cc_config'], settings['project'])
        os.execve(str(release / 'ink'), [str(release / 'ink'), 'serve', '-config',
                  str(release / 'config.json'), '-data', settings['data'],
                  '-poll-gate', str(release / 'enabled')], env)
    except Exception:
        # TOML 解析异常可能包含配置原文，不把它输出到 launchd 日志。
        sys.exit('ink runtime: configuration unavailable; inspect private local configuration')


if __name__ == '__main__':
    main()
