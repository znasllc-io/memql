package release

import pl "github.com/znasllc-io/memql/component/pipelines"

// adoptableBuildRun decides whether a saved release replaces the run it
// recorded with observed, a newer attempt of the same release run key. It is
// the one place a saved release's execution identity may change
// (docs/superpowers/specs/2026-10-28-release-failed-only-rerun-design.md).
//
//   - Trigger: observation found a newer attempt than the recorded one. The
//     owner re-ran the failed release, usually its failed steps only.
//   - Evidence: the native attempt chain (pl.ReleaseBuildRun.Chain), which is
//     built only through rerunOf inside the release run key, and the recorded
//     attempt's lifecycle as that chain reports it now, not as the journal
//     last saw it.
//   - Adoption: only a descendant of a recorded attempt that finished without
//     success (failed, refused or cancelled), and only before a candidate
//     exists or publication starts. A successful or running recorded attempt
//     is never replaced, and a prepared candidate is never re-pointed.
//   - Anything else keeps the caller's refusal, so a broken chain or an
//     unrelated run changes nothing.
//
// Candidate preparation then proves each step the adopted attempt carried
// against the attempt that executed it (candidate_carried.go).
func adoptableBuildRun(record releaseBuildRecord, observed pl.ReleaseBuildRun) bool {
	recorded := record.Run
	if recorded == nil || recorded.ID == observed.ID || record.CandidateID != "" || record.PublicationStarted {
		return false
	}
	for _, earlier := range observed.Chain {
		if earlier.ID == recorded.ID {
			return earlier.Status == "completed" && (earlier.Conclusion != "success" || earlier.CancelRequested)
		}
	}
	return false
}
