# Runs inside the container, with no host mounts or network. Its only input is
# the validated operation, sent through stdin rather than process arguments.
import base64, json, os, pathlib, stat, subprocess, sys, time
ROOT = pathlib.Path('/workspace')
LIMIT = 1024 * 1024

def location(value='.'):
    p = (ROOT / value).resolve()
    if not p.is_relative_to(ROOT):
        raise ValueError('path must stay inside the workbench')
    return p

def main(request):
    action, args = request['action'], request.get('args', {})
    if action == 'exec':
        command = args.get('cmd', '')
        if not isinstance(command, str) or not command.strip():
            raise ValueError('exec requires cmd')
        timeout = min(600, max(1, int(args.get('timeoutSec', 60))))
        # The container has a fixed clean environment. Request variables affect
        # this child only, never the Docker client or a host subprocess.
        env = {'PATH':'/usr/local/bin:/usr/bin:/bin', 'HOME':'/tmp', 'LANG':'C.UTF-8'}
        extra = args.get('env', {})
        if not isinstance(extra, dict) or any(not isinstance(k,str) or not isinstance(v,str) for k,v in extra.items()):
            raise ValueError('env must contain text values')
        env.update(extra)
        import signal, tempfile, threading
        started = time.monotonic()
        captured = [bytearray(), bytearray()]
        truncated = [False, False]
        def read_stream(stream, index):
            while True:
                chunk=stream.read(8192)
                if not chunk: return
                remaining=LIMIT-len(captured[index])
                captured[index].extend(chunk[:remaining])
                if len(chunk)>remaining: truncated[index]=True
        with tempfile.TemporaryFile() as source:
            source.write(args.get('stdin','').encode()); source.seek(0)
            child = subprocess.Popen(['/bin/sh','-c',command],cwd=location(args.get('cwd','.')),env=env,stdin=source,stdout=subprocess.PIPE,stderr=subprocess.PIPE,start_new_session=True)
            threads=[threading.Thread(target=read_stream,args=(stream,i),daemon=True) for i,stream in enumerate([child.stdout,child.stderr])]
            for thread in threads: thread.start()
            try:
                child.wait(timeout=timeout)
            finally:
                try: os.killpg(child.pid,signal.SIGKILL)
                except ProcessLookupError: pass
                child.wait()
                for thread in threads: thread.join(timeout=2)
            return {'exitCode':child.returncode,'stdout':captured[0].decode(errors='replace'),'stderr':captured[1].decode(errors='replace'),'truncated':any(truncated),'durationMs':int((time.monotonic()-started)*1000)}
    p = location(args.get('path','.'))
    if action == 'fs_write':
        content=args.get('content','')
        if not isinstance(content,str): raise ValueError('content must be text')
        data=content.encode()
        if len(data)>16*LIMIT: raise ValueError('file is too large')
        p.parent.mkdir(parents=True,exist_ok=True)
        p.write_bytes(data)
        return {'path':str(p.relative_to(ROOT)),'bytesWritten':len(data)}
    if action == 'fs_read':
        if args.get('encoding') == 'base64':
            offset = int(args.get('offset', 0))
            limit = min(LIMIT, max(1, int(args.get('maxBytes', LIMIT))))
            with p.open('rb') as f:
                before = os.fstat(f.fileno())
                if not stat.S_ISREG(before.st_mode): raise ValueError('export requires a regular file')
                version = ':'.join(str(x) for x in (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns))
                if args.get('version', version) != version: raise ValueError('file changed during export')
                if offset < 0 or offset > before.st_size: raise ValueError('invalid file offset')
                f.seek(offset); data = f.read(limit)
                after = os.fstat(f.fileno())
                after_version = ':'.join(str(x) for x in (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns))
                if version != after_version: raise ValueError('file changed during export')
            return {'path':str(p.relative_to(ROOT)), 'encoding':'base64', 'content':base64.b64encode(data).decode('ascii'), 'offset':offset, 'size':before.st_size, 'version':version, 'eof':offset+len(data)==before.st_size}
        with p.open('rb') as f: data=f.read(LIMIT+1)
        return {'path':str(p.relative_to(ROOT)),'content':data[:LIMIT].decode(errors='replace'),'truncated':len(data)>LIMIT,'bytesRead':min(len(data),LIMIT)}
    if action == 'fs_list':
        import itertools
        entries=list(itertools.islice(p.iterdir(),1001))
        return {'entries':[{'name':x.name,'isDir':x.is_dir()} for x in entries[:1000]],'truncated':len(entries)>1000}
    if action == 'fs_stat':
        if not p.exists(): return {'exists':False}
        st=p.stat()
        return {'exists':True,'isDir':p.is_dir(),'size':st.st_size,'path':str(p.relative_to(ROOT))}
    raise ValueError('unknown action')

request=json.load(sys.stdin)
try:
    result={'ok':True,'action':request['action'],'payload':main(request)}
except subprocess.TimeoutExpired:
    result={'ok':False,'action':request['action'],'errorCode':'timeout','errorMessage':'The workbench command timed out.'}
except Exception as err:
    result={'ok':False,'action':request.get('action',''),'errorCode':'workbench_operation_failed','errorMessage':str(err)}
print(json.dumps(result))
