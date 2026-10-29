import hashlib,io,json,plistlib,tarfile,tempfile,unittest
from pathlib import Path
import qualify

class QualificationTests(unittest.TestCase):
 def fixture(self, root):
  version='0.21.0';expected=qualify.expected_assets(version)
  for name in expected.values():
   path=root/name
   if name.endswith('.cdx.json'):path.write_text(json.dumps({'bomFormat':'CycloneDX','specVersion':'1.6','metadata':{'component':{'version':version}}}))
   elif name.endswith('.cdx.xml'):path.write_text('<bom xmlns="http://cyclonedx.org/schema/bom/1.6"><metadata><component><version>'+version+'</version></component></metadata></bom>')
   elif not name.endswith(('.tar.gz','.sha256','SHA256SUMS')):path.write_bytes(b'fixture:'+name.encode())
  for target in qualify.PLATFORMS:
   with tarfile.open(root/f'memql-{version}-{target}.tar.gz','w:gz') as tar:
    data=(root/f'memql-{target}').read_bytes();member=tarfile.TarInfo('./memql');member.size=len(data);tar.addfile(member,io.BytesIO(data))
   if target.startswith('darwin'):
    for kind,bundle in [('app','MemQL.app'),('menubar','MemQL Cockpit.app')]:
     with tarfile.open(root/f'memql-{kind}-{target}.tar.gz','w:gz') as tar:
      data=plistlib.dumps({'CFBundleShortVersionString':version,'CFBundleVersion':version,'CFBundleIdentifier':'com.znasllc.memql-worker' if kind=='app' else 'com.znasllc.memql-cockpit-menubar','CFBundleExecutable':'MemQL' if kind=='app' else 'MemQLCockpit'});member=tarfile.TarInfo(bundle+'/Contents/Info.plist');member.size=len(data);tar.addfile(member,io.BytesIO(data))
      if kind=='app':
       data=plistlib.dumps({'CFBundleIdentifier':'com.znasllc.memql-cockpit-menubar','CFBundleShortVersionString':version});member=tarfile.TarInfo(bundle+'/Contents/Library/LoginItems/MemQL Menu.app/Contents/Info.plist');member.size=len(data);tar.addfile(member,io.BytesIO(data))
  sums=''.join(qualify.sha(root/f'memql-{version}-{t}.tar.gz')+'  '+f'memql-{version}-{t}.tar.gz'+'\n' for t in qualify.PLATFORMS)
  (root/f'memql-{version}-SHA256SUMS').write_text(sums)
  for name in expected.values():
   if name.endswith('.sha256'):(root/name).write_text(qualify.sha(root/name[:-7])+'  '+name[:-7]+'\n')
  return expected,version
 def test_complete_set_and_tamper_refusal(self):
  with tempfile.TemporaryDirectory() as temp:
   root=Path(temp);expected,version=self.fixture(root)
   self.assertEqual(len(expected),41);qualify.verify_evidence(root,expected,version)
   (root/'memql-darwin-arm64').write_bytes(b'altered')
   with self.assertRaisesRegex(ValueError,'Archive differs'):qualify.verify_evidence(root,expected,version)
 def test_checksum_path_and_duplicate_refused(self):
  with tempfile.TemporaryDirectory() as temp:
   p=Path(temp)/'sums';p.write_text('a'*64+'  ../escape\n')
   with self.assertRaises(ValueError):qualify.parse_sums(p)
   p.write_text(('a'*64+'  asset\n')*2)
   with self.assertRaises(ValueError):qualify.parse_sums(p)
 def test_public_origin_confined(self):
  self.assertEqual(qualify.checked_url('https://github.com/znasllc-io/memql'), 'https://github.com/znasllc-io/memql')
  for url in ['http://github.com/file','https://user:password@github.com/file','https://unapproved.example/file']:
   with self.assertRaises(ValueError):qualify.checked_url(url)
if __name__=='__main__':unittest.main()
