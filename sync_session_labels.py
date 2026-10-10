#!/usr/bin/env python3
"""Read only local Codex titles; publish labels for CPA's recorded IDs only."""
import argparse
import json
import os
from pathlib import Path
import sqlite3
import shutil
import sys
import plistlib
import subprocess
import tempfile
import urllib.request


def sync():
    info = json.loads(subprocess.check_output(['docker', 'inspect', 'cpa-manager-plus'], stderr=subprocess.DEVNULL))[0]
    key = next(x.split('=', 1)[1] for x in info['Config']['Env'] if x.startswith('CPA_MANAGEMENT_KEY='))
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    ids = set()
    for offset in range(0, 1000000, 1000):
        req = urllib.request.Request(f'http://127.0.0.1:9999/v0/management/plugins/anyrouter-retry/requests?limit=1000&offset={offset}', headers={'Authorization': 'Bearer '+key})
        with opener.open(req, timeout=10) as res:
            data = json.load(res)
        for item in data['requests']:
            ids.update(x for x in (item.get('session_id'), item.get('thread_id')) if x)
        if offset + 1000 >= data['total']:
            break
    files = list((Path.home()/'.codex').glob('state_*.sqlite'))
    if not files:
        raise RuntimeError('Codex state database missing')
    db = max(files, key=lambda p: int(p.stem.split('_')[-1]))
    labels = {}
    with sqlite3.connect(db.as_uri()+'?mode=ro', uri=True, timeout=5) as conn:
        columns = {x[1] for x in conn.execute('pragma table_info(threads)')}
        fields = ['id', 'title', 'cwd'] + [x for x in ['name', 'agent_nickname'] if x in columns]
        wanted = list(ids)
        for start in range(0, len(wanted), 300):
            chunk = wanted[start:start+300]
            query = 'select '+','.join(fields)+' from threads where id in ('+','.join('?' for _ in chunk)+')'
            for row in conn.execute(query, chunk):
                item = dict(zip(fields, row))
                title = ' '.join((item.get('name') or item['title'] or '').split())[:160]
                labels[item['id']] = {'title': title, 'project': Path(item['cwd']).name, 'agent': item.get('agent_nickname') or ''}
    payload = json.dumps(labels, ensure_ascii=False, sort_keys=True).encode()
    if len(payload) > 2*1024*1024:
        raise RuntimeError('Title index exceeds limit')
    # Publish in the existing volume; no container restart or new mount.
    with tempfile.TemporaryDirectory() as folder:
        source = Path(folder)/'session-labels.json'
        source.write_bytes(payload)
        target = '/CLIProxyAPI/plugins/anyrouter-retry-data'
        subprocess.run(['docker','exec','cli-proxy-api','mkdir','-p',target],check=True,stdout=subprocess.DEVNULL)
        subprocess.run(['docker','cp',str(source),'cli-proxy-api:'+target+'/.session-labels.next'],check=True,stdout=subprocess.DEVNULL)
        subprocess.run(['docker','exec','cli-proxy-api','sh','-c','chmod 600 '+target+'/.session-labels.next && mv '+target+'/.session-labels.next '+target+'/session-labels.json'],check=True,stdout=subprocess.DEVNULL)
    print(f'Synced labels for {len(labels)} recorded Codex threads; conversation history unchanged')


def configure_auto_sync(remove=False):
    service = 'local.cpa-anyrouter-session-labels'
    path = Path.home()/'Library/LaunchAgents'/f'{service}.plist'
    domain = f'gui/{os.getuid()}'
    if path.exists():
        subprocess.run(['launchctl', 'bootout', domain, str(path)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if remove:
        path.unlink(missing_ok=True)
        print('Automatic title sync removed')
        return
    logdir = Path(__file__).resolve().parent/'logs'
    logdir.mkdir(mode=0o700, exist_ok=True)
    path.parent.mkdir(parents=True, exist_ok=True)
    docker = shutil.which('docker')
    if not docker:
        raise RuntimeError('Docker CLI missing')
    spec = {'Label':service, 'ProgramArguments':[sys.executable, str(Path(__file__).resolve())],
            'RunAtLoad':True, 'StartInterval':60,
            'EnvironmentVariables':{'PATH':str(Path(docker).parent)+':/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin'},
            'StandardOutPath':str(logdir/'session-label-sync.log'),
            'StandardErrorPath':str(logdir/'session-label-sync-error.log')}
    path.write_bytes(plistlib.dumps(spec))
    path.chmod(0o600)
    subprocess.run(['launchctl','bootstrap',domain,str(path)],check=True)
    print('Automatic title sync enabled every 60 seconds')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group()
    group.add_argument('--install-auto-sync', action='store_true')
    group.add_argument('--remove-auto-sync', action='store_true')
    args = parser.parse_args()
    try:
        if args.install_auto_sync or args.remove_auto_sync:
            configure_auto_sync(args.remove_auto_sync)
        else:
            sync()
    except Exception as error:
        # Never print request objects, credentials, or database contents.
        raise SystemExit('Title sync failed: '+type(error).__name__) from None
