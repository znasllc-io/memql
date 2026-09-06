package skills

import (
	"context"

	"path"
	"strings"
)

// capture.go -- copying a script a run DISCOVERED back into the Library
// under the skill that used it.
//
// ===========================================================================
// WHY THIS EXISTS: A TEMPLATE MUST NOT NAME A PATH ON A MACHINE
// ===========================================================================
// A run that solves something on a person's laptop usually solves it with a
// script that was already sitting there. Recording that as
// `/Users/someone/bin/reconcile.sh` produces a template that works on
// exactly one machine, breaks silently when the file is edited, and cannot
// be reviewed -- nobody reading the catalog can see what the step will run.
//
// So capture takes the bytes, puts them in the Library, and rewrites the
// skill's `scripts[]` entry to name the ARTIFACT. From then on runScript
// ships those bytes by hash to wherever the step needs them, and the machine
// the script came from stops being special (spec section C).
//
// IDEMPOTENT BY CONTENT HASH. Capturing the same script twice is the normal
// case -- the same run shape happens every week -- and it must not grow the
// skill a second entry each time.

// Capture refusal codes. As with runScript, each of these means the skill was
// not modified.
const (
	// ErrCaptureUnreadable -- the path could not be read on the surface.
	ErrCaptureUnreadable = "capture_unreadable"
	// ErrCaptureTooLarge -- past MaxVerifiableBytes. The limit is the same
	// one runScript enforces, and for the same reason: a script this cannot
	// read back whole is a script it could never verify at ship time, so
	// capturing it would file something that can never be run.
	ErrCaptureTooLarge = "capture_too_large"
	// ErrCaptureNotWired -- this node has no Library writer. Named rather
	// than silently skipped: a capture that quietly does nothing leaves a
	// template pointing at a machine path forever.
	ErrCaptureNotWired = "capture_not_wired"
	// ErrCaptureFailed -- the Library write or the skill update refused.
	ErrCaptureFailed = "capture_failed"
)

// CaptureRequest names a file on a surface and the skill it belongs to.
type CaptureRequest struct {
	Request
	// Path is where the file is on the far side.
	Path string
	// Platform the script is for. Empty is recorded as `any`, which is the
	// honest reading of "we did not ask": a shell script found on a laptop
	// is not evidence about which platforms it runs on.
	Platform string
	// Entry is the command line. Empty means the file is executed directly.
	Entry string
	// Name overrides the Library file name. Empty uses the base name.
	Name string
}

// Captured is what capture filed.
type Captured struct {
	SkillID     string `json:"skillId"`
	ArtifactID  string `json:"artifactId"`
	ContentHash string `json:"contentHash"`
	Platform    string `json:"platform"`
	Entry       string `json:"entry"`
	Name        string `json:"name"`
	// Changed is false when the skill already held these exact bytes for
	// this platform. The call still succeeded; nothing needed doing.
	Changed bool `json:"changed"`
	// Replaced names the artifact this entry displaced, when a newer script
	// for the same platform arrived. The old artifact stays in the Library
	// -- capture never deletes, because a template pinned to the old id is
	// still a template somebody may be running.
	Replaced string `json:"replaced,omitempty"`
}

// ArtifactWriter files bytes in the Library and answers with the artifact id.
type ArtifactWriter interface {
	WriteArtifact(ctx context.Context, name, mimeType string, data []byte) (artifactID string, err error)
}

// SkillWriter rewrites a skill's `scripts[]`.
type SkillWriter interface {
	SetScripts(ctx context.Context, skillID string, scripts []Script) error
}

// WithLibrary wires the capture half. runScript works without it; capture
// refuses by name without it.
func (r *Runner) WithLibrary(w ArtifactWriter, s SkillWriter) *Runner {
	r.artifactWriter = w
	r.skillWriter = s
	return r
}

