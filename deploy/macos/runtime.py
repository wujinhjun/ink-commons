#!/usr/bin/env python3
"""launchd 的非交互入口；凭据只从本机配置读入子进程环境。"""
import json
import os
from pathlib import Path
import sys
import stat
import tomllib


def credential_environment(settings):
    if settings.get('cc_config'):
        return environment(Path(settings['cc_config']), settings['project'])
    result = clean_environment()
    if not settings.get('credentials'):
        return result
    with Path(settings['credentials']).open('rb') as stream:
        mode = os.fstat(stream.fileno())
        if not stat.S_ISREG(mode.st_mode) or mode.st_uid != os.geteuid() or mode.st_mode & 0o077:
            raise ValueError('credential file must be private and owned by the service user')
        tokens = json.load(stream)
    names = {'cc_connect_token': 'INK_CC_CONNECT_TOKEN', 'discord_token': 'INK_DISCORD_TOKEN',
             'http_provider_token': 'INK_HTTP_PROVIDER_TOKEN'}
    if not isinstance(tokens, dict) or tokens.keys() - names.keys():
        raise ValueError('unknown provider credential')
    for key, token in tokens.items():
        if not isinstance(token, str) or not token:
            raise ValueError('empty runtime credential')
        result[names[key]] = token
    return result


def clean_environment():
    result = os.environ.copy()
    for key in ('INK_GITHUB_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN'):
        result.pop(key, None)
    return result


def environment(cc_config, project):
    with open(cc_config, 'rb') as stream:
        cc = tomllib.load(stream)
    selected = next(p for p in cc['projects'] if p['name'] == project)
    discord = next(p for p in selected['platforms'] if p['type'] == 'discord')
    return token_environment(cc['webhook']['token'], discord['options']['token'])


def token_environment(cc_token, discord_token):
    result = clean_environment()
    result['INK_CC_CONNECT_TOKEN'] = cc_token
    result['INK_DISCORD_TOKEN'] = discord_token
    if not all(isinstance(token, str) and token for token in (cc_token, discord_token)):
        raise ValueError('empty runtime credential')
    return result


def main(args=()):
    try:
        if args not in ((), ('--config-only',)):
            sys.exit('usage: runtime.py [--config-only]')
        release = Path(__file__).resolve().parent
        settings = json.loads((release / 'deployment.json').read_text())
        env = credential_environment(settings)
        os.execve(str(release / 'ink'), [str(release / 'ink'), 'serve', '-config',
                  str(release / 'config.json'), '-data', settings['data'],
                  '-poll-gate', str(release / 'enabled'), *args], env)
    except Exception:
        # TOML 解析异常可能包含配置原文，不把它输出到 launchd 日志。
        sys.exit('ink runtime: configuration unavailable; inspect private local configuration')


if __name__ == '__main__':
    main(tuple(sys.argv[1:]))
