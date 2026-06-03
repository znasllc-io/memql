// Command avatar-mint builds the operator avatar-persona catalog (memql#609)
// and emits the seed the SeedMaterializer reads
// (dsl/agents/avatarPersonas.memql). On a fresh DB the materializer turns those
// seeds into v1:agents:avatarPersona rows the Create-Assistant picker lists
// (copresent#239); the agent's avatarVendor/avatarPersonaId then point at the
// chosen entry and the voice-agent resolves it at session start.
//
// Modes:
//
//	(default)        Resolve a curated set of Anam STOCK avatars live (by
//	                 displayName) and write the catalog seed. No plan limit,
//	                 instant, production-quality stock faces.
//	--mint           Mint CUSTOM avatars from operator images via Anam's
//	                 Create Avatar endpoint (POST /v1/avatars). NOTE: limited by
//	                 the Anam plan's concurrent custom-avatar cap.
//	--list           List the account's Anam avatars (paginated), flagging
//	                 org-created (custom) rows.
//	--delete <id>    Delete a custom Anam avatar (frees a custom-avatar slot).
//
// ANAM_API_KEY is read from the environment, else decrypted from the sealed
// genesis envelope (needs MEMQL_MASTER_KEY) -- the same key the cluster runs
// with, so the catalog matches what the voice-agent resolves.
//
// Usage:
//
//	make avatar-mint                 # write the curated stock catalog seed
//	go run ./cmd/avatar-mint --list
//	go run ./cmd/avatar-mint --delete <avatarId>
//	go run ./cmd/avatar-mint --mint  # custom mint (plan permitting)
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/secret"
)

const anamAvatarsURL = "https://api.anam.ai/v1/avatars"

// curatedStock is the operator-curated selection of Anam STOCK avatars that
// populate the catalog. We resolve each entry's vendor id + thumbnail live by
// displayName (so a stock-id change self-heals); only the gender bucket +
// catalog slug are curated here. Balanced 2 female / 2 male for the picker.
var curatedStock = []stockPick{
	{Slug: "mia", DisplayName: "Mia", Gender: "female"},
	{Slug: "liv", DisplayName: "Liv", Gender: "female"},
	{Slug: "gabriel", DisplayName: "Gabriel", Gender: "male"},
	{Slug: "finn", DisplayName: "Finn", Gender: "male"},
}

type stockPick struct {
	Slug, DisplayName, Gender string
}

// operatorImages is the CUSTOM-mint catalog: operator images under copresent
// public/avatars minted into Anam as custom avatars. The Anam plan caps
// concurrent custom avatars at 1, so only the first that fits is minted; the
// rest wait for a plan upgrade (add them here when the cap lifts). These are the
// operator's OWN faces and are listed first in the catalog.
var operatorImages = []personaSpec{
	{Slug: "sofia", Name: "Sofia", Gender: "female", File: "female_0.png"},
}

type personaSpec struct {
	Slug, Name, Gender, File string
}

// avatar mirrors the Anam avatar record fields we consume.
type avatar struct {
	ID                      string `json:"id"`
	DisplayName             string `json:"displayName"`
	ImageURL                string `json:"imageUrl"`
	CreatedByOrganizationID string `json:"createdByOrganizationId"`
}

// catalogRow is a fully-resolved persona row written to the seed.
type catalogRow struct {
	Slug, Name, Gender, PersonaID, ImageRef string
}

