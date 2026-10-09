package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
	_ "golang.org/x/image/webp"
)

const reviewAttachmentCount = 8
const reviewAttachmentBytes = 16 << 20
const reviewMarkdownBytes = 32 << 10

type reviewAttachment struct {
	ArtifactID string `json:"artifactId"`
	Version    int    `json:"version"`
	Revision   string `json:"revision"`
	SHA256     string `json:"sha256,omitempty"`
	Name       string `json:"name,omitempty"`
	MIME       string `json:"mimeType,omitempty"`
	URI        string `json:"uri,omitempty"`
	Size       int    `json:"size,omitempty"`
}

// Refs confer no authority. Resolve both index and backing rows as the caller,
// pin the bytes, and repeat that check when preparing and approving a proposal.
// No blob credentials or image bytes are persisted in comments/model prompts.
func (i *Integration) reviewAttachments(ctx context.Context, input any) ([]reviewAttachment, [][]byte, error) {
	refs := []reviewAttachment{}
	if input == nil {
		return refs, nil, nil
	}
	raw, err := json.Marshal(input)
	if err != nil || len(raw) > 16000 {
		return nil, nil, fmt.Errorf("attachment references are too large")
	}
	if err = json.Unmarshal(raw, &refs); err != nil || len(refs) > reviewAttachmentCount {
		return nil, nil, fmt.Errorf("attach up to eight images or Markdown files")
	}
	contents := make([][]byte, 0, len(refs))
	seen, total, markdownTotal := map[string]bool{}, 0, 0
	for n := range refs {
		ref := &refs[n]
		if ref.ArtifactID == "" || seen[ref.ArtifactID] {
			return nil, nil, fmt.Errorf("attach each file only once")
		}
		seen[ref.ArtifactID] = true
		doc, err := i.reviewDocument(memql.ContextWithFreshRead(ctx), ref.ArtifactID)
		if err != nil {
			return nil, nil, fmt.Errorf("an attachment is unavailable: %w", err)
		}
		if doc.version != ref.Version || doc.revision != ref.Revision {
			return nil, nil, fmt.Errorf("an attachment changed; remove it and attach its current version")
		}
		name := stringField(doc.backing, "name")
		if name == "" {
			name = stringField(doc.backing, "title")
		}
		limit := 8 << 20
		if strings.EqualFold(path.Ext(name), ".md") {
			limit = reviewMarkdownBytes
		}
		var content []byte
		if doc.kind == "file" {
			if i.blobFetcher == nil {
				return nil, nil, fmt.Errorf("attachment storage is unavailable")
			}
			stream, err := i.blobFetcher.DownloadStreamURL(ctx, stringField(doc.backing, "blobUrl"))
			if err != nil {
				return nil, nil, err
			}
			content, err = io.ReadAll(io.LimitReader(stream, int64(limit+1)))
			stream.Close()
			if err != nil {
				return nil, nil, err
			}
		} else {
			content = []byte(stringField(doc.backing, "body"))
		}
		mime, err := reviewAttachmentMIME(name, content)
		if err != nil {
			return nil, nil, err
		}
		total += len(content)
		if mime == "text/markdown" {
			markdownTotal += len(content)
		}
		if total > reviewAttachmentBytes || markdownTotal > 64<<10 {
			return nil, nil, fmt.Errorf("attachments exceed 16 MiB, or 64 KiB of Markdown")
		}
		digest := sha256.Sum256(content)
		hash := hex.EncodeToString(digest[:])
		if ref.SHA256 != "" && ref.SHA256 != hash {
			return nil, nil, fmt.Errorf("an attachment's contents changed; attach its current version")
		}
		*ref = reviewAttachment{ArtifactID: doc.artifact, Version: doc.version, Revision: doc.revision, SHA256: hash, Name: name, MIME: mime, Size: len(content), URI: "../" + url.PathEscape(doc.artifact) + "/" + url.PathEscape(name)}
		contents = append(contents, content)
	}
	return refs, contents, nil
}

