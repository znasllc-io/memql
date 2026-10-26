#!/usr/bin/env python3
"""Prepare, install, or qualify the reviewed fixed image-verification service.

The plan phase binds compact owner parameters to a verified exact-source archive
and its official artifact manifest. Later phases consume that private plan. This capability never changes worker configuration, host firewall policy,
AppArmor, subordinate-ID allocations, or an existing installation.
"""
import argparse
import datetime as dt
import shutil
import hashlib
import io
import json
import os
from pathlib import Path
import pwd
import re
import signal
import stat
import struct
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.parse
import urllib.request

CAPABILITY = "image-verification.setup"
SUPPORT = "scripts/image-verification/"
PACKAGES = {"uidmap", "libsubid4", "libnftables1", "libnftnl11", "libmnl0", "libgmp10", "libjansson4", "libxtables12", "libedit2", "libbsd0", "libmd0", "libtinfo6"}
TOOLS = {"rootlesskit", "runc", "git", "systemctl", "ip", "newuidmap", "newgidmap"}
ROOTLESS_FLAGS = ("--subid-source", "--port-driver", "--propagation", "--disable-host-loopback", "--slirp4netns-binary", "--pidns", "--copy-up", "--state-dir")
FIXTURES = ["target-export", "resource-limits", "namespace-isolation", "network-isolation", "cancel-and-timeout", "worker-restart-recovery", "ambiguous-start-and-cleanup", "mapped-state-accounting-cleanup"]
HASH = re.compile(r"[0-9a-f]{64}\Z")
CHANGED = False


class Refused(RuntimeError):
    pass


def require(ok, reason):
    if not ok:
        raise Refused(reason)


