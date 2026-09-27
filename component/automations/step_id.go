package automations

// WorkStepId is the v1:work:step row id the journal writes for one step key
// of one run: the run's short id and the sanitized key (journal.go's
// workStepId, which states why the id is composed rather than hashed).
//
// EXPORTED FOR THE APP-SESSION DELEGATE (epic memql#5414). A step handed to an
// app arrives there carrying its run id and its step KEY, and the delegate
// stamps the subrun it opens onto the step ROW. Writing to the key named no row
// at all, so every delegated step silently kept no childRunId. The delegate
// derives the row id through this function rather than restating the formula,
// because a second copy is the one that stops agreeing the day the journal's
// id changes -- and nothing would report it, the stamp would just miss again.
func WorkStepId(runId, stepKey string) string {
	return workStepId(runId, stepKey)
}
