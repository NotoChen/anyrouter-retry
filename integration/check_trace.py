#!/usr/bin/env python3
"""Verify actual host headers, pinned credentials, durable traces and viewer ACL."""
import concurrent.futures
import json
import subprocess
import tempfile
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request
from check import control, calls

base = 'http://127.0.0.1:19998'
opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

def api(path, data=None, token='dummy-management'):
    req = urllib.request.Request(base + path, None if data is None else json.dumps(data).encode(), {'Authorization': 'Bearer '+token, 'Content-Type':'application/json'})
    with opener.open(req, timeout=15) as response:
        return json.load(response)

def listing(session):
    return api('/v0/management/plugins/anyrouter-retry/requests?session='+urllib.parse.quote(session))

def model(session, thread, turn='turn-1'):
    meta = dict(session_id=session, thread_id=thread, parent_thread_id=session, turn_id=turn, agent_name='/root/review')
    req = urllib.request.Request(base+'/v1/responses', json.dumps(dict(model='gpt-6-astra',stream=True,input='say RETRY_OK')).encode(), {'Authorization':'Bearer dummy-client','Content-Type':'application/json','Session-Id':session,'Thread-Id':thread,'X-Codex-Turn-Metadata':json.dumps(meta)})
    with opener.open(req, timeout=15) as response:
        return response.read().decode()

def wait(fn, seconds=8):
    end=time.monotonic()+seconds
    while time.monotonic()<end:
        try: value=fn()
        except (urllib.error.URLError, OSError): value=None
        if value: return value
        time.sleep(.04)
    raise AssertionError('condition not reached')

if __name__ == '__main__':
    wait(lambda: api('/v0/management/plugins/anyrouter-retry/runtime')['version']=='0.1.13')
    stamp=str(time.time_ns())
    session='trace-session-retry-'+stamp
    wait_session='trace-waiting-'+stamp
    concurrent_session='trace-concurrent-'+stamp
    control(mode='http502', failures=2, delay=0)
    result=model(session,'trace-thread-child')
    assert 'response.completed' in result and calls()==3
    rows=wait(lambda: [r for r in listing(session)['requests'] if r['phase']=='completed'])
    assert len(rows)==1
    r=rows[0]
    assert r['session_id']==session and r['thread_id']=='trace-thread-child' and r['turn_id']=='turn-1' and r['agent_name']=='/root/review',r
    assert r['attempts']==3 and r['retries']==2 and r['storage_errors']==0,r
    detail=api('/v0/management/plugins/anyrouter-retry/trace?id='+r['id'])
    attempts=detail['attempts']
    assert [a['number'] for a in attempts]==[1,2,3],attempts
    assert [a['http_status'] for a in attempts]==[502,502,200],attempts
    names=[a['credential_name'] for a in attempts]
    assert set(names)=={'AnyRouter-kj','AnyRouter-qm','AnyRouter-bl'},names
    assert [a['outcome'] for a in attempts]==['retry','retry','completed'],attempts
    print(json.dumps(dict(test='real-host-session-and-attempts',session=session,attempts=3,sequence=names,completed=True)))

    with tempfile.TemporaryDirectory() as directory:
        labels=Path(directory)/'session-labels.json'
        labels.write_text(json.dumps({session: {'title':'可识别的测试会话','project':'RetryProject'}}))
        subprocess.run(['docker','cp',str(labels),'cpa-anyrouter-retry-test:/tmp/anyrouter-retry-traces/session-labels.json'],check=True)
    enriched=listing(session)['requests'][0]
    assert enriched['session_title']=='可识别的测试会话' and enriched['project']=='RetryProject'
    searched=api('/v0/management/plugins/anyrouter-retry/requests?q='+urllib.parse.quote('RetryProject'))
    assert any(item['id']==r['id'] for item in searched['requests'])
    assert api('/v0/management/plugins/anyrouter-retry/trace?id='+r['id'])['request']['session_title']=='可识别的测试会话'
    print(json.dumps(dict(test='session-title-project-and-search',passed=True)))

    control(mode='sse',failures=0,delay=1.2,post_output_delay=1.2)
    with concurrent.futures.ThreadPoolExecutor() as pool:
        waiting=pool.submit(model,wait_session,'trace-wait-thread')
        wait(lambda: calls()>=1)
        row=wait(lambda: next((r for r in listing(wait_session)['requests'] if r['phase']=='waiting_upstream'),None))
        assert row['attempts']==1 and not row.get('ended_at')
        streamed=wait(lambda: next((r for r in listing(wait_session)['requests'] if r['phase']=='streaming'),None))
        assert streamed['first_output_at']>0
        assert 'response.completed' in waiting.result(timeout=10)
    print(json.dumps(dict(test='waiting-vs-streaming-visible',passed=True)))

    control(mode='sse',failures=0,delay=.3,post_output_delay=0)
    with concurrent.futures.ThreadPoolExecutor() as pool:
        futures=[pool.submit(model,concurrent_session,f'child-{n}',f'turn-{n}') for n in range(2)]
        assert all('response.completed' in f.result(timeout=8) for f in futures)
    rows=wait(lambda: [r for r in listing(concurrent_session)['requests'] if r['phase']=='completed'])
    assert len(rows)==2 and len({r['id'] for r in rows})==2 and {r['thread_id'] for r in rows}=={'child-0','child-1'}
    print(json.dumps(dict(test='same-session-concurrent-threads',separate_requests=True)))

    # HTML is public; request/attempt records only have authenticated management routes.
    for path in ['/v0/resource/plugins/anyrouter-retry/view/requests','/v0/management/plugins/anyrouter-retry/requests']:
        try: api(path,token='')
        except urllib.error.HTTPError as error: assert error.code in (401,404),error.code
        else: raise AssertionError('unauthorized request accepted')
    html=opener.open(base+'/v0/resource/plugins/anyrouter-retry/dashboard').read().decode()
    assert '会话请求' in html and '逐次结果' in html and 'restoreManagerLogin' in html
    print(json.dumps(dict(test='dashboard-resource-and-management-auth',passed=True)))