def sha(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def canonical(path):
    p = Path(path)
    require(p.is_absolute() and os.path.normpath(str(p)) == str(p) and "\x00" not in str(p), "A setup path is not canonical")
    return p


def regular(path, maximum, owner=None):
    p = canonical(path)
    for item in (p, *p.parents):
        require(not item.is_symlink(), "A setup file crosses a symbolic path")
    s = p.stat()
    require(stat.S_ISREG(s.st_mode) and s.st_nlink == 1 and 0 < s.st_size <= maximum, "A setup file is not bounded regular content")
    require(owner is None or s.st_uid == owner, "A setup file has the wrong owner")
    return p


def read_json(path, maximum=1 << 20):
    def unique(pairs):
        out = {}
        for k, v in pairs:
            require(k not in out, "Duplicate JSON member")
            out[k] = v
        return out
    value = json.loads(regular(path, maximum).read_bytes(), object_pairs_hook=unique)
    require(isinstance(value, dict), "A setup record is not an object")
    return value


def write_new(path, data, mode=0o600):
    global CHANGED
    p = canonical(path)
    fd = os.open(p, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
    CHANGED = True
    with os.fdopen(fd, "wb") as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
    d = os.open(p.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(d)
    finally:
        os.close(d)


def save(path, value, mode=0o600):
    write_new(path, (json.dumps(value, sort_keys=True) + "\n").encode(), mode)


def command(argv, *, timeout=30, input=None, env=None, maximum=1 << 20, allowed=(0,), binary=False, owner=None):
    # Own the process group. An uncertain package or renderer operation retains
    # its setup intent and is never retried automatically.
    with tempfile.TemporaryFile() as stdin, tempfile.TemporaryFile() as stdout, tempfile.TemporaryFile() as stderr:
        if input is not None: stdin.write(input)
        stdin.seek(0)
        identity={} if owner is None else {"user":owner[0],"group":owner[1],"extra_groups":[]}
        p = subprocess.Popen(argv, stdin=stdin, stdout=stdout, stderr=stderr, start_new_session=True, **identity,
                             env=env or {"PATH": "/usr/bin:/usr/sbin:/bin:/sbin", "LANG": "C", "LC_ALL": "C"})
        deadline=time.monotonic()+timeout
        failure=None
        try:
            while p.poll() is None:
                if time.monotonic()>=deadline: failure="A prerequisite timed out; its outcome may be uncertain";break
                if os.fstat(stdout.fileno()).st_size>maximum or os.fstat(stderr.fileno()).st_size>maximum: failure="A prerequisite returned excessive output";break
                time.sleep(0.05)
        finally:
            try: os.killpg(p.pid,0);remains=True
            except ProcessLookupError: remains=False
            if remains:
                try: os.killpg(p.pid,signal.SIGTERM)
                except ProcessLookupError: pass
                end=time.monotonic()+1
                while time.monotonic()<end:
                    p.poll()
                    try: os.killpg(p.pid,0)
                    except ProcessLookupError: break
                    time.sleep(0.05)
                try: os.killpg(p.pid,signal.SIGKILL)
                except ProcessLookupError: pass
                if failure is None: failure="A prerequisite left descendants; its process group was stopped"
            p.wait(timeout=3)
        require(failure is None,failure)
        require(os.fstat(stdout.fileno()).st_size<=maximum and os.fstat(stderr.fileno()).st_size<=maximum,"A prerequisite returned excessive output")
        require(p.returncode in allowed,"A prerequisite command failed; no automatic retry was made")
        stdout.seek(0);raw=stdout.read(maximum+1)
        return (raw if binary else raw.decode("utf-8")),p.returncode


def immutable(path, executable=False):
    p = canonical(path)
    for item in (p, *p.parents):
        s = item.lstat()
        require(not stat.S_ISLNK(s.st_mode) and s.st_uid == 0 and not s.st_mode & 0o022, "An installation path is mutable or symbolic")
        require(stat.S_ISREG(s.st_mode) if item == p else stat.S_ISDIR(s.st_mode), "An installation path has the wrong type")
    require(not executable or p.stat().st_mode & 0o111, "An immutable executable is not executable")
    return p


def resolve_runtime_path(path, aliases):
    """Resolve only root-owned existing aliases and retain every exact link."""
    p = canonical(path)
    seen = set()
    for _ in range(32):
        require(str(p) not in seen, "Runtime alias cycle")
        seen.add(str(p))
        current = Path("/")
        changed = False
        for index, part in enumerate(p.parts[1:]):
            current /= part
            s = current.lstat()
            require(s.st_uid == 0 and (stat.S_ISLNK(s.st_mode) or not s.st_mode & 0o022), "A runtime path is not root-owned immutable content")
            if stat.S_ISLNK(s.st_mode):
                text = os.readlink(current)
                require(0 < len(text) <= 1024 and "\n" not in text and "\x00" not in text, "Invalid runtime alias")
                require(str(current) not in aliases or aliases[str(current)] == text, "Runtime alias changed during inspection")
                aliases[str(current)] = text
                target = Path(text) if text.startswith("/") else current.parent / text
                p = Path(os.path.normpath(str(target.joinpath(*p.parts[index + 2:]))))
                changed = True
                break
            require(stat.S_ISDIR(s.st_mode) if index < len(p.parts) - 2 else stat.S_ISREG(s.st_mode), "Runtime dependency is not a regular file")
        if not changed:
            return immutable(p)
    raise Refused("Runtime alias chain exceeds its bound")


def private_directory(path, owner):
    p=canonical(path)
    for current in (p,*p.parents):
        s=current.lstat()
        require(stat.S_ISDIR(s.st_mode) and s.st_uid in (0,owner),"A staging directory is symbolic or has a foreign owner")
        require(not s.st_mode&0o022 or current!=p and s.st_uid==0 and s.st_mode&stat.S_ISVTX,"A staging ancestor is writable by another account")
    require(p.stat().st_uid==owner and not p.stat().st_mode&0o077,"Staging must be private to the selected account")
    return p


def source_inventory(archive, archive_sha, source, commit):
    require(HASH.fullmatch(archive_sha or "") and sha(regular(archive,256<<20))==archive_sha,"The reviewed source archive changed")
    prefix="memql-"+commit+"/"
    fixed={"scripts/lib/host_build_guard.py","cockpit/cmd/memql-imageverify/main.go",SUPPORT+"setup.py",SUPPORT+"qualify.py",SUPPORT+"official-artifacts.json"}
    inventory={};seen=set();total=0
    with tarfile.open(archive,"r:*") as tar:
        for member in tar:
            name=member.name
            require(name not in seen and (name==prefix[:-1] or name.startswith(prefix)) and ".." not in Path(name).parts,"The source archive has foreign or duplicate paths")
            seen.add(name);total+=max(member.size,0)
            require(len(seen)<=30000 and total<=512<<20,"The source archive exceeds its bound")
            path=name[len(prefix):]
            selected=path in fixed or re.fullmatch(r"cockpit/internal/worker/imageverify/[A-Za-z0-9_]+\.go",path)
            if not selected: continue
            require(member.isfile() and 0<member.size<=2<<20,"A selected source file is not bounded regular content")
            data=tar.extractfile(member).read(member.size+1)
            require(len(data)==member.size,"A selected source file is truncated")
            digest=hashlib.sha256(data).hexdigest()
            require(sha(regular(source/path,2<<20))==digest,"Extracted source differs from the reviewed archive")
            inventory[path]=digest
    require(fixed<=inventory.keys() and 8<=len(inventory)<=128,"The reviewed source closure is incomplete")
    actual={str(p.relative_to(source)) for p in (source/"cockpit/internal/worker/imageverify").iterdir()}
    require(actual=={p for p in inventory if p.startswith("cockpit/internal/worker/imageverify/")},"Unexpected files in the qualification package")
    return inventory


def selected_workspace(unit_file, account, explicit=None):
    """Read the normal unit and policy; never expose complete policy contents."""
    import shlex
    unit=regular(unit_file,65536,os.getuid())
    starts=[];environments=[]
    for line in unit.read_text().splitlines():
        line=line.strip()
        require(not line.endswith('\\'),"A continued unit directive needs explicit review")
        if line.startswith('ExecStart='):starts.append(line[len('ExecStart='):])
        if line.startswith(('EnvironmentFile=','PassEnvironment=')):raise Refused("An external unit environment needs explicit workspace review")
        require(not re.search(r'(?:^|[\s\"])MEMQL_WORKER_TOKEN=',line),"A worker single-home environment override needs separate review")
        if line.startswith('Environment='):environments.extend(shlex.split(line[len('Environment='):]))
    require(len(starts)==1,"The selected worker command is ambiguous")
    require(not any(v in starts[0] for v in ('$', '%', '\\')),"Systemd command expansions require separate review")
    argv=shlex.split(starts[0]);require(len(argv)>=3 and argv[1:3]==['worker','run'] and argv[0].startswith('/'),"The selected unit is not the normal worker command")
    for item in environments:
        key,separator,value=item.partition('=')
        require(key!='MEMQL_WORKER_TOKEN',"A worker single-home environment override needs separate review")
        require(separator and (key!='HOME' or value==account.pw_dir),"The worker's HOME differs from its account")
    paths={};remaining=argv[3:]
    while remaining:
        token=remaining.pop(0)
        key,sep,value=token.partition('=')
        require(key in ('--config','--workers') and key not in paths,"A worker command override requires separate review")
        if not sep:
            require(bool(remaining),"A worker path override is missing");value=remaining.pop(0)
        paths[key]=canonical(value)
    workers=paths.get('--workers',Path(account.pw_dir)/'.memql/workers.yaml')
    config=paths.get('--config',Path(account.pw_dir)/'.memql/worker.yaml')
    require(workers.parent==config.parent,"Worker mode could select different policy directories; explicit review is required")
    policy=workers.parent/'policy.yaml'
    witness={'unitSHA256':sha(unit),'policyPath':str(policy),'policySHA256':None}
    selected=None
    if policy.exists() or policy.is_symlink():
        regular(policy,1<<20,os.getuid());raw=policy.read_bytes();witness['policySHA256']=hashlib.sha256(raw).hexdigest()
        try: import yaml
        except ImportError:
            require(explicit is not None,"The existing policy requires an installed safe YAML reader or an explicitly reviewed workspace root")
        else:
            class UniqueLoader(yaml.SafeLoader):
                pass
            def mapping(loader,node,deep=False):
                result={}
                for key,value in node.value:
                    require(key.tag!='tag:yaml.org,2002:merge',"A merged policy mapping requires explicit review")
                    name=loader.construct_object(key,deep=deep)
                    require(isinstance(name,str) and name not in result,"The existing policy has an ambiguous key")
                    result[name]=loader.construct_object(value,deep=deep)
                return result
            UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,mapping)
            document=yaml.load(raw,Loader=UniqueLoader)
            require(isinstance(document,dict),"The existing policy is not a mapping")
            roots=[]
            for name in ('pipelines','fs'):
                section=document.get(name) or {};require(isinstance(section,dict),"The existing workspace policy is not a mapping")
                value=section.get('workspace_root') or '';require(isinstance(value,str),"The workspace setting is not a string")
                value=value.strip()
                if value.startswith('~/'):value=account.pw_dir+value[1:]
                require(not value or value.startswith('/'),"A relative workspace root requires explicit review")
                roots.append(str(canonical(value)) if value else '')
            selected=roots[0] or (str(Path(roots[1])/'pipelines') if roots[1] else str(Path(account.pw_dir)/'.memql/pipelines'))
    else:selected=str(Path(account.pw_dir)/'.memql/pipelines')
    if explicit is not None:
        require(selected is None or str(explicit)==selected,"The explicit workspace disagrees with the actual policy")
        selected=str(explicit)
    require(selected is not None,"The existing workspace root could not be resolved")
    return canonical(selected),witness


def check_workspace_witness(p):
    witness=p['workspaceWitness'];require(set(witness)=={'unitSHA256','policyPath','policySHA256'},"Workspace evidence is incomplete")
    require(sha(regular(p['worker']['unitFile']['path'],65536,p['owner']['uid']))==witness['unitSHA256'],"The selected worker command changed")
    policy=canonical(witness['policyPath'])
    if witness['policySHA256'] is None:require(not policy.exists() and not policy.is_symlink(),"A workspace policy appeared after selection")
    else:require(sha(regular(policy,1<<20,p['owner']['uid']))==witness['policySHA256'],"The selected workspace policy changed")


def make_plan(args):
    require(os.getuid()>0 and os.getuid()==os.geteuid(),"Generate the plan as the selected nonroot owner")
    require(re.fullmatch(r"[0-9a-f]{40}",args.source_sha or "") and args.source_archive is not None,"An exact reviewed source commit and archive are required")
    require(not args.plan.exists() and not args.output.exists() and not args.plan_sha256 and not args.confirm,"Generate a new plan without action confirmation")
    private_directory(args.plan.parent,os.getuid())
    require(args.output.parent==args.plan.parent,"Keep the new staging directory beside its private plan")
    require(sys.platform=="linux" and os.uname().machine=="x86_64","This setup requires Linux amd64")
    require(re.fullmatch(r"[a-z0-9_-]+",args.host_id or "") and re.fullmatch(r"[0-9.]+",args.host_version or ""),"The reviewed host identity is required")
    release=dict(line.split("=",1) for line in Path("/etc/os-release").read_text().splitlines() if "=" in line and not line.startswith("#"))
    require(release.get("ID","").strip('"')==args.host_id and release.get("VERSION_ID","").strip('"')==args.host_version,"The actual host differs from the reviewed owner parameters")
    require(args.host_version=="24.04" and (args.host_id=="ubuntu" or "ubuntu" in release.get("ID_LIKE","").strip('"').split()),"The pinned package manifest requires an Ubuntu Noble-compatible host")
    account=pwd.getpwuid(os.getuid())
    require(account.pw_gid==os.getgid() and account.pw_gid>0,"The primary owner account changed")
    require(re.fullmatch(r"memql[-A-Za-z0-9_.@]*\.service",args.worker_unit or "") and args.worker_unit_file is not None,"The existing worker unit is required")
    regular(args.worker_unit_file,1<<20,os.getuid())
    require(args.worker_unit_file==Path(account.pw_dir)/".config/systemd/user"/args.worker_unit,"The existing unit must be the selected account's normal user unit")
    workspace,witness=selected_workspace(args.worker_unit_file,account,args.workspace_root)
    require(workspace.is_dir() and workspace.stat().st_uid==os.getuid() and not workspace.is_symlink() and workspace.is_relative_to(Path(account.pw_dir)),"The existing owner workspace root is required")
    for ancestor in (workspace,*workspace.parents):
        s=ancestor.lstat();require(stat.S_ISDIR(s.st_mode) and s.st_uid in (0,os.getuid()) and not s.st_mode&0o022,"The workspace root crosses an unsafe directory")
    inventory=source_inventory(args.source_archive,args.source_archive_sha256,args.source,args.source_sha)
    require(inventory[SUPPORT+"setup.py"]==sha(Path(__file__).absolute()),"The running setup differs from the reviewed source")
    tools={}
    for name in sorted({"rootlesskit","runc","git","systemctl","ip","dpkg","dpkg-query","dpkg-deb"}):
        found=shutil.which(name,path="/usr/sbin:/usr/bin:/sbin:/bin")
        require(found is not None,"A required existing distro tool is missing: "+name)
        path=str(Path(found).resolve(strict=True))
        immutable(path,True);tools[name]={"path":path,"sha256":sha(path)}
    require(tools["rootlesskit"]["path"]=="/usr/bin/rootlesskit","Retain the existing distro RootlessKit path")
    require(args.idle_observation is not None and len(args.idle_observation.encode())<=4096,"A fresh reviewed activity observation is required")
    idle=json.loads(args.idle_observation)
    require(set(idle)=={"observedAt","activeWork"} and type(idle["activeWork"]) is int and idle["activeWork"]==0,"Only the actual reviewed idle observation is accepted")
    age=(dt.datetime.now(dt.timezone.utc)-dt.datetime.fromisoformat(idle["observedAt"].replace("Z","+00:00"))).total_seconds()
    require(0<=age<=3600,"The reviewed idle observation is stale")
    idle.update(ownerUID=os.getuid(),workerUnit=args.worker_unit)
    idle_path=args.plan.parent/"idle-observation.json"
    save(idle_path,idle)
    artifact_path=args.source/(SUPPORT+"official-artifacts.json")
    plan={"schemaVersion":1,"sourceSHA":args.source_sha,"sourceArchiveSHA256":args.source_archive_sha256,"sourceFiles":inventory,"owner":{"uid":os.getuid(),"gid":os.getgid(),"home":account.pw_dir},"host":{"ID":args.host_id,"VERSION_ID":args.host_version},"worker":{"unit":args.worker_unit,"unitFile":{"path":str(args.worker_unit_file)}},"idleReceipt":{"path":str(idle_path),"sha256":sha(idle_path)},"artifacts":{"path":str(artifact_path),"sha256":sha(artifact_path)},"distroTools":tools,"workspaceRoot":str(workspace),"workspaceWitness":witness,"attemptRoot":"/var/lib/memql-imageverify/"+str(os.getuid()),"limits":{"maxCPUMilli":4000,"maxMemoryMiB":8192,"maxRuntimeSeconds":2700,"maxStorageBytes":15<<30}}
    save(args.plan,plan)
    return {"phase":"planned","plan":str(args.plan),"planSHA256":sha(args.plan),"sourceSHA":args.source_sha,"workerStopped":False}


def load_plan(args):
    require(HASH.fullmatch(args.plan_sha256 or "") and sha(args.plan) == args.plan_sha256, "The reviewed private plan changed")
    p = read_json(args.plan)
    require(HASH.fullmatch(p.get("sourceArchiveSHA256", "")),"Source archive evidence is absent")
    required = {"schemaVersion", "sourceSHA", "sourceArchiveSHA256", "sourceFiles", "owner", "host", "worker", "idleReceipt", "artifacts", "distroTools", "workspaceRoot", "workspaceWitness", "attemptRoot", "limits"}
    require(set(p) == required and p["schemaVersion"] == 1 and re.fullmatch(r"[0-9a-f]{40}", p["sourceSHA"] or ""), "The exact source plan is incomplete")
    require(sys.platform == "linux" and os.uname().machine == "x86_64", "This setup requires the reviewed Linux amd64 host")
    release = dict(line.split("=", 1) for line in Path("/etc/os-release").read_text().splitlines() if "=" in line and not line.startswith("#"))
    require(all(release.get(k, "").strip('"') == v for k, v in p["host"].items()) and set(p["host"]) == {"ID", "VERSION_ID"}, "The host distribution differs from the reviewed plan")
    owner = p["owner"]
    require(set(owner) == {"uid", "gid", "home"} and type(owner["uid"]) is int and owner["uid"] > 0 and type(owner["gid"]) is int and owner["gid"] > 0, "The selected nonroot account is missing")
    account = pwd.getpwuid(owner["uid"])
    require(account.pw_gid == owner["gid"] and account.pw_dir == owner["home"], "The selected account changed")
    require(os.getuid() in (0, owner["uid"]), "Run this as the selected owner or its explicit root installation step")
    for name, expected in p["sourceFiles"].items():
        allowed = name == "scripts/lib/host_build_guard.py" or name == "cockpit/cmd/memql-imageverify/main.go" or re.fullmatch(r"cockpit/internal/worker/imageverify/[A-Za-z0-9_]+\.go", name) or name in (SUPPORT + "setup.py", SUPPORT + "qualify.py", SUPPORT + "official-artifacts.json")
        require(allowed and HASH.fullmatch(expected or "") and sha(regular(args.source / name, 2 << 20)) == expected, "Reviewed source bytes changed")
    require(5 <= len(p["sourceFiles"]) <= 128 and p["sourceFiles"].get(SUPPORT + "setup.py") == sha(Path(__file__).absolute()), "The mechanical setup does not match the source inventory")
    require(p["sourceFiles"].get(SUPPORT + "qualify.py") is not None, "The reviewed qualification wrapper is absent")
    require(set(p["distroTools"]) == {"rootlesskit", "runc", "git", "systemctl", "ip", "dpkg", "dpkg-query", "dpkg-deb"}, "The fixed distro tool inventory is incomplete")
    for item in p["distroTools"].values():
        require(set(item) == {"path", "sha256"} and HASH.fullmatch(item["sha256"]) and sha(immutable(item["path"], True)) == item["sha256"], "A reviewed distro tool changed")
    require(p["distroTools"]["rootlesskit"]["path"] == "/usr/bin/rootlesskit", "Retain the distro RootlessKit executable attachment")
    limits = p["limits"]
    require(set(limits) == {"maxCPUMilli", "maxMemoryMiB", "maxRuntimeSeconds", "maxStorageBytes"}, "Unknown service ceiling")
    require(all(type(v) is int for v in limits.values()) and 1000 <= limits["maxCPUMilli"] <= 4000 and 1024 <= limits["maxMemoryMiB"] <= 8192 and 90 <= limits["maxRuntimeSeconds"] <= 2700 and 1 << 30 <= limits["maxStorageBytes"] <= 15 << 30, "Service ceilings exceed the reviewed setup contract")
    require(p["attemptRoot"]=="/var/lib/memql-imageverify/"+str(owner["uid"]),"The fixed attempt root changed")
    for name in ("workspaceRoot", "attemptRoot"):
        canonical(p[name])
    check_workspace_witness(p)
    artifact = p["artifacts"]
    require(set(artifact) == {"path", "sha256"} and HASH.fullmatch(artifact["sha256"]) and sha(regular(artifact["path"], 1 << 20)) == artifact["sha256"], "Official artifact manifest changed")
    require(artifact["path"]==str(args.source/(SUPPORT+"official-artifacts.json")) and artifact["sha256"]==p["sourceFiles"].get(SUPPORT+"official-artifacts.json"),"Artifact manifest is not bound to the exact source")
    manifest=read_json(artifact["path"])
    require(manifest.get("schemaVersion")==1 and manifest.get("baseDistribution")=={"id":"ubuntu","version":"24.04","architecture":"amd64"},"Official manifest compatibility differs")
    require(set(manifest.get("expectedBuiltBinaries",{}))=={"memql-imageverify","imageverify.test"} and all(HASH.fullmatch(v) for v in manifest["expectedBuiltBinaries"].values()),"Reviewed helper/fixture identities are missing")
    return p,manifest


class OfficialRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        safe_url(newurl)
        return super().redirect_request(req, fp, code, msg, headers, newurl)


def safe_url(url):
    u = urllib.parse.urlparse(url)
    require(u.scheme == "https" and u.hostname in {"github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com", "go.dev", "dl.google.com", "archive.ubuntu.com"} and not u.username and not u.password and u.port in (None, 443), "Artifact URL is not an approved official HTTPS origin")


def download(item, target):
    global CHANGED
    safe_url(item["url"])
    size = int(item.get("size", item.get("Size", 0)))
    expected = item.get("sha256", item.get("SHA256", ""))
    require(0 < size <= 160 << 20 and HASH.fullmatch(expected), "Official artifact identity is incomplete")
    require(not target.exists(), "An artifact download already exists; inspect it before retrying")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), OfficialRedirect())
    request = urllib.request.Request(item["url"], headers={"User-Agent": "MemQL-reviewed-setup", "Accept-Encoding": "identity"})
    deadline=time.monotonic()+180
    with opener.open(request, timeout=45) as response, open(target, "xb") as output:
        CHANGED=True
        require(response.status == 200, "Artifact download failed")
        total = 0
        while True:
            require(time.monotonic()<deadline,"Artifact download exceeded its bounded window")
            chunk = response.read(min(1 << 20, size + 1 - total))
            if not chunk:
                break
            total += len(chunk)
            require(total <= size, "Artifact exceeds its declared size")
            output.write(chunk)
    require(total == size and sha(target) == expected, "Official artifact bytes do not match their published identity")


