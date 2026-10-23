package inference

// Recommended models mirror the engine catalog for classes 16 through 128.
// Setup also supports a machine that passes the 8 GB GPU floor but is below
// class 16: its 4B fallback leaves room for the embedder and runtime buffers.
// The engine owns the catalog; this copy lets an unpaired machine get started.
// Both repositories test every class against the engine's actual selection.

// The default pair, named as constants because the acceptance criterion
// for this change is that they are asserted BY NAME -- a test comparing
// two expressions that both read defaultGeneral would pass on any pair.
const (
	// DefaultGeneralModel is the 16 GB workhorse: tools, thinking,
	// structured output and vision in one record, and every level of
	// it -- a 16 GB machine reasons on it rather than thrashing against
	// a second model.
	DefaultGeneralModel = "qwen3.5:9b"
	// SmallGeneralModel fits the supported 8 GB GPU floor beside the embedder.
	SmallGeneralModel = "qwen3.5:4b"
	// DefaultEmbeddingModel is the cluster's active embedder. The
	// operations this fleet serves locally are both kinds -- planning and
	// conductor turns, and embeddings -- so a machine that pulled only
	// the first would advertise a set that silently cannot answer half
	// of them. It is the same at every class: the cluster embeds with
	// ONE model, and a bigger embedder nobody calls is memory spent.
	DefaultEmbeddingModel = "qwen3-embedding:0.6b"

	// The larger models, by class (2026-09-08 record, section 4).
	//
	// A single text model plus the embedder fits each class at its budgeted
	// context. The 27B Q4 choice at 24/32 uses 17.7 GB of weights plus about
	// 2.3 GB of cache at 32K; higher contexts need more room.
	strongModel = "qwen3.8:27b"
	// strongModelQ8 is the same model at eight bits, for a machine with
	// the room: 30 GB of weights, 38.7 GB resident at 256K. Nothing
	// larger that is stronger fits 64 GB.
	strongModelQ8 = "qwen3.8:27b-q8_0"
)

// RecommendedSet is the ordered list `setup --inference` pulls for a
// machine of this class.
//
// THE ORDER IS THE PULL ORDER AND IT IS NOT INCIDENTAL. Two things
// depend on it: the closing block reads the machine back in the order
// the ids were asked for, so an operator comparing the two does not
// have to hunt; and the embedder goes LAST on every class, because a
// run interrupted halfway should leave a machine with a general model
// rather than with only an embedder -- the second is a machine that
// answers no prompts at all while looking configured.
//
// The engine computes the same answer from its catalog rows
// (component/memql/fleet_recommend.go over dsl/models/seeds.memql),
// and the two are held together by the engine's resident-budget gate
// and this file's tests: a class whose set here differs from the rows
// is the drift that put a 24 GB card on the 16 GB pair.
func RecommendedSet(class string) []string {
	switch class {
	case "64", "128":
		// A 128 GB machine gets the 64 GB set: nothing on the library
		// above 40B outranks the 27B (qwen3.5:122b is index 16 against
		// its 34), and the one that does is 120 GB with no room for a
		// cache. The extra memory is context headroom.
		return []string{strongModelQ8, DefaultEmbeddingModel}
	case "24", "32":
		// A 32 GB machine gets the 24 GB set with more room for the
		// working context requested by the engine.
		return []string{strongModel, DefaultEmbeddingModel}
	case "16":
		return []string{DefaultGeneralModel, DefaultEmbeddingModel}
	default:
		// unsupported, and anything unrecognised or empty.
		//
		// An UNKNOWN class takes the smallest set rather than none, and
		// the asymmetry is the point: guessing small costs a slower
		// model on a machine that could have run a bigger one, and
		// guessing large fills somebody's disk with a model their GPU
		// cannot hold. The first is a disappointment and the second is
		// a support ticket.
		return []string{SmallGeneralModel, DefaultEmbeddingModel}
	}
}
