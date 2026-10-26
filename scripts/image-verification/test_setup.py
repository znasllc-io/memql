"""Offline controls only; no real download, package, worker or service action."""
import argparse
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('image_setup',Path(__file__).with_name('setup.py'))
s=importlib.util.module_from_spec(spec);spec.loader.exec_module(s)

class SetupTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve();s.CHANGED=False
    def tar(self,entries,name='in.tar'):
        p=self.root/name
        with tarfile.open(p,'w') as out:
            for name,data,kind in entries:
                i=tarfile.TarInfo(name);i.mode=0o755
                if kind=='dir':i.type=tarfile.DIRTYPE;out.addfile(i)
                elif kind=='link':i.type=tarfile.SYMTYPE;i.linkname='/etc/passwd';out.addfile(i)
                else:i.size=len(data);out.addfile(i,io.BytesIO(data))
        return p
    def test_selected_archive_members_are_bounded_and_links_never_extracted(self):
        p=self.tar([('./',b'','dir'),('bin/tool',b'approved','file'),('usr/share/man/link',b'','link')])
        s.extract_tar(p,self.root/'out',{'bin/tool'})
        self.assertEqual((self.root/'out/bin/tool').read_bytes(),b'approved')
        self.assertFalse((self.root/'out/usr').exists())
        for entries in [ [('bin/tool',b'','link')], [('bin/tool',b'a','file'),('bin/tool',b'b','file')], [('../tool',b'a','file')], [('other',b'a','file')] ]:
            with self.subTest(entries=entries), tempfile.TemporaryDirectory(dir=self.root) as d:
                p=self.tar(entries,Path(d).name+'.tar')
                with self.assertRaises(s.Refused):s.extract_tar(p,Path(d)/'out',{'bin/tool'})
    def test_source_inventory_rejects_wrong_archive_missing_and_changed_content(self):
        commit='a'*40;source=self.root/'source';source.mkdir()
        files=['scripts/lib/host_build_guard.py','cockpit/cmd/memql-imageverify/main.go',s.SUPPORT+'setup.py',s.SUPPORT+'qualify.py',s.SUPPORT+'official-artifacts.json']+[f'cockpit/internal/worker/imageverify/{n}.go' for n in ('a','b','c')]
        entries=[]
        for name in files:
            p=source/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_bytes(name.encode());entries.append(('memql-'+commit+'/'+name,name.encode(),'file'))
        tar=self.tar(entries)
        self.assertEqual(len(s.source_inventory(tar,s.sha(tar),source,commit)),8)
        with self.assertRaises(s.Refused):s.source_inventory(tar,'0'*64,source,commit)
        with self.assertRaises(s.Refused):s.source_inventory(tar,s.sha(tar),source,'b'*40)
        (source/files[-1]).write_bytes(b'drift')
        with self.assertRaises(s.Refused):s.source_inventory(tar,s.sha(tar),source,commit)
    def test_package_selection_is_missing_only_and_never_nft_install(self):
        rows=[{'Package':n,'Architecture':'amd64','Version':'1','Depends':'base (>= 1)','operation':'extract-only-nft-executable' if n=='nftables' else 'install-only-if-absent'} for n in sorted(s.PACKAGES|{'nftables'})]
        p={'distroTools':{'dpkg':{'path':'/fixed/dpkg'}}};manifest={'ubuntuPackages':rows}
        with patch.object(s,'package_state',side_effect=lambda p,n:None if n=='uidmap' else '1'),patch.object(s,'command',return_value=('',0)):
            self.assertEqual(s.dependencies(p,manifest),['uidmap'])
        with patch.object(s,'package_state',return_value=None):
            with self.assertRaises(s.Refused):s.dependencies(p,manifest)
        with patch.object(s,'package_state',return_value='0'),patch.object(s,'command',return_value=('',1)):
            with self.assertRaises(s.Refused):s.dependencies(p,manifest)
        rows[0]['operation']='upgrade'
        with self.assertRaises(s.Refused):s.dependencies(p,manifest)
    def test_hooks_require_exact_digest_and_no_installable_scripts(self):
        def control(items):
            out=io.BytesIO()
            with tarfile.open(fileobj=out,mode='w') as tar:
                for name,text in items.items():
                    i=tarfile.TarInfo(name);data=text.encode();i.size=len(data);tar.addfile(i,io.BytesIO(data))
            return out.getvalue()
        row={'Package':'uidmap','Version':'1','Architecture':'amd64','maintainerScriptsSHA256':hashlib.sha256(b'{}').hexdigest()}
        p={'distroTools':{'dpkg-deb':{'path':'/fixed/dpkg-deb'}}}
        fields='Package: uidmap\nVersion: 1\nArchitecture: amd64\n'
        with patch.object(s,'command',side_effect=[(fields,0),(control({}),0)]):s.inspect_deb(p,row,self.root/'x',self.root)
        hooks={'./postinst':'#!/bin/sh\ntrue\n'}
        row['maintainerScriptsSHA256']=hashlib.sha256(json.dumps(hooks,sort_keys=True,separators=(',',':')).encode()).hexdigest()
        with patch.object(s,'command',side_effect=[(fields,0),(control(hooks),0)]):
            with self.assertRaises(s.Refused):s.inspect_deb(p,row,self.root/'x',self.root)
    def test_archive_download_digest_and_oversize_refuse(self):
        class Response(io.BytesIO):status=200
        class Opener:
            def __init__(self,data):self.data=data
            def open(self,*a,**k):return Response(self.data)
        item={'url':'https://github.com/org/repo/releases/download/v1/file','size':3,'sha256':hashlib.sha256(b'abc').hexdigest()}
        with patch.object(s.urllib.request,'build_opener',return_value=Opener(b'abc')):s.download(item,self.root/'good')
        for data in (b'abd',b'abcd',b'ab'):
            with self.subTest(data=data),patch.object(s.urllib.request,'build_opener',return_value=Opener(data)):
                with self.assertRaises(s.Refused):s.download(item,self.root/data.hex())
        for url in ('http://github.com/a','https://evil.example/a','https://u:p@github.com/a'):
            with self.assertRaises(s.Refused):s.safe_url(url)
    def test_private_state_and_json_refuse_symlink_duplicate_or_overwrite(self):
        p=self.root/'j';p.write_text('{"a":1,"a":2}')
        with self.assertRaises(s.Refused):s.read_json(p)
        link=self.root/'l';link.symlink_to(p)
        with self.assertRaises(s.Refused):s.regular(link,100)
        with self.assertRaises(FileExistsError):s.write_new(p,b'new')
        self.root.chmod(0o700);self.assertEqual(s.private_directory(self.root,os.getuid()),self.root)
        self.root.chmod(0o755)
        with self.assertRaises(s.Refused):s.private_directory(self.root,os.getuid())
    def test_command_success_failure_timeout_and_descendant_cleanup(self):
        text,code=s.command([sys.executable,'-c','print("ok")']);self.assertEqual((text,code),('ok\n',0))
        with self.assertRaises(s.Refused):s.command([sys.executable,'-c','raise SystemExit(2)'])
        start=time.monotonic()
        with self.assertRaises(s.Refused):s.command([sys.executable,'-c','import time; time.sleep(10)'],timeout=.1)
        self.assertLess(time.monotonic()-start,4)
        pidfile=self.root/'child'
        program='import os,time\np=os.fork()\nif p==0:\n open('+repr(str(pidfile))+',"w").write(str(os.getpid()))\n time.sleep(30)\nelse:\n time.sleep(.2)\n'
        with self.assertRaises(s.Refused):s.command([sys.executable,'-c',program],timeout=3)
        pid=int(pidfile.read_text())
        out=subprocess.run(['ps','-o','stat=','-p',str(pid)],capture_output=True,text=True,timeout=3)
        self.assertTrue(out.returncode!=0 or out.stdout.strip().startswith('Z'),out.stdout)
    def test_elf_interpreter_is_explicit_bounded_and_rejects_malformed(self):
        import struct
        data=bytearray(256);data[:6]=b'\x7fELF\x02\x01';struct.pack_into('<Q',data,32,64);struct.pack_into('<HH',data,54,56,1);struct.pack_into('<I',data,64,3);struct.pack_into('<Q',data,72,128)
        target=b'/lib64/loader.so\0';struct.pack_into('<Q',data,96,len(target));data[128:128+len(target)]=target
        p=self.root/'elf';p.write_bytes(data);self.assertEqual(s.elf_interpreter(p),'/lib64/loader.so')
        struct.pack_into('<Q',data,96,5000);p.write_bytes(data)
        with self.assertRaises(s.Refused):s.elf_interpreter(p)
    def test_cli_bad_parameters_have_one_json_result_and_no_effects(self):
        result=subprocess.run([sys.executable,str(Path(s.__file__)),'--bogus'],capture_output=True,text=True,timeout=3)
        self.assertEqual(result.returncode,2);value=json.loads(result.stdout);self.assertFalse(value['ok']);self.assertFalse(value['changed'])
        help=subprocess.run([sys.executable,str(Path(s.__file__)),'--help'],capture_output=True,text=True,timeout=3)
        self.assertEqual(help.returncode,0);self.assertIn('--phase',help.stdout)

    def test_root_payloads_use_exact_snapshots_of_reviewed_members(self):
        rows={'buildkitd':('buildkit/bin/buildkitd',b'bd'),'buildctl':('buildkit/bin/buildctl',b'bc'),'slirp4netns':('slirp4netns.download',b'sl'),'nft':('nft/usr/sbin/nft',b'nft'),'helper':('qualification/memql-imageverify',b'helper')}
        ids={}
        for name,(path,data) in rows.items():
            target=self.root/path;target.parent.mkdir(parents=True,exist_ok=True);target.write_bytes(data)
            ids[name]={'sha256':hashlib.sha256(data).hexdigest(),'bytes':len(data)}
        manifest={'downloads':[{'name':'buildkit','selectedMemberIdentities':{'bin/buildkitd':ids['buildkitd'],'bin/buildctl':ids['buildctl']}},{'name':'slirp4netns','sha256':ids['slirp4netns']['sha256'],'size':2}], 'ubuntuPackages':[{'Package':'nftables','selectedMemberIdentities':{'usr/sbin/nft':ids['nft']}}], 'expectedBuiltBinaries':{'memql-imageverify':ids['helper']['sha256']}}
        args=argparse.Namespace(output=self.root);p={'owner':{'uid':os.getuid()}}
        snapshot=s.installation_payloads(p,manifest,args)
        self.assertEqual(snapshot['helper'],b'helper')
        (self.root/rows['helper'][0]).write_bytes(b'replaced')
        self.assertEqual(snapshot['helper'],b'helper')
        with self.assertRaises(s.Refused):s.installation_payloads(p,manifest,args)
        (self.root/rows['helper'][0]).write_bytes(b'helper')
        (self.root/rows['nft'][0]).write_bytes(b'replaced')
        with self.assertRaises(s.Refused):s.installation_payloads(p,manifest,args)
    def test_runtime_closure_preserves_aliases_and_refuses_cache_drift(self):
        cache=self.root/'cache';cache.write_bytes(b'cache')
        loader=self.root/'loader';loader.write_bytes(b'loader')
        lib=self.root/'lib';lib.write_bytes(b'library')
        def resolve(path,aliases):
            aliases['/lib64/loader.so']='../usr/lib/loader.so'
            return loader if path=='/lib64/loader.so' else lib
        with patch.object(s,'immutable',return_value=cache),patch.object(s,'elf_interpreter',return_value='/lib64/loader.so'),patch.object(s,'resolve_runtime_path',side_effect=resolve),patch.object(s,'command',return_value=('/usr/lib/lib.so (0xabc)\n',0)):
            files,aliases=s.runtime_closure(['tool'])
            self.assertEqual(set(files),{str(cache),str(loader),str(lib)})
            self.assertIn('/lib64/loader.so',aliases)
        def drift(*args,**kw):
            cache.write_bytes(b'changed');return '/usr/lib/lib.so (0xabc)\n',0
        with patch.object(s,'immutable',return_value=cache),patch.object(s,'elf_interpreter',return_value='/lib64/loader.so'),patch.object(s,'resolve_runtime_path',side_effect=resolve),patch.object(s,'command',side_effect=drift):
            with self.assertRaises(s.Refused):s.runtime_closure(['tool'])
    def test_new_installation_refuses_existing_targets_without_replacement(self):
        path=self.root/'target';path.write_bytes(b'existing')
        with self.assertRaises(FileExistsError):s.write_new(path,b'replacement')
        self.assertEqual(path.read_bytes(),b'existing')
        self.assertFalse(s.CHANGED)
    def test_plan_refuses_root_and_missing_commit(self):
        args=argparse.Namespace(source_sha='not-a-commit',source_archive=None)
        with patch.object(s.os,'getuid',return_value=0),patch.object(s.os,'geteuid',return_value=0):
            with self.assertRaises(s.Refused):s.make_plan(args)
        with self.assertRaises(s.Refused):s.make_plan(args)

    def test_plan_binds_real_owner_source_tools_and_caller_observation(self):
        from types import SimpleNamespace
        for bad in (None,'stale','active'):
            with self.subTest(bad=bad),tempfile.TemporaryDirectory() as d:
                stage=Path(d).resolve();stage.chmod(0o700)
                source=stage/'source';source.mkdir();support=source/s.SUPPORT;support.mkdir(parents=True)
                (support/'official-artifacts.json').write_text('{}')
                unit=stage/'.config/systemd/user/memql-worker.service';unit.parent.mkdir(parents=True);unit.write_text('ExecStart=/fixed/memql worker run\n')
                workspace=stage/'work';workspace.mkdir()
                tool=stage/'tool';tool.write_text('fixed')
                account=SimpleNamespace(pw_gid=os.getgid(),pw_dir=str(stage))
                idle={'observedAt':(s.dt.datetime.now(s.dt.timezone.utc)-s.dt.timedelta(hours=2 if bad=='stale' else 0)).isoformat(),'activeWork':1 if bad=='active' else 0}
                args=argparse.Namespace(source_sha='a'*40,source_archive=stage/'source.tar',source_archive_sha256='b'*64,source=source,plan=stage/'plan.json',output=stage/'output',plan_sha256=None,confirm='',host_id='ubuntu',host_version='24.04',worker_unit=unit.name,worker_unit_file=unit,workspace_root=workspace,idle_observation=json.dumps(idle))
                real_read=Path.read_text
                def read(path,*a,**kw):
                    if str(path)=='/etc/os-release':return 'ID=ubuntu\nVERSION_ID="24.04"\n'
                    return real_read(path,*a,**kw)
                real_resolve=Path.resolve
                def resolve(path,*a,**kw):
                    if str(path).startswith('/usr/bin/'):return path
                    return real_resolve(path,*a,**kw)
                real_sha=s.sha
                def digest(path):
                    if str(path).startswith('/usr/bin/'):return 'f'*64
                    return real_sha(path)
                inventory={s.SUPPORT+'setup.py':real_sha(Path(s.__file__))}
                with patch.object(s.sys,'platform','linux'),patch.object(s.os,'uname',return_value=SimpleNamespace(machine='x86_64')),patch.object(s.pwd,'getpwuid',return_value=account),patch.object(Path,'read_text',read),patch.object(Path,'resolve',resolve),patch.object(s.shutil,'which',side_effect=lambda name,path:'/usr/bin/'+name),patch.object(s,'immutable',side_effect=lambda path,*a:Path(path)),patch.object(s,'sha',side_effect=digest),patch.object(s,'source_inventory',return_value=inventory),patch.object(s,'selected_workspace',return_value=(workspace,{'unitSHA256':real_sha(unit),'policyPath':str(stage/'policy.yaml'),'policySHA256':None})):
                    if bad:
                        with self.assertRaises(s.Refused):s.make_plan(args)
                        self.assertFalse(args.plan.exists())
                    else:
                        result=s.make_plan(args);plan=json.loads(args.plan.read_text())
                        self.assertEqual(result['planSHA256'],real_sha(args.plan));self.assertEqual(plan['sourceArchiveSHA256'],'b'*64)
                        self.assertEqual(plan['owner']['uid'],os.getuid());self.assertEqual(plan['worker']['unit'],unit.name)
                        captured=json.loads(Path(plan['idleReceipt']['path']).read_text());self.assertEqual(captured['observedAt'],idle['observedAt']);self.assertEqual(captured['ownerUID'],os.getuid())
                        self.assertFalse(result['workerStopped'])
    def test_installed_preflight_cannot_claim_qualification_or_change_helper(self):
        config={'helper':{'path':str(self.root/'helper'),'sha256':'a'*64}}
        manifest={'expectedBuiltBinaries':{'memql-imageverify':'a'*64}}
        args=argparse.Namespace(output=self.root)
        observed={'schemaVersion':1,'installationSHA256':'b'*64,'host':{'kernelRelease':'fixture'},'qualified':False}
        with patch.object(s,'immutable',side_effect=lambda path,*a:Path(path)),patch.object(s,'read_json',return_value=config),patch.object(s,'sha',return_value='a'*64),patch.object(s,'command',return_value=(json.dumps(observed),0)) as invoke:
            self.assertEqual(s.check_installed_before_stop({},manifest,args),observed)
            self.assertEqual(invoke.call_args.args[0],[config['helper']['path'],'--check-image-verification-installation'])
        observed['qualified']=True
        with patch.object(s,'immutable',side_effect=lambda path,*a:Path(path)),patch.object(s,'read_json',return_value=config),patch.object(s,'sha',return_value='a'*64),patch.object(s,'command',return_value=(json.dumps(observed),0)):
            with self.assertRaises(s.Refused):s.check_installed_before_stop({},manifest,args)

    def test_workspace_uses_documented_default_and_fences_policy_changes(self):
        from types import SimpleNamespace
        home=self.root/'home';home.mkdir();unit=home/'unit';unit.write_text('ExecStart=/fixed/memql worker run\n')
        account=SimpleNamespace(pw_dir=str(home))
        root,witness=s.selected_workspace(unit,account)
        self.assertEqual(root,home/'.memql/pipelines')
        p={'owner':{'uid':os.getuid()},'worker':{'unitFile':{'path':str(unit)}},'workspaceWitness':witness}
        s.check_workspace_witness(p)
        policy=home/'.memql/policy.yaml';policy.parent.mkdir();policy.write_text('pipelines: {}\n')
        with self.assertRaises(s.Refused):s.check_workspace_witness(p)
        with patch.dict(sys.modules,{'yaml':None}):
            with self.assertRaises(s.Refused):s.selected_workspace(unit,account)
            explicit=home/'reviewed';chosen,proof=s.selected_workspace(unit,account,explicit)
            self.assertEqual(chosen,explicit);self.assertEqual(proof['policySHA256'],s.sha(policy))
        for command in ('/fixed/memql worker run --unknown=x','/fixed/memql worker run --config=/other/worker.yaml','/fixed/memql worker run --config=%h/worker.yaml','/fixed/memql worker run --workers=$HOME/workers.yaml'):
            unit.write_text('ExecStart='+command+'\n')
            with self.assertRaises(s.Refused):s.selected_workspace(unit,account)
        unit.write_text('ExecStart=/fixed/memql worker run\nEnvironment=MEMQL_WORKER_TOKEN=opaque-fixture\n')
        with self.assertRaises(s.Refused):s.selected_workspace(unit,account)
    def test_final_attestation_reuses_validator_and_writes_only_exact_passed_snapshot(self):
        import copy
        from contextlib import ExitStack
        owner=os.getuid();group=os.getgid();out=self.root/'qualification';out.mkdir()
        fixtures=[{'name':n,'result':'passed','evidenceSHA256':'f'*64} for n in s.FIXTURES]
        candidate={'schemaVersion':1,'profile':'rootless-buildkit-v1','installationSHA256':'b'*64,'fixtures':fixtures}
        raw=json.dumps(candidate).encode();(out/'qualification.json').write_bytes(raw)
        digest=hashlib.sha256(raw).hexdigest()
        result={'sourceSHA':'c'*40,'helperSHA256':'a'*64,'testBinarySHA256':'d'*64,'installationSHA256':'b'*64,'cleanupConfirmed':True,'fixtures':fixtures}
        binaries={'memql-imageverify':'a'*64,'imageverify.test':'d'*64}
        args=argparse.Namespace(output=self.root,source=self.root,confirm='publish-verified-image-qualification')
        p={'owner':{'uid':owner,'gid':group},'sourceSHA':'c'*40,'worker':{'unit':'memql-worker.service'},'sourceFiles':{s.SUPPORT+'qualify.py':'e'*64}}
        config={'helper':{'path':'/fixed/helper','sha256':'a'*64}}
        state={'phase':'complete','result':result,'binaries':binaries}
        def records(path,*a):
            name=Path(path).name
            return copy.deepcopy({'installed.json':{'sourceSHA':'c'*40,'configSHA256':'1'*64},'image-verification.json':config,'setup-state.json':state,'qualification-plan.json':{'sourceSHA':'c'*40,'fixtures':s.FIXTURES,'worker':p['worker']}}[name])
        def hashes(path):
            return 'a'*64 if str(path)=='/fixed/helper' else (digest if Path(path).name=='image-verification-qualified.json' else '1'*64)
        verified={'schemaVersion':1,'installationSHA256':'b'*64,'attestationSHA256':digest,'valid':True}
        for failure in (None,'incomplete','changed-validator','failed-host'):
            with self.subTest(failure=failure),ExitStack() as stack:
                current=copy.deepcopy(state)
                if failure=='incomplete':state['phase']='uncertain'
                stack.enter_context(patch.object(s.os,'geteuid',return_value=0));stack.enter_context(patch.dict(os.environ,{'SUDO_UID':str(owner),'SUDO_GID':str(group)}))
                stack.enter_context(patch.object(s,'private_directory'));stack.enter_context(patch.object(s,'immutable',side_effect=lambda path,*a:Path(path)));stack.enter_context(patch.object(s,'read_json',side_effect=records));stack.enter_context(patch.object(s,'sha',side_effect=hashes));stack.enter_context(patch.object(s,'root_directory'));stack.enter_context(patch.object(s,'save'))
                calls=[(json.dumps(result),0),(json.dumps(dict(verified,valid=False)) if failure=='failed-host' else json.dumps(verified),0)]
                command=stack.enter_context(patch.object(s,'command',side_effect=calls));write=stack.enter_context(patch.object(s,'write_new'))
                manifest={'expectedBuiltBinaries':dict(binaries)}
                if failure=='changed-validator':manifest['expectedBuiltBinaries']['memql-imageverify']='0'*64
                if failure:
                    with self.assertRaises(s.Refused):s.attest(p,manifest,args)
                    write.assert_not_called()
                else:
                    value=s.attest(p,manifest,args);self.assertEqual(value['phase'],'attested')
                    write.assert_called_once_with(Path('/etc/memql/image-verification-qualified.json'),raw,0o644)
                    self.assertEqual(command.call_args.kwargs['owner'],(owner,group));self.assertEqual(command.call_args.kwargs['input'],raw)
                state.clear();state.update(current)

if __name__=='__main__':unittest.main()