def extract_tar(path, dest, members=None, maximum=600 << 20):
    seen = set()
    written = set()
    total = 0
    with tarfile.open(path, "r:*") as archive:
        for member in archive:
            name = member.name.removeprefix("./")
            if name in ("", ".") and member.isdir():
                require("." not in seen,"Duplicate archive root directory");seen.add(".");continue
            require(name and not name.startswith("/") and ".." not in Path(name).parts and name not in seen, "Archive has a duplicate or unsafe member")
            seen.add(name)
            require(member.isdir() or member.isfile() or (members is not None and name not in members), "A selected archive member is not a regular file")
            total += member.size
            require(total <= maximum and len(seen) <= 30000, "Archive exceeds its expansion bound")
            if member.isdir() or members is not None and name not in members:
                continue
            target = dest / name
            target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            stream = archive.extractfile(member)
            require(stream is not None, "Archive member is unavailable")
            data = stream.read(member.size + 1)
            require(len(data) == member.size, "Archive member size changed")
            write_new(target, data, 0o755 if member.mode & 0o111 else 0o600)
            written.add(name)
    require(members is None or written == set(members), "Archive omitted a required executable")


def package_state(p, name):
    require(re.fullmatch(r"[a-z0-9][a-z0-9+.-]*", name), "Invalid package identity")
    text, code = command([p["distroTools"]["dpkg-query"]["path"], "-W", "-f=${Status}\t${Version}\t${Architecture}", name], allowed=(0, 1))
    if code == 1:
        return None
    fields = text.split("\t")
    require(len(fields) == 3 and fields[0] == "install ok installed" and fields[2] in ("amd64", "all"), "An existing dependency is partially installed or foreign")
    return fields[1]


