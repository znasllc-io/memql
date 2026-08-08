package memql

// Regeneration goes through `make proto-gen`, which pins protoc as well as the
// two plugins and prefers the pinned copy even when a system protoc exists
// (memql#2774). This directive used to invoke a bare `protoc`, which made
// `make generate` a SECOND generation path that silently disagreed with
// `make proto-gen`: on a machine whose system protoc differed it rewrote the
// `// protoc vX.Y.Z` stamp in all nine generated files, so the
// obvious-sounding target handed the author a diff they had not written
// (memql#3251). TestGoGenerateDirectivesDoNotInvokeBareProtoc keeps it gone.
//
// PROTO_GEN_ONLY scopes the run to this package. The script regenerates every
// proto dir by default, so without it `go generate` over the tree -- which
// reaches all three proto packages -- would do the whole job three times.
//
//go:generate sh -c "cd ../.. && make proto-gen PROTO_GEN_ONLY=component/grpc"
