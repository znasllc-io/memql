import os, pathlib, tempfile, subprocess, plistlib, json
# Test the shipped installer with only launchctl replaced by a local stub.
# All writes target a temporary user home; no installed worker is invoked.
import argparse
parser=argparse.ArgumentParser()
parser.add_argument('--archive', required=True, type=pathlib.Path)
archive=parser.parse_args().archive.resolve()
with tempfile.TemporaryDirectory(prefix='cockpit-installer-') as root:
 root=pathlib.Path(root); unpack=root/'unpack'; unpack.mkdir()
 subprocess.run(['tar','-xzf',str(archive),'-C',str(unpack)],check=True)
 user=root/'user & space'; user.mkdir(); shim=root/'shim'; shim.mkdir()
 state=root/'agent-state'; calls=root/'calls'
 launch=shim/'launchctl'; launch.write_text('''#!/bin/bash
printf '%s\\n' "$*" >> "$MEMQL_TEST_CALLS"
case "$1" in
 print) test -f "$MEMQL_TEST_STATE" ;;
 bootstrap) touch "$MEMQL_TEST_STATE" ;;
 bootout) rm -f "$MEMQL_TEST_STATE" ;;
 kickstart) exit 0 ;;
 *) exit 98 ;;
esac
''');launch.chmod(0o755)
 env=dict(os.environ,HOME=str(user),PATH=str(shim)+':'+os.environ['PATH'],MEMQL_TEST_STATE=str(state),MEMQL_TEST_CALLS=str(calls))
 cmd=['bash',str(unpack/'scripts/macos/install-menubar.sh'),'--app='+str(unpack/'MemQL Cockpit.app')]
 a=subprocess.run(cmd,env=env,check=True,text=True,capture_output=True); print(a.stdout)
 destination=user/'Applications/MemQL Cockpit.app'
 plist=user/'Library/LaunchAgents/com.znasllc.memql-cockpit-menubar.plist'
 data=plistlib.loads(plist.read_bytes())
 assert data['ProgramArguments']==[str(destination/'Contents/MacOS/MemQLCockpit')]
 assert data['KeepAlive']=={'SuccessfulExit':False}
 assert data['RunAtLoad'] and data['LimitLoadToSessionType']=='Aqua'
 assert plist.stat().st_mode&0o777==0o600
 assert json.loads(a.stdout)['changed'] is True
 b=subprocess.run(cmd,env=env,check=True,text=True,capture_output=True); print(b.stdout)
 assert json.loads(b.stdout)['changed'] is False
 assert 'memql-worker' not in calls.read_text()
 assert not (user/'.memql').exists()
 print('PASS: standalone package install, escaped path/plist, idempotent rerun; only stubbed menu service calls, worker untouched')
