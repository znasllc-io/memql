package work

// SpineContract versions the native planning scope, independently of a
// customer's source fingerprint. An incompatible host refuses the run.
const SpineContract = "work.spine/1"
const DefaultSpine = "defaultWorkSpine"

// SpineOperations is the complete capability vocabulary of this contract.
// Calling any of these outside an authenticated compile scope is refused.
func SpineOperations() []string {
	return []string{"spineContext", "spineCandidates", "spineUseCandidate", "spineClassify", "spineAcknowledge", "spineRefineSections", "spinePrepareSections", "spineDraft", "spineDesign", "spineEmit", "spineValidate", "spineRepair", "spinePersist", "spineRefuse"}
}

// SpineDraftOperations bind source encoding, not model calls or external effects.
func SpineDraftOperations() []string { return []string{"spineDraftFacts", "spineDraftAppend"} }

// SpineRemedyOperations bind one claimed repair or replan; none is a public authority token.
func SpineRemedyOperations() []string {
	return []string{"spineRemedyContext", "spineRemedyGenerate", "spineRemedyValidate", "spineRemedyPersist", "spineRemedyInstall", "spineRemedyRepair", "spineRemedyAsk"}
}
