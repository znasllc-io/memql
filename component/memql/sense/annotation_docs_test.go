package sense

import "testing"

// TestEveryAnnotationHasDoc guards that every annotation the editor offers
// (AnnotationsByReceiver) carries a hover doc (AnnotationDocs) -- so completion
// + hover never surface a documented-nowhere annotation (#991).
func TestEveryAnnotationHasDoc(t *testing.T) {
	for receiver, anns := range AnnotationsByReceiver {
		label := receiver
		if label == "" {
			label = "Concept"
		}
		for _, a := range anns {
			if _, ok := AnnotationDocs[a]; !ok {
				t.Errorf("%s: annotation @%s is offered but has no AnnotationDocs entry", label, a)
			}
		}
	}
}