func reviewAttachmentMIME(name string, content []byte) (string, error) {
	if strings.EqualFold(path.Ext(name), ".md") {
		if len(content) == 0 || len(content) > reviewMarkdownBytes || !utf8.Valid(content) || bytes.ContainsRune(content, 0) {
			return "", fmt.Errorf("attach a nonempty UTF-8 Markdown file up to 32 KiB")
		}
		return "text/markdown", nil
	}
	if len(content) == 0 || len(content) > 8<<20 {
		return "", fmt.Errorf("attach an image up to 8 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(content))
	ext := strings.ToLower(path.Ext(name))
	if ext == ".jpg" {
		ext = ".jpeg"
	}
	if err != nil || ext != "."+format || (format != "png" && format != "jpeg" && format != "gif" && format != "webp") || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 24_000_000 {
		return "", fmt.Errorf("attach a readable PNG, JPEG, GIF or WebP image up to 24 million pixels")
	}
	return "image/" + format, nil
}

func proposalAttachmentRefs(proposal map[string]any) ([]reviewAttachment, error) {
	raw, err := json.Marshal(proposal["comments"])
	if err != nil {
		return nil, err
	}
	var comments []struct {
		Attachments []reviewAttachment `json:"attachments"`
	}
	if err = json.Unmarshal(raw, &comments); err != nil {
		return nil, err
	}
	refs := []reviewAttachment{}
	seen := map[string]reviewAttachment{}
	for _, comment := range comments {
		for _, ref := range comment.Attachments {
			if old, ok := seen[ref.ArtifactID]; ok {
				if old != ref {
					return nil, fmt.Errorf("feedback references conflicting versions of an attachment")
				}
				continue
			}
			seen[ref.ArtifactID] = ref
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

type reviewVision interface {
	CallAIVision(context.Context, airoute.ResolveRequest, string, []common.VisionContent) (memql.VisionAIResult, error)
}

// One bounded IO/model operation. DSL owns the prompt, routing level, sequence
// and failure policy; CallAIVision retains the shared journal and model budget.
func (i *Integration) handleRevisionReferences(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	_, captured, _, err := i.revisionRun(ctx, asString(args["requestId"]))
	if err != nil {
		return nil, err
	}
	refs, err := proposalAttachmentRefs(captured)
	if err != nil {
		return nil, err
	}
	refs, contents, err := i.reviewAttachments(ctx, refs)
	if err != nil {
		return nil, err
	}
	files := []map[string]any{}
	labels := []string{}
	images := []common.VisionContent{}
	for n, ref := range refs {
		file := map[string]any{"artifactId": ref.ArtifactID, "name": ref.Name, "mimeType": ref.MIME, "markdownURI": ref.URI}
		if ref.MIME == "text/markdown" {
			file["content"] = string(contents[n])
		} else {
			images = append(images, common.VisionContent{Data: contents[n], MimeType: ref.MIME})
			labels = append(labels, fmt.Sprintf("Image %d: %s (artifact %s)", len(images), ref.Name, ref.ArtifactID))
			file["imageNumber"] = len(images)
		}
		files = append(files, file)
	}
	observations := ""
	if len(images) > 0 {
		level, err := airoute.ParseLevel(asString(args["level"]))
		if err != nil {
			return nil, err
		}
		vision, ok := i.engine.(reviewVision)
		if !ok {
			return nil, fmt.Errorf("this node cannot analyze visual references")
		}
		template := asString(args["templateId"])
		prompt, err := i.engine.RenderPrompt(template, map[string]any{"images": labels})
		if err != nil {
			return nil, err
		}
		result, err := vision.CallAIVision(ctx, airoute.ResolveRequest{Level: level, Modality: airoute.ModalityVision, Needs: airoute.Needs{Vision: true}, PromptName: template}, prompt, images)
		if err != nil {
			return nil, fmt.Errorf("analyzing feedback images: %w", err)
		}
		observations = result.Text
		if strings.TrimSpace(observations) == "" || len(observations) > 32<<10 {
			return nil, fmt.Errorf("image analysis was empty or too large")
		}
	}
	raw, err := json.Marshal(map[string]any{"files": files, "imageObservations": observations})
	if err != nil {
		return nil, err
	}
	return reviewResult(map[string]any{"references": string(raw)})
}

func sameReviewAttachments(value any, refs []reviewAttachment) bool {
	if value == nil {
		return len(refs) == 0
	}
	raw, err := json.Marshal(value)
	var old []reviewAttachment
	if err != nil || json.Unmarshal(raw, &old) != nil || len(old) != len(refs) {
		return false
	}
	for n := range old {
		if old[n] != refs[n] {
			return false
		}
	}
	return true
}
