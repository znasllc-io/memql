import copy
import json
from pathlib import Path
import tempfile
import unittest
from archive_case import build, verify

class ArchiveTests(unittest.TestCase):
    def test_reviewed_selection_is_verified_and_immutable(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root/'README.md').write_text('Fixture evidence, not a live evaluation.\n')
            (root/'case.json').write_text('{"status":"not-run"}')
            s={'caseId':'fixture','snapshotId':'one','privacyReview':{'reviewer':'test','scope':'two synthetic files'},'files':[{'source':p,'path':p,'mediaType':'text/plain','provenance':'synthetic test'} for p in ['README.md','case.json']]}
            out=root/'case.zip'; receipt=build(root,s,out); verify(out)
            self.assertEqual(receipt['members'],3)
            with self.assertRaises(ValueError): build(root,s,out)
            for field,value in [('path','../escape'),('source','.git/config')]:
                bad=copy.deepcopy(s);bad['files'][0][field]=value
                with self.assertRaises(ValueError): build(root,bad,root/'bad.zip')
            (root/'case.json').write_text(json.dumps({'nested':{'encryptedAddress':'sensitive'}}))
            with self.assertRaises(ValueError): build(root,s,root/'private.zip')
            (root/'case.json').unlink();(root/'case.json').symlink_to(root/'README.md')
            with self.assertRaises(ValueError): build(root,s,root/'link.zip')

if __name__=='__main__': unittest.main()
