#!/usr/bin/env python3
"""Measure the unchanged Work suite inside one normal, bounded native job."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time

out = Path('.memql-work-diagnostic')
out.mkdir()
probe = out / 'runtime.go'
probe.write_text('package main\nimport("encoding/json";"os";"runtime")\nfunc main(){json.NewEncoder(os.Stdout).Encode(map[string]any{"version":runtime.Version(),"gomaxprocs":runtime.GOMAXPROCS(0),"numCPU":runtime.NumCPU()})}\n')
runtime = json.loads(subprocess.check_output(['go','run',str(probe)]))
probe.unlink()

def snapshot():
    result={}
    for name in ['cpu.max','cpu.stat','cpu.pressure','memory.max','memory.current','memory.events','io.stat','io.pressure']:
        path=Path('/sys/fs/cgroup')/name
        if path.exists(): result[name]=path.read_text()
    return result

report={'runtime':runtime,'before':snapshot(),'command':['go','test','-json','-count=1','-p=1','-timeout=180s','./integrations/work']}
print(json.dumps({'runtime':runtime,'before':report['before']}),flush=True)
started=time.monotonic()
with (out/'tests.jsonl').open('w') as log:
    result=subprocess.run(report['command'],stdout=log,stderr=subprocess.STDOUT,check=False)
report.update(elapsed=time.monotonic()-started,exitCode=result.returncode,after=snapshot())
events=[]
for line in (out/'tests.jsonl').read_text(errors='replace').splitlines():
    try: event=json.loads(line)
    except ValueError: continue
    if event.get('Action') in ['pass','fail','skip']:events.append({k:event[k] for k in ['Action','Test','Elapsed'] if k in event})
report['results']=events
(out/'summary.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps({k:v for k,v in report.items() if k!='results'}),flush=True)
print(json.dumps({'counts':{a:sum(e['Action']==a for e in events) for a in ['pass','fail','skip']},'slowest':sorted(events,key=lambda x:x.get('Elapsed',0),reverse=True)[:15]}),flush=True)
sys.exit(result.returncode if result.returncode>=0 else 1)
