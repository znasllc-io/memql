package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// ImageAssetWriter is a storage capability, not a workflow. The caller holds
// the cross-replica operation gate; writes must reconcile the immutable file.
type ImageAssetWriter interface {
	SaveImage(context.Context, ImageAssetWrite) error
}
type ImageAssetWrite struct {
	FileID, Name, MIME, RunID, StepKey string
	Data                               []byte
	Provenance                         map[string]any
}

func (i *Integration) SetImageAssets(writer ImageAssetWriter) { i.imageAssets = writer }

type revisionImageSpec struct {
	Mode        string `json:"mode"`
	Name        string `json:"name"`
	Alt         string `json:"alt"`
	URL         string `json:"url"`
	SourceURL   string `json:"sourceURL"`
	Attribution string `json:"attribution"`
	License     string `json:"license"`
	Prompt      string `json:"prompt"`
}

func (s revisionImageSpec) validate() error {
	if s.Name == "" || len(s.Name) > 120 || path.Base(s.Name) != s.Name || strings.ContainsAny(s.Name, "\\\r\n\x00") || strings.TrimSpace(s.Alt) == "" || len(s.Alt) > 1000 || !utf8.ValidString(s.Alt) {
		return fmt.Errorf("image requires a safe filename and descriptive alt text")
	}
	ext := strings.ToLower(path.Ext(s.Name))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".gif" && ext != ".webp" {
		return fmt.Errorf("use a PNG, JPEG, GIF or WebP image filename")
	}
	if len(s.Attribution) > 2000 || len(s.License) > 1000 || len(s.Prompt) > 12000 {
		return fmt.Errorf("image description exceeds its limit")
	}
	switch s.Mode {
	case "import":
		if err := checkImageURL(s.URL); err != nil {
			return err
		}
		if err := checkImageURL(s.SourceURL); err != nil {
			return err
		}
		if strings.TrimSpace(s.License) == "" || strings.TrimSpace(s.Attribution) == "" || s.Prompt != "" {
			return fmt.Errorf("a researched image requires source attribution and reuse licensing")
		}
	case "generate":
		if ext != ".png" || strings.TrimSpace(s.Prompt) == "" || s.URL != "" || s.SourceURL != "" || s.License != "" || s.Attribution != "" {
			return fmt.Errorf("a generated PNG requires a prompt, not archival source or licensing claims")
		}
	default:
		return fmt.Errorf("image mode must be import or generate")
	}
	return nil
}
func (i *Integration) handleRevisionImagePlan(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	raw := asString(args["response"])
	if len(raw) > 128<<10 {
		return nil, fmt.Errorf("image plan is too large")
	}
	var plan struct {
		Images      []revisionImageSpec `json:"images"`
		Limitations string              `json:"limitations"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("invalid image plan: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("image plan has trailing data")
	}
	maximum := intField(args, "maxImages")
	if maximum == 0 {
		maximum = 64
	}
	minimum := intField(args, "minImages")
	if maximum < 1 || maximum > 64 || minimum < 0 || minimum > maximum {
		return nil, fmt.Errorf("invalid image plan bounds")
	}
	if len(plan.Images) < minimum || len(plan.Images) > maximum || len(plan.Limitations) > 4000 {
		return nil, fmt.Errorf("image plan exceeds its bounds")
	}
	seen := map[string]bool{}
	for _, spec := range plan.Images {
		if mode := asString(args["mode"]); mode != "" && spec.Mode != mode {
			return nil, fmt.Errorf("image plan changed the requested acquisition mode")
		}
		if err := spec.validate(); err != nil {
			return nil, err
		}
		if seen[spec.Name] {
			return nil, fmt.Errorf("image filenames must be distinct")
		}
		seen[spec.Name] = true
	}
	return reviewResult(map[string]any{"images": plan.Images, "limitations": plan.Limitations})
}

type revisionImageGenerator interface {
	CallAIImage(context.Context, airoute.ResolveRequest, memql.FleetImageRequest, string, func(context.Context, memql.FleetImage, airoute.Resolution) (string, error)) (memql.StoredImageAIResult, error)
}

func (i *Integration) handlePrepareRevisionImage(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	requestID := asString(args["requestId"])
	ids, _, _, err := i.revisionRun(ctx, requestID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(args["specification"])
	if err != nil || len(raw) > 20000 {
		return nil, fmt.Errorf("image specification is too large")
	}
	var spec revisionImageSpec
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&spec); err != nil {
		return nil, err
	}
	if err = spec.validate(); err != nil {
		return nil, err
	}
	if i.imageAssets == nil {
		return nil, fmt.Errorf("image storage is unavailable")
	}
	ac, _ := auth.AccessFromContext(ctx)
	rc, _ := common.RunFromContext(ctx)
	width, height := intField(args, "width"), intField(args, "height")
	if spec.Mode == "generate" && (width < 256 || width > 1536 || height < 256 || height > 1536) {
		return nil, fmt.Errorf("image dimensions must be between 256 and 1536 pixels")
	}
	identity, _ := json.Marshal([]any{memql.BareShortId(ac.UserId), requestID, spec, width, height, rc.Override})
	fileID := "review-image-" + string(id.NewUntracked().FromBytes(identity))
	release, err := i.fileVersionGate(ctx, fileID)
	if err != nil {
		return nil, err
	}
	defer release()
	stored, err := i.revisionRow(ctx, "libraryFileById", map[string]any{"fileId": fileID})
	if err != nil {
		return nil, err
	}
	if stored != nil {
		if boolField(stored, "archived") || revisionMap(stored["imageProvenance"])["requestId"] != requestID {
			return nil, fmt.Errorf("the stored image is unavailable")
		}
		return reviewResult(map[string]any{"fileId": fileID})
	}
	rows, err := i.revisionRows(ctx, "libraryImagesForRevision", map[string]any{"requestId": requestID, "runId": ids.RunID})
	if err != nil {
		return nil, err
	}
	if len(rows) >= 64 {
		return nil, fmt.Errorf("a document revision can prepare up to 64 images")
	}
	provenance := map[string]any{"requestId": requestID, "mode": spec.Mode, "alt": spec.Alt, "sourceURL": spec.SourceURL, "url": spec.URL, "attribution": spec.Attribution, "license": spec.License, "prompt": spec.Prompt}
	if rc.Override != nil {
		provenance["stepOverride"] = rc.Override
	}
	save := func(ctx context.Context, content []byte) error {
		mime, err := reviewAttachmentMIME(spec.Name, content)
		if err != nil {
			return err
		}
		if _, _, _, err = i.revisionRun(ctx, requestID); err != nil {
			return err
		}
		return i.imageAssets.SaveImage(ctx, ImageAssetWrite{FileID: fileID, Name: spec.Name, MIME: mime, RunID: ids.RunID, StepKey: rc.StepKey, Data: content, Provenance: provenance})
	}
	if spec.Mode == "import" {
		fetch := i.imageFetch
		if fetch == nil {
			fetch = fetchRevisionImage
		}
		content, err := fetch(ctx, spec.URL)
		if err == nil {
			if _, invalid := reviewAttachmentMIME(spec.Name, content); invalid != nil {
				err = unavailableImageSource("unsupported or invalid raster image: %v", invalid)
			}
		}
		if err != nil {
			var unavailable *imageSourceUnavailable
			if ctx.Err() == nil && boolField(args, "reportSourceFailure") && errors.As(err, &unavailable) {
				return reviewResult(map[string]any{"fileId": "", "problem": unavailable.Error()})
			}
			return nil, err
		}
		if err = save(ctx, content); err != nil {
			return nil, err
		}
	} else {
		generator, ok := i.engine.(revisionImageGenerator)
		if !ok {
			return nil, fmt.Errorf("image generation is unavailable on this node")
		}
		level := airoute.Level(asString(args["level"]))
		_, err = generator.CallAIImage(ctx, airoute.ResolveRequest{Level: level, PromptName: "libraryRevisionImageGeneration"}, memql.FleetImageRequest{Prompt: spec.Prompt, Width: width, Height: height, Count: 1, Format: "png"}, fileID, func(ctx context.Context, image memql.FleetImage, resolution airoute.Resolution) (string, error) {
			provenance["provider"], provenance["model"] = resolution.ProviderName, resolution.Model
			provenance["attribution"] = "AI-generated illustration; model " + resolution.Model
			if err := save(ctx, image.Data); err != nil {
				return "", err
			}
			return fileID, nil
		})
		if err != nil {
			return nil, err
		}
	}
	return reviewResult(map[string]any{"fileId": fileID})
}

type preparedRevisionImage struct {
	Reference  reviewAttachment `json:"reference"`
	Provenance map[string]any   `json:"provenance"`
}

func (i *Integration) preparedRevisionImages(ctx context.Context, requestID, runID string) ([]preparedRevisionImage, error) {
	sourceRequest, sourceRun, err := i.revisionImageSource(ctx, requestID, runID)
	if err != nil {
		return nil, err
	}
	rows, err := i.revisionRows(memql.ContextWithFreshRead(ctx), "libraryImagesForRevision", map[string]any{"requestId": sourceRequest, "runId": sourceRun})
	if err != nil {
		return nil, err
	}
	if len(rows) > 64 {
		return nil, fmt.Errorf("too many prepared images")
	}
	result := []preparedRevisionImage{}
	for _, row := range rows {
		if boolField(row, "archived") {
			return nil, fmt.Errorf("a prepared image was deleted")
		}
		artifactID, _ := i.awaitArtifactId(ctx, asString(row["id"]))
		if artifactID == "" {
			return nil, fmt.Errorf("a saved image has not been indexed yet; retry this step")
		}
		doc, err := i.reviewDocument(ctx, artifactID)
		if err != nil {
			return nil, err
		}
		refs, _, err := i.reviewAttachments(ctx, []reviewAttachment{{ArtifactID: artifactID, Version: doc.version, Revision: doc.revision}})
		if err != nil {
			return nil, err
		}
		result = append(result, preparedRevisionImage{Reference: refs[0], Provenance: revisionMap(row["imageProvenance"])})
	}
	return result, nil
}
func (i *Integration) handleRevisionPreparedImages(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	requestID := asString(args["requestId"])
	ids, _, _, err := i.revisionRun(ctx, requestID)
	if err != nil {
		return nil, err
	}
	sourceRun := asString(args["sourceRunId"])
	if sourceRun == "" {
		sourceRun = ids.RunID
	}
	images, err := i.preparedRevisionImages(ctx, requestID, sourceRun)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(images)
	if err != nil {
		return nil, err
	}
	if len(raw) > 128<<10 {
		return nil, fmt.Errorf("prepared image metadata exceeds the limit")
	}
	return reviewResult(map[string]any{"references": string(raw), "sourceRunId": sourceRun})
}
func (i *Integration) validatePreparedRevisionImages(ctx context.Context, proposal map[string]any) error {
	if proposal["preparedImages"] == nil {
		return nil
	}
	images, err := decodePreparedRevisionImages(proposal["preparedImages"])
	if err != nil {
		return err
	}
	// Read inheritance from the owned durable request, never from a manifest
	// supplied by the model or a caller asking to approve a proposal.
	_, captured, _, _, err := i.revisionRequest(ctx, asString(proposal["requestId"]))
	if err != nil {
		return err
	}
	inherited, err := decodePreparedRevisionImages(captured["inheritedPreparedImages"])
	if err != nil {
		return err
	}
	for _, image := range images {
		refs, _, err := i.reviewAttachments(ctx, []reviewAttachment{image.Reference})
		if err != nil {
			return err
		}
		if refs[0] != image.Reference {
			return fmt.Errorf("prepared image reference changed")
		}
		doc, err := i.reviewDocument(ctx, refs[0].ArtifactID)
		if err != nil {
			return err
		}
		provenance := revisionMap(doc.backing["imageProvenance"])
		if workstate.ArtifactHash(provenance) != workstate.ArtifactHash(image.Provenance) {
			return fmt.Errorf("prepared image provenance changed")
		}
		if len(revisionMap(captured["amendment"])) > 0 && containsPreparedImage(inherited, image) {
			source, err := revisionIdentity(ctx, asString(provenance["requestId"]))
			if err != nil || memql.BareShortId(asString(doc.backing["producedByRunId"])) != source.RunID {
				return fmt.Errorf("inherited image no longer belongs to its recorded run")
			}
			continue
		}
		sourceRequest, sourceRun, err := i.revisionImageSource(ctx, asString(proposal["requestId"]), asString(doc.backing["producedByRunId"]))
		if err != nil {
			return err
		}
		if provenance["requestId"] != sourceRequest || memql.BareShortId(asString(doc.backing["producedByRunId"])) != sourceRun {
			return fmt.Errorf("prepared image provenance changed or belongs to another request")
		}
	}
	return nil
}

func decodePreparedRevisionImages(value any) ([]preparedRevisionImage, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 128<<10 {
		return nil, fmt.Errorf("prepared image manifest is too large")
	}
	var images []preparedRevisionImage
	if json.Unmarshal(raw, &images) != nil || len(images) > 64 {
		return nil, fmt.Errorf("invalid prepared image manifest")
	}
	return images, nil
}

func containsPreparedImage(images []preparedRevisionImage, candidate preparedRevisionImage) bool {
	key := workstate.ArtifactHash(map[string]any{"image": candidate})
	for _, image := range images {
		if key == workstate.ArtifactHash(map[string]any{"image": image}) {
			return true
		}
	}
	return false
}

// An amendment may remove or replace an earlier image. Pin the inherited
// assets it actually retains; freshly prepared assets remain required by DSL.
func amendedPreparedImages(captured map[string]any, content string, fresh []preparedRevisionImage) ([]preparedRevisionImage, error) {
	inherited, err := decodePreparedRevisionImages(captured["inheritedPreparedImages"])
	if err != nil {
		return nil, err
	}
	result := append([]preparedRevisionImage(nil), fresh...)
	for _, image := range inherited {
		if len(missingPreparedImages(content, []preparedRevisionImage{image})) == 0 && !containsPreparedImage(result, image) {
			result = append(result, image)
		}
	}
	return decodePreparedRevisionImages(result)
}

// A retry may reuse assets only from an owned, captured predecessor with the
// identical document, human feedback and attachments. Run IDs are not authority.
func sameRevisionInput(a, b map[string]any) bool {
	for _, key := range []string{"artifactId", "sourceId", "revision", "content", "comments", "instruction", "amendment"} {
		if workstate.ArtifactHash(map[string]any{key: a[key]}) != workstate.ArtifactHash(map[string]any{key: b[key]}) {
			return false
		}
	}
	return true
}
func (i *Integration) revisionImageSource(ctx context.Context, requestID, sourceRun string) (string, string, error) {
	ids, captured, _, _, err := i.revisionRequest(ctx, requestID)
	if err != nil {
		return "", "", err
	}
	sourceRun = memql.BareShortId(sourceRun)
	if sourceRun == "" || sourceRun == ids.RunID {
		return requestID, ids.RunID, nil
	}
	raw, err := json.Marshal(captured["previousRunIds"])
	var previous []string
	if err != nil || json.Unmarshal(raw, &previous) != nil {
		return "", "", fmt.Errorf("invalid revision predecessors")
	}
	allowed := false
	for _, run := range previous {
		if run == sourceRun {
			allowed = true
		}
	}
	if !allowed {
		return "", "", fmt.Errorf("image source is not a captured predecessor")
	}
	run, err := i.revisionRow(ctx, "workRunForOwner", map[string]any{"runId": sourceRun})
	if err != nil {
		return "", "", err
	}
	sourceRequest := asString(revisionMap(run["input"])["requestId"])
	sourceIDs, original, _, _, err := i.revisionRequest(ctx, sourceRequest)
	if err != nil {
		return "", "", err
	}
	if sourceIDs.RunID != sourceRun || !sameRevisionInput(captured, original) {
		return "", "", fmt.Errorf("prepared images belong to different feedback")
	}
	return sourceRequest, sourceRun, nil
}

// Verify actual Markdown image nodes, not filenames in prose, code or links.
func missingPreparedImages(content string, images []preparedRevisionImage) []string {
	embedded := map[string]bool{}
	root := goldmark.DefaultParser().Parse(text.NewReader([]byte(content)))
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if image, ok := n.(*ast.Image); entering && ok {
			embedded[string(image.Destination)] = true
		}
		return ast.WalkContinue, nil
	})
	missing := []string{}
	for _, image := range images {
		if !embedded[image.Reference.URI] {
			missing = append(missing, image.Reference.URI)
		}
	}
	return missing
}
