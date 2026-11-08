package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual downloader and placement fence. The alternative route
// must recover a failed browser download without trusting metadata as a new pin.
func TestInstallBinaryGitHubAssetFallback(t *testing.T) {
	const primary = "https://github.com/k3d-io/k3d/releases/download/v5.9.0/k3d-linux-amd64"
	const metadataURL = "https://api.github.com/repos/k3d-io/k3d/releases/tags/v5.9.0"
	const assetURL = "https://api.github.com/repos/k3d-io/k3d/releases/assets/1234"
	const payload = "verified inert fixture bytes\n"
	for _, tc := range []struct {
		name, failure, body        string
		primaryOK, want, wantAsset bool
		mutate                     func(map[string]any, map[string]any)
	}{
		{name: "primary succeeds without API", primaryOK: true, want: true},
		{name: "504 uses exact official asset", want: true, wantAsset: true},
		{name: "legacy asset without API digest uses committed pin", want: true, wantAsset: true, mutate: func(_, a map[string]any) { a["digest"] = nil }},
		{name: "wrong tag", mutate: func(r, _ map[string]any) { r["tag_name"] = "v5.8.0" }},
		{name: "draft", mutate: func(r, _ map[string]any) { r["draft"] = true }},
		{name: "prerelease", mutate: func(r, _ map[string]any) { r["prerelease"] = true }},
		{name: "missing asset", mutate: func(r, _ map[string]any) { r["assets"] = []any{} }},
		{name: "ambiguous asset", mutate: func(r, a map[string]any) { r["assets"] = []any{a, a} }},
		{name: "wrong platform", mutate: func(_, a map[string]any) { a["name"] = "k3d-darwin-arm64" }},
		{name: "invalid asset ID", mutate: func(_, a map[string]any) { a["id"] = "1234/../../other" }},
		{name: "boolean asset ID", mutate: func(_, a map[string]any) { a["id"] = true }},
		{name: "not uploaded", mutate: func(_, a map[string]any) { a["state"] = "new" }},
		{name: "oversized asset", mutate: func(_, a map[string]any) { a["size"] = 256 * 1024 * 1024 }},
		{name: "metadata digest differs from pin", mutate: func(_, a map[string]any) { a["digest"] = "sha256:" + strings.Repeat("0", 64) }},
		{name: "metadata unavailable", failure: "metadata"},
		{name: "partial asset download", failure: "asset", wantAsset: true},
		{name: "size mismatch", body: payload + "extra", wantAsset: true},
		{name: "pinned digest mismatch", body: "tampered inert fixture bytes\n", wantAsset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pathDir, home, dest := installPATH(t), t.TempDir(), t.TempDir()
			linkReal(t, pathDir, "python3")
			a := map[string]any{"id": 1234, "name": "k3d-linux-amd64", "state": "uploaded", "size": len(payload), "digest": "sha256:" + sha256Hex([]byte(payload)), "url": "https://untrusted.invalid/asset", "browser_download_url": "https://untrusted.invalid/asset"}
			r := map[string]any{"tag_name": "v5.9.0", "draft": false, "prerelease": false, "assets": []any{a}}
			if tc.mutate != nil {
				tc.mutate(r, a)
			}
			metadata, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			fixture := t.TempDir()
			if err := os.WriteFile(filepath.Join(fixture, "release.json"), metadata, 0600); err != nil {
				t.Fatal(err)
			}
			body := tc.body
			if body == "" {
				body = payload
			}
			if err := os.WriteFile(filepath.Join(fixture, "asset"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			// A refused replacement must preserve the user's existing binary.
			target := filepath.Join(dest, "k3d")
			if err := os.WriteFile(target, []byte("previous tool"), 0755); err != nil {
				t.Fatal(err)
			}
			writeStub(t, pathDir, "curl", `exec python3 - "$@" <<'PY'
import json, os, pathlib, shutil, sys
args = sys.argv[1:]
root = pathlib.Path(os.environ["FIXTURE"])
with (root / "calls").open("a") as log:
    log.write(json.dumps(args) + "\n")
url = args[-1]
out = pathlib.Path(args[args.index("-o") + 1])
if url == os.environ["PRIMARY"]:
    if os.environ["PRIMARY_OK"] != "yes":
        out.write_text("partial 504 response")
        sys.exit(22)
    shutil.copyfile(root / "asset", out)
elif url == os.environ["METADATA_URL"]:
    if os.environ["FAILURE"] == "metadata": sys.exit(22)
    shutil.copyfile(root / "release.json", out)
elif url == os.environ["ASSET_URL"]:
    if os.environ["FAILURE"] == "asset":
        out.write_text("partial asset")
        sys.exit(22)
    shutil.copyfile(root / "asset", out)
else:
    sys.exit("unexpected URL: " + url)
PY
`)
			pins := writePins(t, map[string][3]string{"k3d": {"v5.9.0", primary, sha256Hex([]byte(payload))}})
			cmd := exec.Command("bash", scriptPath(t, "install-binary.sh"), "--tool=k3d", "--pins="+pins, "--dest="+dest)
			primaryOK := "no"
			if tc.primaryOK {
				primaryOK = "yes"
			}
			cmd.Env = []string{"PATH=" + pathDir, "HOME=" + home, "MEMQL_FETCH_ATTEMPTS=1", "MEMQL_FETCH_RETRY_DELAY_SECONDS=0", "FIXTURE=" + fixture, "PRIMARY=" + primary, "METADATA_URL=" + metadataURL, "ASSET_URL=" + assetURL, "PRIMARY_OK=" + primaryOK, "FAILURE=" + tc.failure}
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("success=%v, want %v: %v\n%s", err == nil, tc.want, err, out)
			}
			var envelope capEnvelope
			if err := json.Unmarshal([]byte(lastJSONObject(string(out))), &envelope); err != nil {
				t.Fatalf("missing capability result: %s", out)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			want := "previous tool"
			if tc.want {
				want = payload
			}
			if string(got) != want {
				t.Fatalf("placed %q, want %q", got, want)
			}
			calls, err := os.ReadFile(filepath.Join(fixture, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(calls), assetURL) != tc.wantAsset {
				t.Fatalf("wrong asset download decision: %s", calls)
			}
			if tc.primaryOK && strings.Contains(string(calls), "api.github.com") {
				t.Fatal("successful primary used the fallback")
			}
			if tc.wantAsset && (!strings.Contains(string(calls), "--max-filesize") || !strings.Contains(string(calls), "--proto-redir")) {
				t.Fatal("API download lost its transfer bounds")
			}
		})
	}
}
