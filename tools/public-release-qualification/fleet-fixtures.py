#!/usr/bin/env python3
"""Run existing Fleet command/UI fixtures with synthetic tokens and fresh HOME.
Default is plan only; never executes generated commands or enrolls a worker.
"""
import argparse,json,os,shutil,subprocess,tempfile
from pathlib import Path

TESTS=["test/fleet/localInstall.test.ts","test/fleet/localInstallUI.test.tsx","test/fleet/addMachineInstall.test.tsx","test/fleet/addMachineFlow.test.ts","test/fleet/addMachineIntent.test.tsx","test/fleet/addMachine.test.tsx"]

def main():
 p=argparse.ArgumentParser(description=__doc__)
 p.add_argument('--source-tree',type=Path,required=True)
 p.add_argument('--source-commit',required=True)
 p.add_argument('--execute',action='store_true')
 a=p.parse_args();root=a.source_tree.resolve();osroot=root/'clients/os'
 for test in TESTS:
  if not (osroot/test).is_file():raise ValueError('Missing existing fixture: '+test)
 command=['npm','test','--',*TESTS]
 if not a.execute:
  print(json.dumps({'planned':True,'directory':str(osroot),'command':command,'uses':'synthetic test tokens only; generated install commands never executed'},indent=2));return
 head=subprocess.check_output(['git','-C',str(root),'rev-parse','HEAD'],text=True).strip()
 if head!=a.source_commit:raise ValueError('Source tree does not match reviewed release commit')
 npm=shutil.which('npm');node=shutil.which('node')
 if not npm or not node or not (osroot/'node_modules/.bin/vitest').exists():raise ValueError('Existing Node/npm/Vitest installation required; no dependency installation performed')
 with tempfile.TemporaryDirectory(prefix='memql-fleet-fixtures-') as temp:
  home=Path(temp);(home/'tmp').mkdir()
  env={'HOME':temp,'TMPDIR':str(home/'tmp'),'PATH':str(Path(node).parent)+':'+str(Path(npm).parent)+':/usr/bin:/bin:/usr/sbin:/sbin','LANG':'C','CI':'1','MEMQL_COCKPIT_CRED_STORE':'file'}
  run=subprocess.run([npm,*command[1:]],cwd=osroot,env=env,text=True,capture_output=True,timeout=600)
  print(json.dumps({'ok':run.returncode==0,'returncode':run.returncode,'sourceCommit':head,'fixtures':TESTS,'stdout':run.stdout,'stderr':run.stderr},indent=2))
  if run.returncode:raise SystemExit(5)
if __name__=='__main__':
 try:main()
 except Exception as exc:print(json.dumps({'ok':False,'error':str(exc)}));raise SystemExit(5)
