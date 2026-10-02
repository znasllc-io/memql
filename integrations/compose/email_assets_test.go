package compose

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	pure "github.com/znasllc-io/memql/component/compose"
)

func TestEmailUsesOnlyExplicitlyIncludedCapturedImageBytes(t *testing.T) {
	data := referencePNG(t)
	url := "memql-asset:" + (pure.Result{Bytes: data}).SHA256()
	draft, _ := json.Marshal(pure.EmailTemplate{Subject: "Welcome", TextBody: "Hello", HTMLBody: `<p>Hello</p><img alt="Logo" src="` + url + `">`})
	files, err := readReference("brand.zip", "application/zip", referenceZIP(t, map[string][]byte{"logo.png": data}, ""), &referenceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	inputs := []Resolved{{Ref: SourceRef{Kind: KindLibraryFile, Content: true}, Files: files}}
	if _, err := EmbedEmailAssets(string(draft), inputs); err == nil {
		t.Fatal("reference-only image was included")
	}
	for _, unapproved := range []string{"https://private.example/asset", "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)} {
		bad, _ := json.Marshal(pure.EmailTemplate{Subject: "Welcome", TextBody: "Hi", HTMLBody: `<img src="` + unapproved + `">`})
		if _, err := EmbedEmailAssets(string(bad), inputs); err == nil {
			t.Fatal("unselected image accepted", unapproved)
		}
	}
	inputs[0].Ref.IncludeImages = true
	// Simulate the durable execution input crossing a replica boundary.
	stored, _ := json.Marshal(inputs)
	var second []Resolved
	if err := json.Unmarshal(stored, &second); err != nil {
		t.Fatal(err)
	}
	output, err := EmbedEmailAssets(string(draft), second)
	if err != nil {
		t.Fatal(err)
	}
	var template pure.EmailTemplate
	if err := json.Unmarshal([]byte(output), &template); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(template.HTMLBody, base64.StdEncoding.EncodeToString(data)) || strings.Contains(template.HTMLBody, "memql-asset:") {
		t.Fatal("output does not carry selected image")
	}
	if _, err := pure.RenderEmailTemplate(output); err != nil {
		t.Fatal(err)
	}
	second[0].Files[0].Image = []byte("changed after capture")
	if _, err := EmbedEmailAssets(string(draft), second); err == nil {
		t.Fatal("stale asset handle used changed bytes")
	}
}

func TestRecipeKeepsAssetPermissionSeparateFromReferencePermission(t *testing.T) {
	selectors := []any{map[string]any{"kind": "library_file", "selector": "file", "content": true, "includeImages": true}}
	refs, err := selectorsToSources(selectors)
	if err != nil || len(refs) != 1 || !refs[0].IncludeImages || !refs[0].Content {
		t.Fatal(refs, err)
	}
	if _, err := parseSourceRefs([]any{map[string]any{"kind": "library_file", "ref": "file", "includeImages": true}}); err == nil {
		t.Fatal("asset without content permission accepted")
	}
}

func TestGeneratedEmailCannotBypassImageSelections(t *testing.T) {
	for _, body := range []string{
		`<img srcset="https://private.example/image 2x">`,
		`<a href="https://client.example" ping="https://unselected.example/track">Click</a>`,
		`<p style="background:image('https://private.example/image')">Hi</p>`,
		`<table background="https://private.example/image"><tr><td>Hi</td></tr></table>`,
		`<p style="background:url(https://private.example/image)">Hi</p>`,
		`<p style="background:u\72l(https://private.example/image)">Hi</p>`,
		`<p style="background:u/**/rl(https://private.example/image)">Hi</p>`,
		`<style>@import "https://private.example/style"</style>`,
		`<svg><image href="https://private.example/image" /></svg>`,
		`<!--[if mso]><v:fill src="https://private.example/image" /><![endif]-->`,
		`<a href="javascript:alert(1)">Click</a>`,
		`<p onclick="alert(1)">Hi</p>`,
	} {
		draft, _ := json.Marshal(pure.EmailTemplate{Subject: "Hello", TextBody: "Hello", HTMLBody: body})
		if _, err := EmbedEmailAssets(string(draft), nil); err == nil {
			t.Fatalf("accepted unselected resource or active markup: %s", body)
		}
	}
	body := `<table style="width:100%; background:#fff"><tr><td><p style="color:#123; padding:16px">Hello</p><a href="https://client.example/news">Read more</a></td></tr></table>`
	draft, _ := json.Marshal(pure.EmailTemplate{Subject: "Hello", TextBody: "Hello", HTMLBody: body})
	if _, err := EmbedEmailAssets(string(draft), nil); err != nil {
		t.Fatal(err)
	}
}