func main() {
	imagesDir := flag.String("images-dir", "../copresent/public/avatars", "directory holding the operator images (custom mint)")
	genesisPath := flag.String("genesis", defaultGenesisPath(), "sealed genesis envelope path (for ANAM_API_KEY)")
	out := flag.String("out", filepath.Join("dsl", "agents", "avatarPersonas.memql"), "seed file to write")
	noCustom := flag.Bool("stock-only", false, "skip the custom operator-image mint; write only the curated stock catalog")
	list := flag.Bool("list", false, "list the account's Anam avatars and exit")
	del := flag.String("delete", "", "delete a custom Anam avatar by id and exit")
	dryRun := flag.Bool("dry-run", false, "report what would happen; no writes / vendor mutations")
	flag.Parse()

	var err error
	switch {
	case *list:
		err = listAvatars(*genesisPath)
	case *del != "":
		err = deleteAvatar(*genesisPath, *del, *dryRun)
	default:
		err = runCatalog(*imagesDir, *genesisPath, *out, !*noCustom, *dryRun)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "avatar-mint:", err)
		os.Exit(1)
	}
}

// runCatalog produces the full catalog: the operator's OWN custom avatars
// (minted from their images, up to the Anam plan's custom cap) FIRST, then the
// curated stock fillers. Rows are merged by slug, so a custom avatar already in
// the seed is preserved (and not re-minted) and the stock set is refreshed live.
func runCatalog(imagesDir, genesisPath, out string, withCustom, dryRun bool) error {
	apiKey, err := resolveAnamKey(genesisPath)
	if err != nil {
		return err
	}
	existing, err := parseExisting(out)
	if err != nil {
		return fmt.Errorf("read existing seed %q: %w", out, err)
	}
	have := map[string]catalogRow{}
	for _, p := range existing {
		have[p.Slug] = p
	}

	// ordered accumulates rows in catalog order (custom first, then stock),
	// upserting by slug so re-runs stay idempotent.
	var ordered []catalogRow
	seen := map[string]bool{}
	add := func(r catalogRow) {
		if seen[r.Slug] {
			return
		}
		seen[r.Slug] = true
		ordered = append(ordered, r)
	}

	// 1. Custom operator-image avatars (the operator's own faces).
	if withCustom {
		client := &http.Client{Timeout: 5 * time.Minute}
		for _, spec := range operatorImages {
			if prev, ok := have[spec.Slug]; ok {
				fmt.Printf("[skip] %s already minted -- preserving %s\n", spec.Slug, prev.PersonaID)
				add(prev)
				continue
			}
			imgPath := filepath.Join(imagesDir, spec.File)
			if _, statErr := os.Stat(imgPath); statErr != nil {
				return fmt.Errorf("image for %q not found: %w", spec.Slug, statErr)
			}
			if dryRun {
				fmt.Printf("[dry-run] would mint custom %s (%s) from %s\n", spec.Slug, spec.Gender, imgPath)
				continue
			}
			id, mintErr := mintAnamAvatar(client, apiKey, spec.Name, imgPath)
			if mintErr != nil {
				if isCustomCapError(mintErr) {
					fmt.Printf("[warn] custom-avatar cap reached -- skipping %s (upgrade the Anam plan to add more of your own faces)\n", spec.Slug)
					continue
				}
				return fmt.Errorf("mint %q: %w", spec.Slug, mintErr)
			}
			fmt.Printf("[ok] minted custom %s -> anam avatar %s\n", spec.Slug, id)
			add(catalogRow{Slug: spec.Slug, Name: spec.Name, Gender: spec.Gender, PersonaID: id, ImageRef: "avatars/" + spec.File})
		}
	}

	// 2. Curated stock fillers (resolved live by displayName).
	all, err := fetchAllAvatars(apiKey)
	if err != nil {
		return err
	}
	byName := map[string]avatar{}
	for _, a := range all {
		byName[strings.ToLower(a.DisplayName)] = a
	}
	for _, pick := range curatedStock {
		a, ok := byName[strings.ToLower(pick.DisplayName)]
		if !ok {
			return fmt.Errorf("curated stock avatar %q not found in the account (run --list)", pick.DisplayName)
		}
		fmt.Printf("[ok] %s -> anam stock %q (%s) %s\n", pick.Slug, a.DisplayName, pick.Gender, a.ID)
		add(catalogRow{Slug: pick.Slug, Name: pick.DisplayName, Gender: pick.Gender, PersonaID: a.ID, ImageRef: a.ImageURL})
	}

	if dryRun {
		fmt.Printf("[dry-run] would write %d persona(s) to %s\n", len(ordered), out)
		return nil
	}
	if err := writeSeedFile(out, ordered); err != nil {
		return fmt.Errorf("write seed %q: %w", out, err)
	}
	fmt.Printf("[done] wrote %d persona(s) to %s\n", len(ordered), out)
	return nil
}

