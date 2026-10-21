package workbench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestExportBinaryRanges(t *testing.T) {
	i := NewIntegration(nil)
	ws := &workspace{rootPath: t.TempDir()}
	data := bytes.Repeat([]byte{0, 255, 128, 13, 10, 80, 75}, 120000)
	if err := os.WriteFile(filepath.Join(ws.rootPath, "bundle.tar.gz"), data, 0600); err != nil {
		t.Fatal(err)
	}
	read := func(offset int64, version string) (exportChunk, error) {
		args := map[string]any{"path": "bundle.tar.gz", "encoding": "base64", "offset": offset, "maxBytes": 256 << 10}
		if version != "" {
			args["version"] = version
		}
		result := i.handleFSRead(context.Background(), ws, args)
		if !result.OK {
			t.Fatalf("read: %+v", result)
		}
		p := result.Payload.(map[string]any)
		return exportChunk{Path: p["path"].(string), Encoding: p["encoding"].(string), Content: p["content"].(string), Version: p["version"].(string), Offset: p["offset"].(int64), Size: p["size"].(int64), EOF: p["eof"].(bool)}, nil
	}
	var out bytes.Buffer
	size, digest, err := copyExport(context.Background(), &out, read)
	want := sha256.Sum256(data)
	if err != nil || size != int64(len(data)) || digest != hex.EncodeToString(want[:]) || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("export mismatch: %d %s %v", size, digest, err)
	}
	// A second replica/read using a stale snapshot may not join changed bytes.
	first, _ := read(0, "")
	if err = os.WriteFile(filepath.Join(ws.rootPath, "bundle.tar.gz"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	r := i.handleFSRead(context.Background(), ws, map[string]any{"path": "bundle.tar.gz", "encoding": "base64", "version": first.Version})
	if r.OK {
		t.Fatal("accepted changed source")
	}
}

func TestExportRefusesIncompleteAndChangedReceipts(t *testing.T) {
	for _, tc := range []string{"old-runtime", "gap", "changed", "too-large", "premature-eof", "bad-base64", "empty-nonterminal"} {
		t.Run(tc, func(t *testing.T) {
			read := func(offset int64, version string) (exportChunk, error) {
				c := exportChunk{Path: "test.zip", Encoding: "base64", Version: "v1", Offset: offset, Size: 2, Content: base64.StdEncoding.EncodeToString([]byte{0}), EOF: offset == 1}
				switch tc {
				case "old-runtime":
					c.Encoding = ""
				case "gap":
					c.Offset++
				case "changed":
					if offset > 0 {
						c.Version = "v2"
					}
				case "too-large":
					c.Size = 3 << 30
				case "premature-eof":
					c.EOF = true
				case "bad-base64":
					c.Content = "!"
				case "empty-nonterminal":
					c.Content = ""
				}
				return c, nil
			}
			if _, _, err := copyExport(context.Background(), &bytes.Buffer{}, read); err == nil {
				t.Fatal("accepted invalid export")
			}
		})
	}
}