def dependencies(p, manifest):
    rows = manifest["ubuntuPackages"]
    require(len(rows) == len(PACKAGES) + 1 and {r["Package"] for r in rows} == PACKAGES | {"nftables"}, "The closed package allowlist changed")
    indexed = {r["Package"]: r for r in rows}
    for name, row in indexed.items():
        require(row["Architecture"] == "amd64" and row["operation"] == ("extract-only-nft-executable" if name == "nftables" else "install-only-if-absent"), "An unapproved package operation was requested")
    ordered = []
    visited = set()
    def visit(name):
        require(name not in visited, "Cyclic package dependency")
        visited.add(name)
        row = indexed[name]
        for dep in row["Depends"].split(","):
            match = re.fullmatch(r"\s*([a-z0-9+.-]+)(?: \((>=|=|>>|<=|<<) ([^() ]+)\))?\s*", dep)
            require(match is not None, "Unknown package dependency form")
            other, relation, version = match.groups()
            have = package_state(p, other)
            if have is None:
                require(other in PACKAGES and other != "uidmap", "A platform dependency is missing; no implicit system upgrade is allowed")
                if other not in ordered:
                    visit(other)
                have = indexed[other]["Version"]
            if relation:
                _, code = command([p["distroTools"]["dpkg"]["path"], "--compare-versions", have, relation, version], allowed=(0, 1))
                require(code == 0, "An installed dependency would require an upgrade or replacement")
        if name != "nftables" and package_state(p, name) is None and name not in ordered:
            ordered.append(name)
        visited.remove(name)
    visit("nftables")
    if package_state(p, "uidmap") is None:
        visit("uidmap")
    return ordered