// isCustomCapError reports whether the error is Anam's concurrent custom-avatar
// cap (403) -- a plan limit, not a failure. We stop minting custom avatars and
// fall back to stock for the rest of the catalog.
func isCustomCapError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "status 403") && strings.Contains(s, "one-shot")
}

// mintAnamAvatar POSTs the image to Anam's Create Avatar endpoint and returns
// the created avatar id. multipart/form-data: displayName + imageFile.
func mintAnamAvatar(client *http.Client, apiKey, displayName, imagePath string) (string, error) {
	imgData, err := os.ReadFile(imagePath)
	if err != nil {
		return "", fmt.Errorf("read image: %w", err)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("displayName", displayName); err != nil {
		return "", err
	}
	// Stamp the file part's Content-Type explicitly: Go's CreateFormFile
	// defaults to application/octet-stream, which Anam rejects as "invalid
	// image format". Detect the real type from the bytes (PNG/JPEG/WebP).
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="imageFile"; filename=%q`, filepath.Base(imagePath)))
	hdr.Set("Content-Type", http.DetectContentType(imgData))
	part, err := w.CreatePart(hdr)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(imgData); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, anamAvatarsURL, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("anam create avatar: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed avatar
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decode anam response: %w (body: %s)", err, string(respBody))
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("anam response missing avatar id (body: %s)", string(respBody))
	}
	return parsed.ID, nil
}

// fetchAllAvatars pages through GET /v1/avatars and returns every avatar.
func fetchAllAvatars(apiKey string) ([]avatar, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	var out []avatar
	for page := 1; page <= 50; page++ {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s?page=%d", anamAvatarsURL, page), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("anam list avatars: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}
		var parsed struct {
			Data []avatar `json:"data"`
			Meta struct {
				LastPage    int `json:"lastPage"`
				CurrentPage int `json:"currentPage"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return nil, fmt.Errorf("decode anam list: %w", err)
		}
		out = append(out, parsed.Data...)
		if parsed.Meta.CurrentPage >= parsed.Meta.LastPage || len(parsed.Data) == 0 {
			break
		}
	}
	return out, nil
}

// listAvatars prints every avatar, flagging org-created (custom) rows -- the
// ones that count against the plan's custom-avatar cap.
func listAvatars(genesisPath string) error {
	apiKey, err := resolveAnamKey(genesisPath)
	if err != nil {
		return err
	}
	all, err := fetchAllAvatars(apiKey)
	if err != nil {
		return err
	}
	fmt.Printf("%d avatar(s):\n", len(all))
	for _, a := range all {
		kind := "stock"
		if strings.TrimSpace(a.CreatedByOrganizationID) != "" {
			kind = "CUSTOM"
		}
		fmt.Printf("  [%-6s] %-40s %s\n", kind, a.ID, a.DisplayName)
	}
	return nil
}