// Capture reads a file off a surface and files it under the skill.
func (r *Runner) Capture(ctx context.Context, req CaptureRequest) (Captured, error) {
	if strings.TrimSpace(req.SkillID) == "" {
		return Captured{}, refuse(ErrSkillNotFound, "skillId is required")
	}
	if strings.TrimSpace(req.Path) == "" {
		return Captured{}, refuse(ErrCaptureUnreadable, "path is required")
	}
	if r.artifactWriter == nil || r.skillWriter == nil {
		return Captured{}, refuse(ErrCaptureNotWired,
			"this node cannot file a captured script: no Library writer is wired")
	}

	surface, err := r.chooseSurface(req.Request)
	if err != nil {
		return Captured{}, err
	}

	skill, found, err := r.skills.SkillScripts(ctx, req.SkillID)
	if err != nil {
		return Captured{}, refuse(ErrSkillNotFound, "reading skill %s: %v", req.SkillID, err)
	}
	if !found {
		return Captured{}, refuse(ErrSkillNotFound,
			"skill %s is not readable here -- it does not exist, or it is not yours", req.SkillID)
	}

	data, present, err := r.readRemote(ctx, surface, req.Request, req.Path)
	if err != nil {
		return Captured{}, err
	}
	if !present {
		return Captured{}, refuse(ErrCaptureUnreadable, "%s is not readable on %s", req.Path, surface.Name())
	}
	if len(data) == 0 {
		return Captured{}, refuse(ErrCaptureUnreadable, "%s is empty on %s", req.Path, surface.Name())
	}
	hash := sha256Hex(data)

	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		platform = platformAny
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = path.Base(req.Path)
	}

	// ALREADY HELD? The comparison is by the artifact the entry points at,
	// resolved to its bytes -- not by name and not by path, because both of
	// those can match while the content differs, which is the case capture
	// exists to notice.
	for _, existing := range skill.Scripts {
		if strings.ToLower(strings.TrimSpace(existing.Platform)) != platform {
			continue
		}
		held, err := r.artifacts.ReadArtifact(ctx, existing.ArtifactID)
		if err != nil {
			// An unreadable existing artifact is not a reason to refuse a
			// capture: it is a reason to file the new one.
			break
		}
		if sha256Hex(held.Data) == hash {
			return Captured{
				SkillID: skill.SkillID, ArtifactID: existing.ArtifactID, ContentHash: hash,
				Platform: platform, Entry: existing.Entry, Name: name, Changed: false,
			}, nil
		}
		break
	}

	artifactID, err := r.artifactWriter.WriteArtifact(ctx, name, mimeForScript(name), data)
	if err != nil {
		return Captured{}, refuse(ErrCaptureFailed, "filing %s in the Library: %v", name, err)
	}
	if strings.TrimSpace(artifactID) == "" {
		return Captured{}, refuse(ErrCaptureFailed, "the Library accepted %s and named no artifact", name)
	}

	entry := strings.TrimSpace(req.Entry)
	next := make([]Script, 0, len(skill.Scripts)+1)
	replaced := ""
	for _, existing := range skill.Scripts {
		if strings.ToLower(strings.TrimSpace(existing.Platform)) == platform {
			replaced = existing.ArtifactID
			continue
		}
		next = append(next, existing)
	}
	next = append(next, Script{Platform: platform, ArtifactID: artifactID, Entry: entry})
	next = SortScripts(next)

	if err := r.skillWriter.SetScripts(ctx, skill.SkillID, next); err != nil {
		return Captured{}, refuse(ErrCaptureFailed, "recording the script on skill %s: %v", req.SkillID, err)
	}
	return Captured{
		SkillID: skill.SkillID, ArtifactID: artifactID, ContentHash: hash,
		Platform: platform, Entry: entry, Name: name, Changed: true, Replaced: replaced,
	}, nil
}

// mimeForScript is a courtesy for the Library's own format bucket. Text is
// the honest default: a script is text, and claiming a richer type it cannot
// support would send the analysis pass down a path that fails.
func mimeForScript(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".py":
		return "text/x-python"
	case ".js", ".mjs":
		return "text/javascript"
	case ".json":
		return "application/json"
	case ".md":
		return "text/markdown"
	default:
		return "text/plain"
	}
}
