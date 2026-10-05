package compose

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSourcePackageRebuildsOutsideTheWorkspace(t *testing.T) {
	p := Provenance{Title: "Bird report", CreatedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}
	for _, format := range []Format{FormatPDF, FormatDOCX, FormatHTML, FormatCSV, FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			recipe := RenderRecipe{Name: p.Title, Format: format, Draft: Draft{Title: p.Title, Body: "A sample report", Header: []string{"bird"}, Rows: []map[string]any{{"bird": "quail"}}}, Provenance: p}
			out, err := RenderRecipeBytes(recipe)
			if err != nil {
				t.Fatal(err)
			}
			pack, err := BuildSourcePackage(recipe, "report."+string(format), out, map[string][]byte{"inputs/template.txt": []byte("A saved template")})
			if err != nil {
				t.Fatal(err)
			}
			zr, err := zip.NewReader(bytes.NewReader(pack.Bytes), int64(len(pack.Bytes)))
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			for _, f := range zr.File {
				r, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(dir, filepath.FromSlash(f.Name))
				if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(target, data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			// The PDF exercises the external module, copied renderer, numeric helper,
			// pinned dependencies, saved clock and actual standalone CLI in one test.
			if format == FormatPDF {
				cmd := exec.Command("go", "run", "-mod=mod", ".")
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GOWORK=off")
				if result, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("standalone rebuild: %v\n%s", err, result)
				}
				rebuilt, err := os.ReadFile(filepath.Join(dir, "rebuilt", "report.pdf"))
				if err != nil || !bytes.Equal(out.Bytes, rebuilt) {
					t.Fatalf("rebuild mismatch: %v", err)
				}
			}
			var manifest struct{ Files map[string]string }
			raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err = json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			for name, hash := range manifest.Files {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || (Result{Bytes: data}).SHA256() != hash {
					t.Fatalf("invalid source hash: %s %v", name, err)
				}
			}
		})
	}
}
func TestSourcePackageRejectsMismatchAndUnsafeMembers(t *testing.T) {
	recipe := RenderRecipe{Format: FormatText, Draft: Draft{Body: "Expected"}}
	out, _ := RenderRecipeBytes(recipe)
	if _, err := BuildSourcePackage(recipe, "file.txt", Result{Bytes: []byte("different")}, nil); err == nil {
		t.Fatal("accepted different delivered bytes")
	}
	for _, name := range []string{"../secret", "inputs/../../outside", "inputs\\bad"} {
		if _, err := BuildSourcePackage(recipe, "file.txt", out, map[string][]byte{name: nil}); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}
