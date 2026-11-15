#!/usr/bin/env python3
"""launchd 的非交互入口；凭据只从本机配置读入子进程环境。"""
import json
import os
from pathlib import Path
import sys
import tomllib


def environment(cc_config, project):
    with open(cc_config, 'rb') as stream:
        cc = tomllib.load(stream)
    selected = next(p for p in cc['projects'] if p['name'] == project)
    discord = next(p for p in selected['platforms'] if p['type'] == 'discord')
    result = os.environ.copy()
    # 禁止继承旧启动器或个人 gh 环境；GitHub 由配置中的 App CLI 自行认证。
    for key in ('INK_GITHUB_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN'):
        result.pop(key, None)
    result['INK_CC_CONNECT_TOKEN'] = cc['webhook']['token']
    result['INK_DISCORD_TOKEN'] = discord['options']['token']
    if not result['INK_CC_CONNECT_TOKEN'] or not result['INK_DISCORD_TOKEN']:
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
