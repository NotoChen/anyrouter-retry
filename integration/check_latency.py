#!/usr/bin/env python3
"""Measure real CPA native and plugin delivery against local upstream timestamps."""
import json
import time
import urllib.request
from check import control, calls

opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
base='http://127.0.0.1:19998'
def api(path,data=None):
    request=urllib.request.Request(base+path,None if data is None else json.dumps(data).encode(),{'Authorization':'Bearer dummy-management','Content-Type':'application/json'},method='GET' if data is None else 'PATCH')
    with opener.open(request,timeout=10) as response:return json.load(response)
def run(enabled,output_bytes=0,delay=.4,post=1.2):
    api('/v0/management/plugins/anyrouter-retry/config',{'accept_new_requests':enabled})
    control(mode='sse',failures=0,delay=delay,post_output_delay=post,output_bytes=output_bytes)
    started=time.time();first_delta=None;completed=None
    req=urllib.request.Request(base+'/v1/responses',json.dumps({'model':'gpt-6-astra','stream':True,'input':'latency check'}).encode(),{'Content-Type':'application/json','Authorization':'Bearer dummy-client','Session-Id':'latency-test'})
    with opener.open(req,timeout=15) as response:
        for line in response:
            if not line.startswith(b'data:'):continue
            try:event=json.loads(line[5:])
            except ValueError:continue
            if event.get('type')=='response.output_text.delta':
                first_delta=time.time();assert len(event['delta'])==8+output_bytes
            if event.get('type')=='response.completed':completed=time.time()
    assert first_delta and completed and calls()==1
    stats=json.load(opener.open('http://127.0.0.1:19997/stats'))
    delivery_ms=round((first_delta-stats['delta_sent_at'])*1000,2)
    if not output_bytes:
        assert first_delta-started<delay+.8 and completed-first_delta>post-.3,(first_delta-started,completed-first_delta)
    record={'test':'stream-latency','plugin':enabled,'output_bytes':output_bytes,'first_delta_s':round(first_delta-started,3),'completion_s':round(completed-started,3),'upstream_to_client_delta_ms':delivery_ms,'completed':True}
    print(json.dumps(record))
    return record
if __name__=='__main__':
    try:
        native=run(False)
        plugin=run(True)
        # This assertion concerns a local controlled stream, not real provider latency.
        assert plugin['upstream_to_client_delta_ms']<200,plugin
        run(True,output_bytes=9*1024*1024,delay=.1,post=1.2)
        rows=api('/v0/management/plugins/anyrouter-retry/requests?session=latency-test')['requests']
        assert any(r.get('last_attempt',{}).get('first_forwarded_at') for r in rows)
    finally:
        api('/v0/management/plugins/anyrouter-retry/config',{'accept_new_requests':True})
