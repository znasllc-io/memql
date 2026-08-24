package release

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// status.go -- "are the images for this version there yet", answered honestly.
//
// THREE OUTCOMES, AND THEY STAY THREE. The single design rule of this file is
// that a failed CHECK never becomes a claim about the ARTIFACT:
//
//   present  -> the row moves to images_available.
//   absent   -> the row stays dispatched, and the caller gets the age.
//   errored  -> the error is returned, the row is NOT touched, and the status
//               reported is whatever it already was.
//
// The temptation the third case exists to refuse is collapsing it into
// "absent" -- which is what a naive `exists bool` return would do, and which
// would report a perfectly good release as unbuilt every time GHCR had a bad
// minute. Collapsing it the other way would be worse. So Check returns a
// CheckResult carrying Err, and this function consults Err first.
//
// ON DEMAND ONLY. No poller, no schedule, no @trigger. A cut happens a
// handful of times a month and its images take minutes; a background poller
// would be a permanent process asking a public registry about versions nobody
// is looking at, in exchange for saving one click on the one occasion someone
// is.

// StatusOutcome is what a status check returns to the DSL caller.
type StatusOutcome struct {
	Version     string        `json:"version"`
	BareVersion string        `json:"bareVersion"`
	Status      string        `json:"status"`
	Images      []ImageDetail `json:"images"`
	// Age is how long ago the cut was dispatched, rendered. Empty when the
	// row carries no dispatch time.
	Age string `json:"age,omitempty"`
	// CheckError is set when the CHECK failed. Its presence means Status is
	// the row's PREVIOUS status rather than a fresh verdict, and the
	// portal says so in those words.
	CheckError string `json:"checkError,omitempty"`
	Repository string `json:"repository"`
}

// ImageDetail is one image's answer, for the portal to list.
type ImageDetail struct {
	Repository string `json:"repository"`
	Present    bool   `json:"present"`
}

// Status checks one version's images.
func (i *Integration) Status(ctx context.Context, rawVersion string) (StatusOutcome, error) {
	// The same owner wall as the cut. A status check is a read of the
	// release history plus a registry probe, and both belong to the same
	// operator surface -- offering the check to a caller who cannot cut
	// would leak the cut history through the back door the query's
	// requiresOwner gate closes at the front.
	if _, err := requireOwner(ctx); err != nil {
		return StatusOutcome{}, err
	}

	v, ok := normalizeVersion(strings.TrimSpace(rawVersion))
	if !ok {
		return StatusOutcome{}, refuse(CodeInvalidBump,
			"%q is not a release version. Give it as v1.2.3 or 1.2.3.", rawVersion)
	}

	cfg, err := i.resolver.loadSettings(ctx)
	if err != nil {
		return StatusOutcome{}, err
	}

	row, found, err := i.store.CutByVersion(ctx, v.tag())
	if err != nil {
		return StatusOutcome{}, err
	}
	if !found {
		// DISTINCT from "the images are missing", and the distinction
		// matters: a version cut by hand has no row here and never
		// will, so reporting it as an absent build would be a claim
		// about a registry nobody asked us to make.
		return StatusOutcome{}, refuse(CodeVersionNotCut,
			"this cluster has no record of cutting %s. It may have been cut by hand, or on another installation -- either way there is no row here to move.",
			v.tag())
	}
	previousStatus := asString(row["status"])

	out := StatusOutcome{
		Version:     v.tag(),
		BareVersion: v.bare(),
		Status:      previousStatus,
		Repository:  cfg.repo.String(),
		Age:         ageOf(asString(row["dispatchedAt"]), i.store.now()),
	}

	result := i.registry.Check(ctx, cfg.repo, v)
	for _, img := range result.Images {
		out.Images = append(out.Images, ImageDetail{Repository: img.Repository, Present: img.Present})
	}
	if result.Err != nil {
		// The row is deliberately NOT written. `checkedAt` stays where
		// it was too: stamping it would record that we looked, which
		// invites a later reader to treat the unchanged status as
		// confirmed.
		out.CheckError = describeRefusal(result.Err)
		return out, nil
	}

	if !result.AllPresent {
		// Still dispatched. No write either -- the row already says
		// dispatched, and writing the same value back would append a
		// history entry that records nothing.
		out.Status = "dispatched"
		return out, nil
	}

	out.Status = "images_available"
	if previousStatus != "images_available" {
		if err := i.store.UpdateStatus(ctx, v.tag(), "images_available", ""); err != nil {
			// The images ARE there; that is a verified fact and the
			// caller gets it. Failing to persist it costs one
			// re-check later, so it is logged rather than returned.
			i.logger.Error("release: images verified and the row did not move",
				"component", "integrations.release", "version", v.tag(), "error", err)
		}
	}
	return out, nil
}

// ageOf renders how long ago a cut was dispatched, in the coarsest unit that
// is still informative.
//
// COARSE ON PURPOSE. The question this answers is "should I keep waiting" and
// the useful resolutions are minutes (the build is running), hours (something
// is wrong), days (nobody is coming). Seconds would be noise on a surface
// refreshed by hand.
func ageOf(dispatchedAt string, now time.Time) string {
	if strings.TrimSpace(dispatchedAt) == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, dispatchedAt)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return strconv.Itoa(n) + " " + unit + "s ago"
}
