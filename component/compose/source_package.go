package compose

import (
	"archive/zip"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"runtime"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/num"
)

// The renderer travels with the artifact, so a later MemQL release cannot
// change how an old document rebuilds. Dependencies are pinned by go.mod/sum.
//
//go:embed recipe.go write.go provenance.go pdf.go docx.go email.go email_images.go deployable.go go.mod go.sum source.LICENSE
var rendererSources embed.FS

// BuildSourcePackage verifies that the editable recipe reproduces the delivered
// bytes before filing either as complete. Extras are explicit captured assets,
// never secrets, environment dumps, or paths read from the host filesystem.
func BuildSourcePackage(recipe RenderRecipe, outputName string, output Result, extras map[string][]byte) (Result, error) {
	if path.Base(outputName) != outputName || outputName == "." || strings.Contains(outputName, "\\") {
		return Result{}, fmt.Errorf("unsafe output name")
	}
	rebuilt, err := RenderRecipeBytes(recipe)
	if err != nil {
		return Result{}, err
	}
	if !bytes.Equal(rebuilt.Bytes, output.Bytes) {
		return Result{}, fmt.Errorf("source recipe does not reproduce the delivered file")
	}
	raw, err := json.MarshalIndent(recipe, "", "  ")
	if err != nil {
		return Result{}, err
	}
	files := map[string][]byte{"recipe.json": raw, "output/" + outputName: output.Bytes, "main.go": []byte(rebuildMain), "README.md": []byte(rebuildReadme), "core/num/num.go": []byte(num.Source), "core/go.mod": []byte("module github.com/znasllc-io/memql/core\n\ngo 1.26.1\n"), "go.mod": []byte(rebuildModule)}
	names, err := rendererSources.ReadDir(".")
	if err != nil {
		return Result{}, err
	}
	for _, name := range names {
		data, e := rendererSources.ReadFile(name.Name())
		if e != nil {
			return Result{}, e
		}
		if name.Name() == "go.sum" {
			files["go.sum"] = data
		}
		if name.Name() == "source.LICENSE" {
			files["LICENSE"] = data
		} else {
			files["renderer/"+name.Name()] = data
			if name.Name() == "go.mod" {
				mod := strings.Replace(string(data), "module github.com/znasllc-io/memql/component/compose", "module memql-rebuild", 1)
				mod = strings.Replace(mod, "=> ../../core", "=> ./core", 1)
				files["go.mod"] = []byte(mod + "\nrequire github.com/znasllc-io/memql/component/compose v0.0.0\nreplace github.com/znasllc-io/memql/component/compose => ./renderer\n")
			}
			if name.Name() == "go.sum" {
				files["go.sum"] = data
			}
		}
	}
	for name, data := range extras {
		if !strings.HasPrefix(name, "inputs/") || path.Clean(name) != name || strings.Contains(name, "\\") || strings.ContainsRune(name, 0) {
			return Result{}, fmt.Errorf("unsafe source path %q", name)
		}
		files[name] = data
	}
	hashes := map[string]string{}
	for name, data := range files {
		hashes[name] = (Result{Bytes: data}).SHA256()
	}
	manifest, _ := json.MarshalIndent(map[string]any{"version": 1, "renderer": "MemQL Materializer", "toolchain": runtime.Version(), "output": "output/" + outputName, "sha256": output.SHA256(), "files": hashes}, "", "  ")
	files["manifest.json"] = manifest
	keys := make([]string, 0, len(files))
	for key := range files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range keys {
		w, e := zw.Create(name)
		if e != nil {
			return Result{}, e
		}
		if _, e = w.Write(files[name]); e != nil {
			return Result{}, e
		}
	}
	if err = zw.Close(); err != nil {
		return Result{}, err
	}
	return Result{Bytes: buf.Bytes(), Embedded: true, Note: "Editable sources, captured inputs, pinned renderer and verified rebuild instructions."}, nil
}

const rebuildModule = `module memql-rebuild

go 1.26.1
toolchain go1.27.1

require github.com/znasllc-io/memql/component/compose v0.0.0
replace github.com/znasllc-io/memql/component/compose => ./renderer
replace github.com/znasllc-io/memql/core => ./core
`
const rebuildReadme = `# Rebuild this artifact

The delivered file is in output/. Edit recipe.json to reuse its content and layout.
The exact renderer source and pinned dependencies are included. No MemQL account,
model call, credentials, or original source server is needed for rendering.
Captured source inputs and the template used to compose the draft are in inputs/.
A model generated the saved draft; rerunning a model is not part of rebuilding it.

Install Go (the pinned toolchain is declared in go.mod), then run from this folder:

    GOWORK=off go run -mod=mod .

Go downloads the pinned public dependencies/toolchain on first use. The result
is written to rebuilt/. Its SHA-256 is checked against the delivered original.
After editing the recipe, use --verify=false to build an intentional variation.
manifest.json lists all original source and output hashes. Keep this folder
private if your sources contain private material. Rendering does not execute
commands, scripts or instructions contained in those sources.
`
const rebuildMain = `package main
import("encoding/json";"flag";"fmt";"os";"path/filepath"; c "github.com/znasllc-io/memql/component/compose")
func main(){if err:=run();err!=nil{fmt.Fprintln(os.Stderr,err);os.Exit(1)}}
func run()error{
 verify:=flag.Bool("verify",true,"check against the delivered original");flag.Parse()
 data,err:=os.ReadFile("recipe.json");if err!=nil{return err};var r c.RenderRecipe;if err=json.Unmarshal(data,&r);err!=nil{return err}
 data,err=os.ReadFile("manifest.json");if err!=nil{return err};var m struct{Output string;SHA256 string};if err=json.Unmarshal(data,&m);err!=nil{return err}
 result,err:=c.RenderRecipeBytes(r);if err!=nil{return err};if *verify&&result.SHA256()!=m.SHA256{return fmt.Errorf("output differs from original: %s",result.SHA256())}
 if err=os.MkdirAll("rebuilt",0755);err!=nil{return err};name:=filepath.Join("rebuilt",filepath.Base(m.Output));if err=os.WriteFile(name,result.Bytes,0644);err!=nil{return err};fmt.Println(name,result.SHA256());return nil
}
`
