package pipelinesteps

import (
	"context"
	"os/exec"
	"testing"
)

func verifyImageReadGrant(t *testing.T, ctx context.Context, archive, token string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "python3", "-c", `
import gzip, io, json, sys, tarfile
secret = sys.argv[2].encode()
proof = False
with tarfile.open(sys.argv[1]) as image:
    for member in image:
        if not member.isfile():
            continue
        body = image.extractfile(member).read()
        if body.startswith(b'\x1f\x8b'):
            body = gzip.decompress(body)
        if secret in body:
            raise SystemExit('read grant persisted in image metadata or a layer')
        try:
            layer = tarfile.open(fileobj=io.BytesIO(body))
        except tarfile.TarError:
            continue
        with layer:
            for entry in layer:
                if entry.name.lstrip('./') == 'read-grant-proof.txt' and entry.isfile():
                    proof = layer.extractfile(entry).read() == b'read-grant-verified\n'
if not proof:
    raise SystemExit('build did not retain its successful mount/lifetime proof')
print(json.dumps({'readGrantMounted': True, 'ordinaryRunEnvironmentClear': True,
                  'secretAbsentFromExport': True, 'secretAbsentAfterInstruction': True}))
`, archive, token)
	proof, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("read grant image proof: %v %s", err, proof)
	}
	t.Logf("read grant image proof: %s", proof)
}
