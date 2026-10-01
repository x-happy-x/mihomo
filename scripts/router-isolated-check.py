#!/usr/bin/env python3
"""MIHOMO-4: compare router cores on loopback without changing live routing.

Uses an existing SSH host and provider file. Credentials stay in a mode-600
temporary router directory. Every instance has independent state and a bounded
watchdog; no TUN, redirect, DNS listener, or Tailscale instance is started.
"""
import argparse
import json
import shlex
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--router', default='keenetic')
    parser.add_argument('--candidate', required=True)
    args = parser.parse_args()

    def ssh(command, data=None):
        result = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o',
                                 'ConnectTimeout=8', args.router, command],
                                input=data, capture_output=True, timeout=35)
        if result.returncode:
            raise RuntimeError('Router check command failed; private output withheld')
        return result.stdout

    provider = json.loads(ssh('/opt/sbin/yq -o=json /opt/etc/mihomo/proxy-providers/router.yaml'))
    wanted = ['🇫🇮 Финляндия, Extra Whitelist2', '🇫🇲 ОБХОД №1-2',
              '🇫🇲 ОБХОД №4-1', '🇫🇲 ОБХОД №4-2']
    proxies = [p for p in provider['proxies'] if p['name'] in wanted]
    assert len(proxies) == len(wanted), 'Expected test nodes are missing'
    for p in proxies:
        assert p['type'] == 'vless' and 'dialer-proxy' not in p
    live = json.loads(ssh('/opt/sbin/yq -o=json /opt/etc/mihomo/config.yaml'))
    assert live.get('routing-mark') == 255, 'Unexpected router bypass mark'
    config = {'mixed-port': 11080, 'allow-lan': False, 'bind-address': '127.0.0.1',
              'external-controller': '127.0.0.1:19090', 'mode': 'rule',
              'routing-mark': 255, 'log-level': 'warning', 'ipv6': live.get('ipv6', False),
              'proxies': proxies, 'proxy-groups': [{'name': 'TEST', 'type': 'select',
              'proxies': [p['name'] for p in proxies]}], 'rules': ['MATCH,TEST']}
    root = '/opt/tmp/homenet-probe-' + str(int(time.time()))
    assert not ssh('curl --noproxy "*" -s --max-time 2 http://127.0.0.1:19090/version; true'), 'Diagnostic port is already in use'
    ssh('umask 077; mkdir ' + root + '; cat >' + root + '/config.json',
        json.dumps(config, ensure_ascii=False).encode())
    for label, binary in [('previous', '/opt/sbin/mihomo'), ('candidate', args.candidate)]:
        directory = root + '/' + label
        ssh('mkdir ' + directory)
        launch = ('nohup ' + shlex.quote(binary) + ' -d ' + directory + ' -f ' + root
                  + '/config.json >' + directory + '/run.log 2>&1 & p=$!; '
                  + 'echo $p >' + directory + '/pid; '
                  + '(sleep 240; test ! -f ' + directory + '/pid || kill $p 2>/dev/null) >/dev/null 2>&1 </dev/null &')
        try:
            ssh(launch)
            time.sleep(2)
            version = json.loads(ssh('curl --noproxy "*" -fsS --max-time 5 http://127.0.0.1:19090/version'))
            print(json.dumps({'core': label, **version}), flush=True)
            for p in proxies:
                body = json.dumps({'name': p['name']}, ensure_ascii=False).encode()
                ssh('curl --noproxy "*" -fsS --max-time 5 -X PUT -H "Content-Type: application/json" '
                    '--data-binary @- http://127.0.0.1:19090/proxies/TEST', body)
                for url in ['https://www.google.com/', 'https://www.cloudflare.com/cdn-cgi/trace',
                            'https://chatgpt.com/']:
                    command = ('curl --noproxy "" -x http://127.0.0.1:11080 '
                               '--connect-timeout 4 --max-time 8 -sS -o /dev/null '
                               '-w "%{http_code} %{size_download} %{time_total}" '
                               + shlex.quote(url) + ' 2>/dev/null; true')
                    result = ssh(command).decode().strip()
                    print(json.dumps({'core': label, 'node': p['name'], 'url': url,
                                      'result': result}, ensure_ascii=False), flush=True)
        finally:
            ssh('test ! -f ' + directory + '/pid || kill $(cat ' + directory + '/pid) 2>/dev/null; rm -f ' + directory + '/pid; true')
            time.sleep(1)
    print('Private diagnostic files retained at ' + root)


if __name__ == '__main__':
    import sys
    sys.stdout.reconfigure(encoding='utf-8')
    main()
