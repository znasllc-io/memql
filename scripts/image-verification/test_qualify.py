import copy
import importlib.util
import json
import io
import os
import signal
from pathlib import Path
import subprocess
import sys
import tempfile
import tarfile
import time
import unittest
from unittest import mock

import qualify as q
import prepare_go

GUARD_PATH = Path(__file__).resolve().parent.parent / "lib/host_build_guard.py"


class WrapperTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="memql-wrapper-offline-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.names = ["fixture" + str(i) for i in range(8)]
        self.plan = {"sourceSHA": "a" * 40, "fixtures": self.names}
        self.result = {"schemaVersion": 1, "sourceSHA": "a" * 40,
                       "installationSHA256": "b" * 64, "passed": True, "cleanupConfirmed": True,
                       "fixtures": [{"name": n, "result": "passed", "evidenceSHA256": str(i) * 64}
                                    for i, n in enumerate(self.names)],
                       "candidateAttestationPath": str(self.root / "qualification.json")}
        for item in self.result["fixtures"]:
            evidence = self.root / (item["name"] + ".json")
            evidence.write_text(json.dumps({"fixture":item["name"]}))
            item["evidencePath"] = str(evidence)
            item["evidenceSHA256"] = q.digest(evidence)
        for field, name in (("helperSHA256", "memql-imageverify"), ("testBinarySHA256", "imageverify.test")):
            (self.root / name).write_text(name)
            self.result[field] = q.digest(self.root / name)
        (self.root / "qualification.json").write_text(json.dumps({"schemaVersion":1,"profile":"rootless-buildkit-v1","installationSHA256":"b"*64,"fixtures":self.result["fixtures"]}))

    def write_result(self, result):
        (self.root / "qualification-result.json").write_text(json.dumps(result))

    def test_exact_success_requires_cleanup_and_complete_evidence(self):
        self.write_result(self.result)
        self.assertEqual(q.validate_result(self.plan, self.root), self.result)
        changes = [dict(passed=False), dict(cleanupConfirmed=False), dict(sourceSHA="c" * 40),
                   dict(installationSHA256=""), dict(schemaVersion=2), dict(fixtures=self.result["fixtures"][:-1]),
                   dict(fixtures=self.result["fixtures"][:7] + self.result["fixtures"][:1]),
                   dict(candidateAttestationPath=str(self.root.parent / "outside.json"))]
        for delta in changes:
            with self.subTest(delta=delta):
                result = dict(self.result, **delta)
                self.write_result(result)
                with self.assertRaises(q.Refused):
                    q.validate_result(self.plan, self.root)

    def test_candidate_symlink_and_missing_file_refuse(self):
        self.write_result(self.result)
        candidate = self.root / "qualification.json"
        candidate.unlink()
        for linked in (False, True):
            with self.subTest(linked=linked):
                if linked:
                    candidate.symlink_to(self.root / "qualification-result.json")
                with self.assertRaises((q.Refused, FileNotFoundError)):
                    q.validate_result(self.plan, self.root)

    def test_evidence_and_tool_bytes_are_bound(self):
        self.write_result(self.result)
        for name in ("fixture0.json", "memql-imageverify", "imageverify.test"):
            path = self.root / name
            before = path.read_bytes()
            path.write_bytes(before + b"changed")
            with self.assertRaises(q.Refused):
                q.validate_result(self.plan, self.root)
            path.write_bytes(before)

    def test_unknown_or_nonobject_result_refuses(self):
        for raw in ("null", "[]", "not JSON", "{}"):
            (self.root / "qualification-result.json").write_text(raw)
            with self.assertRaises((q.Refused, ValueError)):
                q.validate_result(self.plan, self.root)

    def test_pending_source_plan_never_executes_tool(self):
        p = self.root / "pending.json"
        p.write_text(json.dumps({"schemaVersion": 1, "sourceSHA": None}))
        with mock.patch.object(q.subprocess, "run") as run:
            with self.assertRaises(q.Refused):
                q.load_plan(p, self.root)
            run.assert_not_called()

    def test_shared_guard_executes_only_verified_source_bytes(self):
        path = self.root / "scripts/lib/host_build_guard.py"
        path.parent.mkdir(parents=True)
        path.write_text("VALUE = 123\n")
        (path.parent / "__pycache__").mkdir()
        (path.parent / "__pycache__/host_build_guard.pyc").write_bytes(b"untrusted cache")
        plan = {"sourceFiles":{"scripts/lib/host_build_guard.py":q.digest(path)}}
        self.assertEqual(q.load_guard(plan, self.root).VALUE, 123)
        path.write_text("raise RuntimeError('must not execute changed source')\n")
        with self.assertRaises(q.Refused):
            q.load_guard(plan, self.root)

    def test_stopped_unit_identity_and_cgroup_are_required(self):
        prior = {"ControlGroup": "", "UnitFileState": "enabled"}
        current = {"ActiveState": "inactive", "MainPID": "0", "UnitFileState": "enabled"}
        with mock.patch.object(q, "worker_snapshot", return_value=current):
            q.stopped({}, prior)
        for key, value in (("ActiveState", "active"), ("MainPID", "10"), ("UnitFileState", "disabled")):
            with mock.patch.object(q, "worker_snapshot", return_value=dict(current, **{key: value})):
                with self.assertRaises(q.Refused):
                    q.stopped({}, prior)
        with mock.patch.object(q, "worker_snapshot", return_value=current):
            with self.assertRaises(q.Refused):
                q.stopped({}, dict(prior, ControlGroup="/system.slice/foreign"))

    def test_marker_and_descriptor_are_preserved_on_child_failure(self):
        guardpath = GUARD_PATH
        spec = importlib.util.spec_from_file_location("guard_fixture", guardpath)
        guard = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(guard)
        capacity = self.root / "capacity"
        capacity.mkdir()
        with mock.patch.object(guard, "capacity_root", return_value=capacity), mock.patch.object(guard, "disk_preflight", return_value=99 << 30):
            with guard.host_build_guard() as fd:
                record = {"kind":"local-build-v1","token":"test","pid":os.getpid(),"workspace":str(self.root)}
                guard.write_marker(capacity, record)
                with open(self.root / "run.log", "xb") as log:
                    # Real child verifies descriptor inheritance; no host unit,
                    # Docker, network, or account-capacity path is touched.
                    code = "import os,sys; os.fstat(int(sys.argv[1])); sys.exit(7)"
                    with self.assertRaises(q.Refused):
                        q.run([sys.executable, "-c", code, str(fd)], {}, self.root, log, guard, fd, record, 5)
                self.assertEqual(guard.read_marker(capacity), record)
                self.assertGreater(record["processGroup"], 0)
            with self.assertRaises(guard.GuardError):
                with guard.host_build_guard():
                    self.fail("uncertain child permitted a new build")

    def test_exited_leader_does_not_leave_its_descendant_running(self):
        spec = importlib.util.spec_from_file_location("descendant_guard", GUARD_PATH)
        guard = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(guard)
        capacity = self.root / "capacity"
        capacity.mkdir()
        code = """import os,signal,time,pathlib,sys
root=pathlib.Path(sys.argv[1])
pid=os.fork()
if pid:
    while not (root/'ready').exists(): time.sleep(.01)
    sys.exit(0)
def stop(sig,frame):
    (root/'terminated').write_text(str(sig))
    sys.exit(0)
signal.signal(signal.SIGTERM,stop)
(root/'ready').write_text(str(os.getpid()))
time.sleep(30)
(root/'escaped').write_text('bad')
"""
        with mock.patch.object(guard, "capacity_root", return_value=capacity), mock.patch.object(guard, "disk_preflight", return_value=99 << 30):
            with guard.host_build_guard() as fd:
                record = {"kind":"local-build-v1","token":"test","pid":os.getpid()}
                guard.write_marker(capacity, record)
                try:
                    with open(self.root / "descendant.log", "xb") as log:
                        with self.assertRaises(q.Refused):
                            q.run([sys.executable,"-c",code,str(self.root)],{},self.root,log,guard,fd,record,5)
                    self.assertEqual((self.root / "terminated").read_text(),str(int(signal.SIGTERM)))
                    self.assertFalse((self.root / "escaped").exists())
                    self.assertEqual(guard.read_marker(capacity),record)
                finally:
                    try:
                        os.killpg(record["processGroup"],signal.SIGKILL)
                    except ProcessLookupError:
                        pass

    def test_loaded_settings_ignore_only_command_outcome_metadata(self):
        current = {k:"" for k in q.LOADED_PROPERTIES}
        current.update(DropInPaths="",Transient="no",NeedDaemonReload="no",ExecStart="{ path=/opt/memql ; argv[]=/opt/memql worker ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }")
        first=q.loaded_settings_digest(current)
        nextrow=dict(current,ExecStart=current['ExecStart'].replace('pid=0','pid=999').replace('start_time=[n/a]','start_time=[Mon 2026-10-26 20:00:00 UTC]'))
        self.assertEqual(first,q.loaded_settings_digest(nextrow))
        for key,value in (("Environment","TOKEN=changed"),("NoNewPrivileges","no"),("ExecStart",current['ExecStart'].replace('/opt/memql worker','/opt/memql other'))):
            self.assertNotEqual(first,q.loaded_settings_digest(dict(current,**{key:value})))
        for key,value in (("DropInPaths","/tmp/override.conf"),("Transient","yes"),("NeedDaemonReload","yes"),("ExecStart","unknown command encoding")):
            with self.assertRaises(q.Refused):q.loaded_settings_digest(dict(current,**{key:value}))

    def test_capture_baseline_then_refuse_changed_unit_without_preapproved_hash(self):
        unit = self.root / "memql.service"; unit.write_text("[Service]\nExecStart=/opt/memql worker\n")
        plan = {"worker":{"unit":"memql.service","unitFile":{"path":str(unit)}}}
        current = {k:"" for k in q.LOADED_PROPERTIES}
        current.update(Id="memql.service",ActiveState="active",SubState="running",MainPID="123",FragmentPath=str(unit),ControlGroup="",UnitFileState="enabled",KillMode="control-group",DropInPaths="",Transient="no",NeedDaemonReload="no",ExecStart="{ path=/opt/memql ; argv[]=/opt/memql worker ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=123 ; code=(null) ; status=0/0 }")
        def output(): return "\n".join(k+"="+v for k,v in current.items())
        with mock.patch.object(q,"systemctl",side_effect=lambda *a:output()):
            baseline=q.worker_snapshot(plan)
            self.assertEqual(q.worker_snapshot(plan,baseline),baseline)
            self.assertNotIn("ExecStart",baseline)
            unit.write_text("changed")
            with self.assertRaises(q.Refused):q.worker_snapshot(plan,baseline)
            unit.write_text("[Service]\nExecStart=/opt/memql worker\n")
            current["Environment"]="CHANGED=true"
            with self.assertRaises(q.Refused):q.worker_snapshot(plan,baseline)

    def test_point_in_time_observation_keeps_timestamp_and_refuses_old_setup(self):
        import datetime
        observed=datetime.datetime.now(datetime.timezone.utc)-datetime.timedelta(minutes=20)
        path=self.root/"observation.json"
        row={"workerUnit":"memql.service","ownerUID":os.getuid(),"activeWork":0,"observedAt":observed.isoformat()}
        path.write_text(json.dumps(row));plan={"worker":{"unit":"memql.service"},"idleReceipt":{"path":str(path),"sha256":q.digest(path)}}
        retained=q.point_in_time_idle(plan)
        self.assertTrue(retained['pointInTimeOnly']);self.assertEqual(retained['observedAt'],row['observedAt'])
        for key,value in (("activeWork",1),("observedAt",(observed-datetime.timedelta(hours=2)).isoformat()),("workerUnit","other.service")):
            path.write_text(json.dumps(dict(row,**{key:value})));plan['idleReceipt']['sha256']=q.digest(path)
            with self.assertRaises(q.Refused):q.point_in_time_idle(plan)

    def test_worker_descendants_refuse_before_any_stop(self):
        group="/user.slice/user-"+str(os.getuid())+".slice/user@1000.service/app.slice/memql.service"
        cg=self.root/group.lstrip('/');cg.mkdir(parents=True)
        current={"ControlGroup":group,"ActiveState":"active","MainPID":"123"}
        (cg/'cgroup.procs').write_text('123\n')
        q.no_worker_descendants(current,self.root)
        nested=cg/'child';nested.mkdir();(nested/'cgroup.procs').write_text('456\n')
        with self.assertRaises(q.Refused):q.no_worker_descendants(current,self.root)
        prior={"ActiveState":"active","UnitFileState":"enabled"};state={"phase":"prepared"};sp=self.root/'state'
        q.save(sp,state)
        with mock.patch.object(q,'point_in_time_idle',return_value={'pointInTimeOnly':True}),mock.patch.object(q,'worker_snapshot',return_value=dict(current,UnitFileState='enabled')),mock.patch.object(q,'no_worker_descendants',side_effect=q.Refused('child')),mock.patch.object(q,'systemctl') as ctl:
            with self.assertRaises(q.Refused):q.stop_for_qualification({'worker':{'unit':'memql.service'}},prior,sp,state)
            ctl.assert_not_called();self.assertNotIn('stopRequestedAt',q.read_json(sp))

    def test_stop_is_immediately_preceded_by_checks_and_durable_intent(self):
        prior={"ActiveState":"active","UnitFileState":"enabled"};state={"phase":"prepared"};sp=self.root/'state';q.save(sp,state)
        calls=[]
        def ctl(*a):
            self.assertEqual(q.read_json(sp)['phase'],'stopping_worker');calls.append('stop')
        with mock.patch.object(q,'point_in_time_idle',side_effect=lambda p:calls.append('observation') or {'pointInTimeOnly':True}),mock.patch.object(q,'worker_snapshot',side_effect=lambda *a:calls.append('unit') or prior),mock.patch.object(q,'no_worker_descendants',side_effect=lambda c:calls.append('children')),mock.patch.object(q,'systemctl',side_effect=ctl),mock.patch.object(q,'stopped',side_effect=lambda *a:calls.append('empty')):
            q.stop_for_qualification({'worker':{'unit':'memql.service'}},prior,sp,state)
        self.assertEqual(calls,['observation','unit','children','stop','empty'])
        self.assertTrue(q.read_json(sp)['workerStopped'])

    def test_stop_error_reports_uncertain_effect_and_retains_intent(self):
        import contextlib
        from types import SimpleNamespace
        spec=importlib.util.spec_from_file_location("stop_error_guard",GUARD_PATH)
        guard=importlib.util.module_from_spec(spec);spec.loader.exec_module(guard)
        capacity=self.root/'capacity';capacity.mkdir()
        output=self.root/'output';output.mkdir(mode=0o700)
        source=self.root/'source';source.mkdir();go=self.root/'go';go.write_text('tool')
        planpath=self.root/'plan';planpath.write_text('{}')
        plan={'go':{'path':str(go)},'worker':{'unit':'memql.service'},'sourceSHA':'a'*40}
        prior={'ActiveState':'active','UnitFileState':'enabled'}
        record={'kind':'local-build-v1','token':'fixture','pid':os.getpid()}
        guard.write_marker(capacity,record)
        state={'phase':'prepared','planSHA256':q.digest(planpath),'reservation':record,'priorWorker':prior,'binaries':{}}
        q.save(output/'setup-state.json',state)
        stdout=io.StringIO();stderr=io.StringIO()
        args=['qualify.py','--phase=qualify','--plan='+str(planpath),'--source='+str(source),'--output='+str(output),'--go='+str(go)]
        with contextlib.ExitStack() as stack:
            stack.enter_context(mock.patch.object(sys,'argv',args));stack.enter_context(mock.patch.object(sys,'platform','linux'))
            stack.enter_context(mock.patch.object(q.os,'uname',return_value=SimpleNamespace(machine='x86_64')))
            stack.enter_context(mock.patch.object(q,'load_plan',return_value=plan));stack.enter_context(mock.patch.object(q,'load_guard',return_value=guard))
            stack.enter_context(mock.patch.object(q.subprocess,'run',return_value=SimpleNamespace(returncode=0,stdout=b'go version go1.27.3 linux/amd64')))
            stack.enter_context(mock.patch.object(guard,'capacity_root',return_value=capacity));stack.enter_context(mock.patch.object(guard,'disk_preflight',return_value=99<<30))
            stack.enter_context(mock.patch.object(q,'point_in_time_idle',return_value={'pointInTimeOnly':True}));stack.enter_context(mock.patch.object(q,'worker_snapshot',return_value=prior));stack.enter_context(mock.patch.object(q,'no_worker_descendants'))
            stack.enter_context(mock.patch.object(q,'systemctl',side_effect=TimeoutError('stop reply unavailable')))
            stack.enter_context(contextlib.redirect_stdout(stdout));stack.enter_context(contextlib.redirect_stderr(stderr))
            self.assertEqual(q.main(),5)
        envelope=json.loads(stdout.getvalue());self.assertTrue(envelope['changed']);self.assertEqual(envelope['error']['code'],5)
        retained=q.read_json(output/'setup-state.json');self.assertEqual(retained['phase'],'stopping_worker');self.assertTrue(retained['stopRequestedAt'])
        self.assertEqual(guard.read_marker(capacity),record)

    def test_prepare_keeps_worker_running_and_ignores_persisted_go_environment(self):
        import contextlib
        from types import SimpleNamespace
        spec=importlib.util.spec_from_file_location("prepare_guard",GUARD_PATH)
        guard=importlib.util.module_from_spec(spec);spec.loader.exec_module(guard)
        capacity=self.root/'capacity';capacity.mkdir();output=self.root/'output';output.mkdir(mode=0o700)
        source=self.root/'source';source.mkdir();go=self.root/'go';go.write_text('tool')
        planpath=self.root/'plan';planpath.write_text('{}')
        plan={'go':{'path':str(go)},'worker':{'unit':'memql.service'},'sourceSHA':'a'*40,'sourceFiles':{}}
        prior={'ActiveState':'active','UnitFileState':'enabled'};envs=[]
        def version(*a,**kw):
            self.assertEqual(kw['env']['GOENV'],'off');return SimpleNamespace(returncode=0,stdout=b'go version go1.27.3 linux/amd64')
        def build(command,env,*args):
            envs.append(env);Path(command[command.index('-o')+1]).write_text('binary')
        args=['qualify.py','--phase=prepare','--plan='+str(planpath),'--source='+str(source),'--output='+str(output),'--go='+str(go)]
        with contextlib.ExitStack() as stack:
            stack.enter_context(mock.patch.object(sys,'argv',args));stack.enter_context(mock.patch.object(sys,'platform','linux'))
            stack.enter_context(mock.patch.object(q.os,'uname',return_value=SimpleNamespace(machine='x86_64')))
            stack.enter_context(mock.patch.object(q,'load_plan',return_value=plan));stack.enter_context(mock.patch.object(q,'load_guard',return_value=guard))
            stack.enter_context(mock.patch.object(q.subprocess,'run',side_effect=version));stack.enter_context(mock.patch.object(q,'run',side_effect=build))
            stack.enter_context(mock.patch.object(guard,'capacity_root',return_value=capacity));stack.enter_context(mock.patch.object(guard,'disk_preflight',return_value=99<<30))
            stack.enter_context(mock.patch.object(q,'point_in_time_idle',return_value={'pointInTimeOnly':True}));stack.enter_context(mock.patch.object(q,'worker_snapshot',return_value=prior))
            ctl=stack.enter_context(mock.patch.object(q,'systemctl'));stack.enter_context(contextlib.redirect_stdout(io.StringIO()));stack.enter_context(contextlib.redirect_stderr(io.StringIO()))
            self.assertEqual(q.main(),0);ctl.assert_not_called()
        self.assertEqual(len(envs),2)
        for env in envs:self.assertEqual(env['GOENV'],'off')
        state=q.read_json(output/'setup-state.json');self.assertEqual(state['phase'],'prepared');self.assertFalse(state['workerStopped']);self.assertNotIn('stopRequestedAt',state)

    def test_atomic_state_creation_refuses_replay(self):
        target = self.root / "state.json"
        q.save(target, {"phase": "prepared"})
        with self.assertRaises(FileExistsError):
            q.save(target, {"phase": "new"})
        self.assertEqual(q.read_json(target), {"phase": "prepared"})
        self.assertEqual(target.stat().st_mode & 0o777, 0o600)

    def test_no_worker_start_without_cleanup_or_unchanged_unit(self):
        plan = {"worker":{"unit":"memql.service"}}
        state = {"phase":"qualifying","result":{"cleanupConfirmed":False}}
        with mock.patch.object(q, "systemctl") as call:
            with self.assertRaises(q.Refused):
                q.settle(None, {}, self.root / "state", state, plan, {"ActiveState":"active"})
            call.assert_not_called()

    def test_success_resumes_only_prior_active_unchanged_worker(self):
        spec = importlib.util.spec_from_file_location("settle_guard", GUARD_PATH)
        guard = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(guard)
        for prior_active, changed_unit in ((True, False), (False, False), (True, True)):
            with self.subTest(prior_active=prior_active, changed_unit=changed_unit):
                capacity = self.root / ("settle-" + str(prior_active) + str(changed_unit))
                capacity.mkdir()
                record = {"kind":"local-build-v1","token":"fixture","pid":os.getpid()}
                guard.write_marker(capacity, record)
                statepath = capacity / "state.json"
                state = {"phase":"cleanup_confirmed","result":{"cleanupConfirmed":True}}
                q.save(statepath, state)
                with mock.patch.object(guard, "capacity_root", return_value=capacity), mock.patch.object(q, "stopped", side_effect=q.Refused("changed unit") if changed_unit else None), mock.patch.object(q, "systemctl") as call, mock.patch.object(q, "worker_snapshot", return_value={"ActiveState":"active"}):
                    if changed_unit:
                        with self.assertRaises(q.Refused):
                            q.settle(guard, record, statepath, state, {"worker":{"unit":"memql.service"}}, {"ActiveState":"active"})
                        self.assertEqual(guard.read_marker(capacity), record)
                        call.assert_not_called()
                    else:
                        q.settle(guard, record, statepath, state, {"worker":{"unit":"memql.service"}}, {"ActiveState":"active" if prior_active else "inactive"})
                        self.assertFalse((capacity / guard.MARKER_NAME).exists())
                        self.assertEqual(call.call_count, 1 if prior_active else 0)
                        self.assertEqual(q.read_json(statepath)["phase"], "complete")

    def test_go_archive_digest_refuses_before_extraction(self):
        archive = self.root / "go.tar.gz"
        archive.write_bytes(b"not the pinned toolchain")
        dest = self.root / "toolchain"
        with self.assertRaises(ValueError):
            prepare_go.extract(archive, dest)
        self.assertFalse(dest.exists())

    def test_go_extraction_refuses_escape_alias_and_duplicate(self):
        for kind in ("valid", "escape", "alias", "duplicate"):
            with self.subTest(kind=kind):
                archive = self.root / (kind + ".tar.gz")
                with tarfile.open(archive, "w:gz") as out:
                    item = tarfile.TarInfo("go/bin/go" if kind != "escape" else "go/../../escaped")
                    item.size = 3
                    item.mode = 0o755
                    if kind == "alias":
                        item.type = tarfile.SYMTYPE
                        item.linkname = "/outside"
                        item.size = 0
                    out.addfile(item, io.BytesIO(b"go!"))
                    if kind == "duplicate":
                        out.addfile(item, io.BytesIO(b"go!"))
                dest = self.root / (kind + "-extract")
                with mock.patch.object(prepare_go, "SIZE", archive.stat().st_size), mock.patch.object(prepare_go, "SHA256", q.digest(archive)):
                    if kind == "valid":
                        result = prepare_go.extract(archive, dest)
                        self.assertEqual(result["sha256"], q.digest(dest / "go/bin/go"))
                    else:
                        with self.assertRaises(ValueError):
                            prepare_go.extract(archive, dest)
                        self.assertFalse((dest / "go/bin/go").exists())
                self.assertFalse((self.root / "escaped").exists())


if __name__ == "__main__":
    unittest.main()