def package_snapshot(p,manifest):
    names={r["Package"] for r in manifest["ubuntuPackages"]}
    for row in manifest["ubuntuPackages"]:
        for dep in row["Depends"].split(","):
            match=re.match(r"\s*([a-z0-9+.-]+)",dep)
            require(match is not None,"Unknown package dependency")
            names.add(match.group(1))
    require(len(names)<=64,"Package observation exceeds its bound")
    return {name:package_state(p,name) for name in sorted(names)}


def inspect_deb(p, row, path, stage):
    tool = p["distroTools"]["dpkg-deb"]["path"]
    text, _ = command([tool, "--field", str(path), "Package", "Version", "Architecture"])
    fields = dict(line.split(": ", 1) for line in text.splitlines())
    require(all(fields.get(k) == row[k] for k in ("Package", "Version", "Architecture")), "Package identity differs from the pinned archive")
    raw,_ = command([tool, "--ctrl-tarfile", str(path)],maximum=1<<20,binary=True)
    scripts = {}
    with tarfile.open(fileobj=io.BytesIO(raw)) as tar:
        for member in tar:
            name = member.name.removeprefix("./")
            if name in ("preinst", "postinst", "prerm", "postrm", "triggers"):
                require(member.isfile() and member.size <= 65536 and not any(k.removeprefix("./")==name for k in scripts), "Unbounded or duplicate package hook")
                scripts[member.name] = tar.extractfile(member).read().decode()
    require(hashlib.sha256(json.dumps(scripts, sort_keys=True,separators=(',', ':')).encode()).hexdigest() == row["maintainerScriptsSHA256"], "Package maintainer scripts differ from the reviewed bytes")
    if row["Package"] != "nftables":
        require(not any(k.removeprefix('./') != "triggers" for k in scripts), "Unexpected package maintainer command")
        require(all(x.strip() in ("", "activate-noawait ldconfig") or x.lstrip().startswith("#") for x in scripts.get("./triggers",scripts.get("triggers", "")).splitlines()), "Unexpected system trigger")
    else:
        raw,_ = command([tool, "--fsys-tarfile", str(path)],maximum=32<<20,binary=True)
        archive = stage / "nft-files.tar"
        write_new(archive, raw)
        extract_tar(archive, stage / "nft", {"usr/sbin/nft"}, 32 << 20)


def preflight(p):
    text, _ = command([p["distroTools"]["rootlesskit"]["path"], "--help"])
    require(all(flag in text for flag in ROOTLESS_FLAGS), "Existing RootlessKit lacks a required fixed option")
    # An on-disk profile or generic unshare cannot prove the executable's loaded
    # policy. The installed collector and actual service fixtures decide it.
    require(not Path("/etc/ld.so.preload").exists() and not Path("/etc/ld.so.preload").is_symlink(), "Global loader preload requires separate review")
    text, _ = command([p["distroTools"]["dpkg"]["path"], "--audit"])
    require(not text.strip(), "Package-manager state needs repair before setup")
    text, _ = command([p["distroTools"]["dpkg-query"]["path"], "-W", "-f=${Triggers-Pending}${Triggers-Awaited}\n"], maximum=4 << 20)
    require(not text.strip(), "Pending system triggers must settle before setup")
    cfg = [Path("/etc/dpkg/dpkg.cfg")]
    cfg.extend(sorted(Path("/etc/dpkg/dpkg.cfg.d").glob("*")))
    for path in cfg:
        if path.is_file():
            for line in immutable(path).read_text().splitlines():
                require(not re.match(r"\s*(?:--)?(?:pre|post)-invoke(?:\s|=|$)", line), "A package-manager execution hook requires separate review")


def wrapper(p, args, phase):
    source_files = {k:v for k,v in p["sourceFiles"].items() if k == "scripts/lib/host_build_guard.py" or k.startswith("cockpit/")}
    plan = {"schemaVersion":1,"sourceSHA":p["sourceSHA"],"sourceFiles":source_files,"systemctl":p["distroTools"]["systemctl"],"worker":p["worker"],"fixtures":FIXTURES,"idleReceipt":p["idleReceipt"],"go":{"path":str(args.output / "go/go/bin/go"),"sha256":sha(args.output / "go/go/bin/go")}}
    path = args.output / "qualification-plan.json"
    if phase == "prepare":
        save(path, plan)
    else:
        require(read_json(path) == plan, "Prepared qualification selection changed")
    env = {k:v for k,v in os.environ.items() if k in ("HOME","USER","LOGNAME","XDG_RUNTIME_DIR","DBUS_SESSION_BUS_ADDRESS")}
    text, _ = command([sys.executable,"-I",str(args.source / (SUPPORT + "qualify.py")),"--phase="+phase,"--plan="+str(path),"--source="+str(args.source),"--output="+str(args.output / "qualification"),"--go="+plan["go"]["path"]], timeout=3100, env=env)
    result = json.loads(text)
    require(result.get("ok") is True, "Qualification wrapper refused; retain its journal before any recovery")
    return result