// deleteAvatar DELETEs a custom Anam avatar by id.
func deleteAvatar(genesisPath, id string, dryRun bool) error {
	apiKey, err := resolveAnamKey(genesisPath)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Printf("[dry-run] would DELETE anam avatar %s\n", id)
		return nil
	}
	req, err := http.NewRequest(http.MethodDelete, anamAvatarsURL+"/"+id, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("anam delete avatar: status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	fmt.Printf("[ok] deleted anam avatar %s\n", id)
	return nil
}

// resolveAnamKey reads ANAM_API_KEY from the environment, else decrypts it from
// the sealed genesis envelope (needs MEMQL_MASTER_KEY in env).
func resolveAnamKey(genesisPath string) (string, error) {
	if v := strings.TrimSpace(os.Getenv("ANAM_API_KEY")); v != "" {
		return v, nil
	}
	if strings.TrimSpace(os.Getenv(secret.EnvMasterKey)) == "" {
		return "", fmt.Errorf("ANAM_API_KEY not in env and %s not set (cannot decrypt %s)", secret.EnvMasterKey, genesisPath)
	}
	entries, err := genesis.OpenFile(genesisPath)
	if err != nil {
		return "", fmt.Errorf("open genesis envelope %s: %w", genesisPath, err)
	}
	for _, e := range entries {
		if e.Name == "ANAM_API_KEY" {
			if v := strings.TrimSpace(e.Value); v != "" {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("ANAM_API_KEY not found in env or genesis envelope %s", genesisPath)
}

var seedBlockRe = regexp.MustCompile(`(?s)seed avatarPersona (\S+) \{(.*?)\}`)
var fieldRe = regexp.MustCompile(`(\w+):\s*"([^"]*)"`)

// parseExisting reads the already-written seed file so --mint re-runs are
// idempotent (existing rows preserved, never re-minted).
func parseExisting(path string) ([]catalogRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []catalogRow
	for _, m := range seedBlockRe.FindAllStringSubmatch(string(data), -1) {
		fields := map[string]string{}
		for _, f := range fieldRe.FindAllStringSubmatch(m[2], -1) {
			fields[f[1]] = f[2]
		}
		out = append(out, catalogRow{
			Slug:      m[1],
			Name:      fields["name"],
			Gender:    fields["gender"],
			PersonaID: fields["personaId"],
			ImageRef:  fields["imageRef"],
		})
	}
	return out, nil
}

func writeSeedFile(path string, rows []catalogRow) error {
	var b strings.Builder
	b.WriteString("// avatarPersonas.memql\n")
	b.WriteString("//\n")
	b.WriteString("// GENERATED by `make avatar-mint` (cmd/avatar-mint) -- memql#609.\n")
	b.WriteString("// Operator-curated avatar persona catalog (Anam). The SeedMaterializer\n")
	b.WriteString("// turns each row into a v1:agents:avatarPersona the Create-Assistant\n")
	b.WriteString("// picker lists (copresent#239); the agent's avatarVendor/avatarPersonaId\n")
	b.WriteString("// point at the chosen entry and the voice-agent resolves it at session\n")
	b.WriteString("// start. Re-run avatar-mint to regenerate. Do not hand-edit personaId --\n")
	b.WriteString("// it is the vendor-issued avatar id.\n")
	b.WriteString("\n")
	b.WriteString("use agents.concepts.{ avatarPersona }\n")
	for _, p := range rows {
		b.WriteString("\n")
		fmt.Fprintf(&b, "@description(\"Avatar persona %s (%s) -- Anam.\")\n", p.Name, p.Gender)
		fmt.Fprintf(&b, "seed avatarPersona %s {\n", p.Slug)
		fmt.Fprintf(&b, "  vendor:     \"anam\"\n")
		fmt.Fprintf(&b, "  personaId:  %q\n", p.PersonaID)
		fmt.Fprintf(&b, "  name:       %q\n", p.Name)
		fmt.Fprintf(&b, "  gender:     %q\n", p.Gender)
		fmt.Fprintf(&b, "  imageRef:   %q\n", p.ImageRef)
		fmt.Fprintf(&b, "  previewRef: %q\n", p.ImageRef)
		b.WriteString("}\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func defaultGenesisPath() string {
	if p := strings.TrimSpace(os.Getenv(genesis.EnvGenesisPath)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".memql", "genesis.znas")
	}
	return filepath.Join(home, ".memql", "genesis.znas")
}