def prepare(p, manifest, args):
    global CHANGED
    private_directory(args.output.parent,p["owner"]["uid"])
    require(os.geteuid() == p["owner"]["uid"] and not args.output.exists(), "Prepare once as the selected nonroot owner into a new directory")
    args.output.mkdir(mode=0o700)
    CHANGED=True
    preflight(p)
    ordered = dependencies(p, manifest)
    packages=package_snapshot(p,manifest)
    downloads = manifest["downloads"]
    require(len(downloads) == 3 and {x["name"] for x in downloads} == {"go","buildkit","slirp4netns"}, "Unexpected upstream artifact")
    records = {}
    for item in downloads:
        name = item["name"]
        target = args.output / (name + ".download")
        download(item,target)
        records[name] = {"path":str(target),"sha256":sha(target)}
        if name == "go":
            extract_tar(target,args.output / "go")
        elif name == "buildkit":
            require(item.get("selectedMembers") == ["bin/buildkitd","bin/buildctl"], "Unexpected BuildKit members")
            extract_tar(target,args.output / "buildkit",set(item["selectedMembers"]))
    for row in manifest["ubuntuPackages"]:
        if row["Package"] not in ordered and row["Package"] != "nftables":
            continue
        target = args.output / (row["Package"] + ".deb")
        download(row,target)
        inspect_deb(p,row,target,args.output)
        records[row["Package"]] = {"path":str(target),"sha256":sha(target)}
    built = wrapper(p,args,"prepare")
    state = read_json(args.output / "qualification/setup-state.json")
    require(state["binaries"]==manifest["expectedBuiltBinaries"],"Built helper/fixture bytes differ from the reviewed reproducible build")
    result = {"schemaVersion":1,"sourceSHA":p["sourceSHA"],"planSHA256":args.plan_sha256,"artifacts":records,"missingPackages":ordered,"packageStates":packages,"binaries":state["binaries"],"wrapper":built}
    save(args.output / "prepared.json",result)
    return {"phase":"prepared","receipt":str(args.output / "prepared.json"),"workerStopped":False}


def elf_interpreter(path):
    with open(path,"rb") as f:
        header=f.read(64)
        require(len(header)==64 and header[:6]==b"\x7fELF\x02\x01", "A selected tool is not a Linux64 little-endian ELF")
        offset=struct.unpack_from("<Q",header,32)[0]
        size,count=struct.unpack_from("<HH",header,54)
        require(size==56 and count<=128, "Unbounded ELF program table")
        f.seek(offset)
        table=f.read(size*count)
        require(len(table)==size*count,"Truncated ELF program table")
        for i in range(count):
            entry=table[i*size:(i+1)*size]
            if struct.unpack_from("<I",entry)[0]==3:
                at,n=struct.unpack_from("<Q",entry,8)[0],struct.unpack_from("<Q",entry,32)[0]
                require(1<n<=1024,"Unbounded ELF interpreter")
                f.seek(at);raw=f.read(n)
                require(len(raw)==n and raw.endswith(b"\0") and b"\0" not in raw[:-1],"Malformed ELF interpreter")
                return str(canonical(raw[:-1].decode()))
    return None


def runtime_closure(tools):
    aliases={};files={}
    require(not Path("/etc/ld.so.preload").exists() and not Path("/etc/ld.so.preload").is_symlink(),"Global preload is unsupported")
    cache=immutable("/etc/ld.so.cache");cache_hash=sha(cache)
    files[str(cache)]=cache_hash
    for tool in tools:
        interpreter=elf_interpreter(tool)
        if interpreter is None: continue
        loader=resolve_runtime_path(interpreter,aliases);files[str(loader)]=sha(loader)
        text,_=command([str(loader),"--list",str(tool)],timeout=10)
        for line in text.splitlines():
            if "linux-vdso.so." in line and "=>" not in line: continue
            match=re.fullmatch(r"\s*(?:[^\s]+ => )?(/[^\s]+) \(0x[0-9a-f]+\)\s*",line)
            require(match is not None,"Unknown dynamic loader dependency observation")
            target=resolve_runtime_path(match.group(1),aliases);files[str(target)]=sha(target)
    require(len(files)>=2 and len(files)<=128 and len(aliases)<=256 and sha(cache)==cache_hash,"Runtime dependency closure is missing, excessive or changed")
    return files,aliases


def root_directory(path):
    global CHANGED
    p=canonical(path)
    missing=[]
    for current in (p,*p.parents):
        if not current.exists() and not current.is_symlink(): missing.append(current);continue
        s=current.lstat()
        require(stat.S_ISDIR(s.st_mode) and s.st_uid==0 and not s.st_mode&0o022,"A new installation parent is mutable or symbolic")
    for current in reversed(missing):
        CHANGED=True
        current.mkdir(mode=0o755)


def installation_payloads(p,manifest,args):
    chosen={"buildkitd":args.output / "buildkit/bin/buildkitd","buildctl":args.output / "buildkit/bin/buildctl","slirp4netns":args.output / "slirp4netns.download","nft":args.output / "nft/usr/sbin/nft","helper":args.output / "qualification/memql-imageverify"}
    # Compare one payload read to the genuine official member or reproducible
    # build identity, then copy those same bytes into the root-owned directory.
    buildkit=next(x for x in manifest["downloads"] if x["name"]=="buildkit")
    slirp=next(x for x in manifest["downloads"] if x["name"]=="slirp4netns")
    nft=next(x for x in manifest["ubuntuPackages"] if x["Package"]=="nftables")
    payloads={"buildkitd":buildkit["selectedMemberIdentities"]["bin/buildkitd"],"buildctl":buildkit["selectedMemberIdentities"]["bin/buildctl"],"slirp4netns":{"sha256":slirp["sha256"],"bytes":slirp["size"]},"nft":nft["selectedMemberIdentities"]["usr/sbin/nft"],"helper":{"sha256":manifest["expectedBuiltBinaries"]["memql-imageverify"]}}
    payload_bytes={}
    for name,source in chosen.items():
        regular(source,128<<20,p["owner"]["uid"])
        data=source.read_bytes();identity=payloads[name]
        require(len(data)<=128<<20 and hashlib.sha256(data).hexdigest()==identity["sha256"] and ("bytes" not in identity or len(data)==identity["bytes"]),"Selected installation payload differs from its reviewed bytes")
        payload_bytes[name]=data
    return payload_bytes


def install(p,manifest,args):
    global CHANGED
    require(os.geteuid()==0 and args.confirm=="install-reviewed-image-verification", "Root installation requires the explicit reviewed confirmation")
    require(os.environ.get("SUDO_UID")==str(p["owner"]["uid"]) and os.environ.get("SUDO_GID")==str(p["owner"]["gid"]),"Root setup was not invoked by the selected owner")
    private_directory(args.output,p["owner"]["uid"])
    preflight(p)
    prepared=read_json(args.output / "prepared.json")
    require(prepared["planSHA256"]==args.plan_sha256 and prepared["sourceSHA"]==p["sourceSHA"],"Prepared source or plan changed")
    state=read_json(args.output / "qualification/setup-state.json")
    require(state["phase"]=="prepared" and state["binaries"]==prepared["binaries"],"The normal preparation did not finish")
    require(package_snapshot(p,manifest)==prepared["packageStates"],"Installed package identities changed since preparation")
    require(dependencies(p,manifest)==prepared["missingPackages"],"Installed dependencies changed since preparation")
    wanted={x.get("name",x.get("Package")):x for x in manifest["downloads"]+manifest["ubuntuPackages"] if x.get("name") or x.get("Package") in prepared["missingPackages"]+["nftables"]}
    require(set(prepared["artifacts"])==set(wanted),"Prepared official artifact set changed")
    for name,row in wanted.items():
        record=prepared["artifacts"][name];expected=row.get("sha256",row.get("SHA256"))
        require(record["path"]==str(args.output / (name + (".download" if name in {"go","buildkit","slirp4netns"} else ".deb"))),"Prepared artifact path changed")
        require(record["sha256"]==expected and sha(regular(record["path"],160<<20,p["owner"]["uid"]))==expected,"Prepared artifact changed")
    require(prepared["binaries"]==manifest["expectedBuiltBinaries"],"Prepared binaries differ from the reviewed exact build")
    for name,expected in prepared["binaries"].items(): require(sha(regular(args.output / "qualification" / name,128<<20,p["owner"]["uid"]))==expected,"Prepared helper or fixture changed")
    fixed=[Path("/etc/memql/image-verification.json"),Path("/etc/memql/image-verification-qualified.json"),Path("/etc/systemd/system/memql-imageverify.socket"),Path("/etc/systemd/system/memql-imageverify@.service")]
    require(not any(f.exists() or f.is_symlink() for f in fixed),"An image-verification installation already exists; it will not be overwritten")
    toolkit=Path("/opt/memql/image-verification") / p["sourceSHA"]
    require(not toolkit.exists() and not toolkit.is_symlink(),"The fixed toolkit destination already exists")
    attempt=canonical(p["attemptRoot"])
    require(not attempt.exists() and not attempt.is_symlink(),"The new private attempt root already exists")
    for unit in ("memql-imageverify.socket","memql-imageverify@.service"):
        text,_=command([p["distroTools"]["systemctl"]["path"],"--system","show",unit,"--property=LoadState,ActiveState,FragmentPath,DropInPaths,Transient","--no-pager"])
        current=dict(line.split("=",1) for line in text.splitlines() if "=" in line)
        require(current.get("LoadState")=="not-found" and current.get("ActiveState")=="inactive" and current.get("FragmentPath","")=="" and current.get("DropInPaths","")=="", "An existing loaded service/socket would conflict")
    payload_bytes=installation_payloads(p,manifest,args)
    save(args.output / "install.intent.json",{"schemaVersion":1,"planSHA256":args.plan_sha256,"sourceSHA":p["sourceSHA"],"packages":prepared["missingPackages"]})
    # Privileged consumers never re-open mutable owner staging. Copy each
    # verified package snapshot into a new root-owned directory first.
    root_directory(toolkit)
    package_paths=[]
    for name in prepared["missingPackages"]:
        source=args.output / (name+".deb")
        data=regular(source,160<<20,p["owner"]["uid"]).read_bytes()
        expected=wanted[name]["SHA256"]
        require(hashlib.sha256(data).hexdigest()==expected,"Package changed before root staging")
        target=toolkit / (name+".deb")
        write_new(target,data,0o600);package_paths.append(str(target))
    if package_paths:
        command([p["distroTools"]["dpkg"]["path"],"--install",*package_paths],timeout=180)
    installed={}
    for name,data in payload_bytes.items():
        target=toolkit / name
        write_new(target,data,0o755)
        installed[name]={"path":str(target),"sha256":sha(target)}
    for name in ("rootlesskit","runc","git","systemctl","ip"):
        installed[name]=p["distroTools"][name]
    for name in ("newuidmap","newgidmap"):
        path=immutable("/usr/bin/"+name,True)
        require(path.stat().st_mode&stat.S_ISUID,"Subordinate-ID helper lacks its reviewed setuid prerequisite")
        installed[name]={"path":str(path),"sha256":sha(path)}
    files,aliases=runtime_closure([x["path"] for x in installed.values()])
    root_directory(attempt.parent);attempt.mkdir(mode=0o700);os.chown(attempt,p["owner"]["uid"],p["owner"]["gid"])
    config={"ownerUID":p["owner"]["uid"],"ownerGID":p["owner"]["gid"],"helper":installed.pop("helper"),"tools":installed,"runtimeFiles":files,"runtimeAliases":aliases,"attemptRoot":str(attempt),"workspaceRoot":p["workspaceRoot"],"socketPath":"/run/memql-imageverify/"+str(p["owner"]["uid"])+".sock",**p["limits"]}
    text,_=command([config["helper"]["path"],"--render-image-verification-installation"],input=json.dumps(config).encode())
    rendered=json.loads(text)
    require(set(rendered)=={"schemaVersion","config","socketUnit","serviceUnit"} and rendered["schemaVersion"]==1 and rendered["config"]==config,"The pure renderer did not accept the exact installation")
    for path,data in [(fixed[0],(json.dumps(config,sort_keys=True)+"\n").encode()),(fixed[2],rendered["socketUnit"].encode()),(fixed[3],rendered["serviceUnit"].encode())]:
        root_directory(path.parent);write_new(path,data,0o644)
    command([p["distroTools"]["systemctl"]["path"],"--system","daemon-reload"])
    command([p["distroTools"]["systemctl"]["path"],"--system","start","memql-imageverify.socket"])
    result={"schemaVersion":1,"sourceSHA":p["sourceSHA"],"configSHA256":sha(fixed[0]),"unitSHA256":sha(fixed[3]),"socketSHA256":sha(fixed[2]),"runtimeFiles":files,"runtimeAliases":aliases,"packagesInstalled":prepared["missingPackages"],"qualificationPending":True}
    save(args.output / "installed.json",result,mode=0o644)
    return {"phase":"installed","receipt":str(args.output / "installed.json"),"qualificationPending":True,"workerStopped":False}


class Parameters(argparse.ArgumentParser):
    def error(self, message):
        raise ValueError(message)


def check_installed_before_stop(p,manifest,args):
    config=read_json(immutable("/etc/memql/image-verification.json"),65536)
    helper=config["helper"]
    require(helper["sha256"]==manifest["expectedBuiltBinaries"]["memql-imageverify"] and sha(immutable(helper["path"],True))==helper["sha256"],"The installed helper identity changed")
    text,_=command([helper["path"],"--check-image-verification-installation"],timeout=60,maximum=65536)
    value=json.loads(text)
    require(set(value)=={"schemaVersion","installationSHA256","host","qualified"} and value["schemaVersion"]==1 and HASH.fullmatch(value["installationSHA256"]) and value["qualified"] is False,"The installed-service observation is not the expected read-only preflight")
    save(args.output / "preflight.json",value)
    return value


def attest(p,manifest,args):
    require(os.geteuid()==0 and args.confirm=="publish-verified-image-qualification","Attestation publication requires the explicit fixed confirmation")
    require(os.environ.get('SUDO_UID')==str(p['owner']['uid']) and os.environ.get('SUDO_GID')==str(p['owner']['gid']),"Run the final root step from the same selected owner")
    private_directory(args.output,p['owner']['uid'])
    target=Path('/etc/memql/image-verification-qualified.json')
    require(not target.exists() and not target.is_symlink(),"An existing attestation is never overwritten")
    installed=read_json(args.output/'installed.json')
    config_path=immutable('/etc/memql/image-verification.json')
    require(installed['sourceSHA']==p['sourceSHA'] and installed['configSHA256']==sha(config_path),"The installed source or configuration changed")
    config=read_json(config_path,65536);helper=config['helper']
    require(helper['sha256']==manifest['expectedBuiltBinaries']['memql-imageverify'] and sha(immutable(helper['path'],True))==helper['sha256'],"Installed validation helper changed")
    output=args.output/'qualification'
    state=read_json(output/'setup-state.json')
    require(state.get('phase')=='complete' and state.get('result',{}).get('cleanupConfirmed') is True and state['binaries']==manifest['expectedBuiltBinaries'],"Real qualification, cleanup and worker restoration must complete first")
    qplan=read_json(args.output/'qualification-plan.json')
    require(qplan['sourceSHA']==p['sourceSHA'] and qplan['fixtures']==FIXTURES and qplan['worker']==p['worker'],"Qualification selection changed")
    # Reuse the already-reviewed result/evidence joins under the actual owner.
    # Only hash-verified source bytes are loaded; this is not a generic script
    # or privileged callback port.
    validator='''import hashlib,json,pathlib,sys,types
raw=pathlib.Path(sys.argv[1]).read_bytes()
if hashlib.sha256(raw).hexdigest()!=sys.argv[2]:raise SystemExit("validator changed")
m=types.ModuleType("reviewed_qualification");m.__file__=sys.argv[1]
exec(compile(raw,sys.argv[1],"exec"),m.__dict__)
p=json.loads(pathlib.Path(sys.argv[3]).read_bytes())
print(json.dumps(m.validate_result(p,pathlib.Path(sys.argv[4]))))
'''
    account=(p['owner']['uid'],p['owner']['gid'])
    text,_=command([sys.executable,'-I','-c',validator,str(args.source/(SUPPORT+'qualify.py')),p['sourceFiles'][SUPPORT+'qualify.py'],str(args.output/'qualification-plan.json'),str(output)],owner=account,maximum=65536)
    result=json.loads(text)
    require(result==state['result'] and result['helperSHA256']==manifest['expectedBuiltBinaries']['memql-imageverify'] and result['testBinarySHA256']==manifest['expectedBuiltBinaries']['imageverify.test'],"Final evidence no longer matches the completed native fixture")
    raw=regular(output/'qualification.json',65536,p['owner']['uid']).read_bytes()
    candidate=json.loads(raw)
    tuples=lambda rows:{(r['name'],r['result'],r['evidenceSHA256']) for r in rows}
    require(candidate['installationSHA256']==result['installationSHA256'] and len(candidate['fixtures'])==8 and tuples(candidate['fixtures'])==tuples(result['fixtures']),"The retained candidate no longer matches the real receipts")
    expected=hashlib.sha256(raw).hexdigest()
    text,_=command([helper['path'],'--validate-image-verification-attestation'],input=raw,owner=account,timeout=60,maximum=65536)
    verified=json.loads(text)
    require(verified=={'schemaVersion':1,'installationSHA256':result['installationSHA256'],'attestationSHA256':expected,'valid':True},"The existing Go validator refused the current host or exact candidate")
    require(sha(config_path)==installed['configSHA256'],"The installation changed before publication")
    root_directory(target.parent);write_new(target,raw,0o644)
    require(sha(immutable(target))==expected and sha(config_path)==installed['configSHA256'],"Attestation readback or installation changed")
    receipt={'schemaVersion':1,'sourceSHA':p['sourceSHA'],'installationSHA256':result['installationSHA256'],'attestationSHA256':expected,'resultSHA256':sha(output/'qualification-result.json'),'target':str(target),'fixtures':FIXTURES}
    save(args.output/'attested.json',receipt,mode=0o644)
    return {'phase':'attested','receipt':str(args.output/'attested.json'),'sourceSHA':p['sourceSHA'],'installationSHA256':result['installationSHA256'],'attestationSHA256':expected,'controllerActivationPerformed':False}


def main():
    global CHANGED
    args=None
    try:
        parser=Parameters(description=__doc__)
        parser.add_argument("--phase",choices=("plan","prepare","install","qualify","attest"),required=True)
        parser.add_argument("--plan",type=Path,required=True)
        parser.add_argument("--plan-sha256")
        parser.add_argument("--source",type=Path,required=True)
        parser.add_argument("--output",type=Path,required=True)
        parser.add_argument("--confirm",default="")
        parser.add_argument("--source-sha")
        parser.add_argument("--source-archive",type=Path)
        parser.add_argument("--source-archive-sha256")
        parser.add_argument("--worker-unit")
        parser.add_argument("--worker-unit-file",type=Path)
        parser.add_argument("--workspace-root",type=Path)
        parser.add_argument("--host-id")
        parser.add_argument("--host-version")
        parser.add_argument("--idle-observation",help="Reviewed compact JSON observation; not an automatic idle claim")
        args=parser.parse_args()
        for path in (args.plan,args.source,args.output): canonical(path)
        if args.phase=="plan": result=make_plan(args)
        else:
            require(not any((args.source_sha,args.source_archive,args.source_archive_sha256,args.worker_unit,args.worker_unit_file,args.workspace_root,args.host_id,args.host_version,args.idle_observation)),"Plan-generation fields are invalid for a later phase")
            p,manifest=load_plan(args)
            if args.phase=="prepare": result=prepare(p,manifest,args)
            elif args.phase=="install": result=install(p,manifest,args)
            elif args.phase=="attest": result=attest(p,manifest,args)
            else:
                require(os.geteuid()==p["owner"]["uid"],"Qualification must run as the selected owner")
                installed=read_json(args.output / "installed.json")
                require(installed["sourceSHA"]==p["sourceSHA"] and installed["configSHA256"]==sha(immutable("/etc/memql/image-verification.json")),"Installed selection changed")
                check_installed_before_stop(p,manifest,args)
                CHANGED=True
                result={"phase":"qualified","wrapper":wrapper(p,args,"qualify"),"attestationInstallationPending":True}
        print(json.dumps({"ok":True,"capability":CAPABILITY,"changed":CHANGED or args.phase=="qualify","result":result,"error":None}))
        return 0
    except (Exception,KeyboardInterrupt) as error:
        message=str(error) or "Setup interrupted"
        code=5 if CHANGED else (2 if isinstance(error,(ValueError,TypeError)) else 3)
        print("MemQL: "+message+". Preserve the setup journal; no automatic retry was made.",file=sys.stderr)
        print(json.dumps({"ok":False,"capability":CAPABILITY,"changed":CHANGED,"result":{},"error":{"code":code,"message":message}}))
        return code


if __name__=="__main__":
    sys.exit(main())
