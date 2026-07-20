package memql

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestSpaceLifecycleMutations_StampOwnerUserId pins memql#401: the
// five lifecycle space mutations (archive / save / restore /
// deleteSpaceNow / rename) must stamp ownerUserId server-side from
// actor.userId regardless of whether the caller's payload includes
// it.
//
// Without the stamp, the v1:cognition:space concept's
// `ownerUserId @required` enforcement rejects the mutation when a
// caller (like the product SPA before the workaround in
// frontend#120) omits ownerUserId, and the failure
// rides back through the gRPC stream as a thrown error -- in the
// worst case silently swallowed by an SDK hook (precedent:
// memql#392 / frontend#114).
//
// The test renders each mutation against a synthesised actor and
// asserts ownerUserId == actor.userId on the resulting payload, even
// when the caller's payload arg omits the field.
func TestSpaceLifecycleMutations_StampOwnerUserId(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})

	// The five lifecycle mutations share an identical shape; table-
	// drive across them. Each entry's source is the same generic
	// `id + ownerUserId + args.payload` upsert, parameterised only by
	// the mutation name so the test fails informatively when a single
	// mutation drifts from the pattern.
	cases := []struct {
		name string
		src  string
	}{
		{
			"mutationArchiveSpace",
			`@actor
mutate space mutationArchiveSpace {
				args {
					partitionId  string  @required
					payload  object  @required
				}
				insert {
					id: args.partitionId
					ownerUserId: actor.userId
					args.payload
				}
			}`,
		},
		{
			"mutationSaveSpace",
			`@actor
mutate space mutationSaveSpace {
				args {
					partitionId  string  @required
					payload  object  @required
				}
				insert {
					id: args.partitionId
					ownerUserId: actor.userId
					args.payload
				}
			}`,
		},
		{
			"mutationRestoreSpace",
			`@actor
mutate space mutationRestoreSpace {
				args {
					partitionId  string  @required
					payload  object  @required
				}
				insert {
					id: args.partitionId
					ownerUserId: actor.userId
					args.payload
				}
			}`,
		},
		{
			"mutationDeleteSpaceNow",
			`@actor
mutate space mutationDeleteSpaceNow {
				args {
					partitionId  string  @required
					payload  object  @required
				}
				insert {
					id: args.partitionId
					ownerUserId: actor.userId
					args.payload
				}
			}`,
		},
		{
			"mutationRenameSpace",
			`@actor
mutate space mutationRenameSpace {
				args {
					partitionId  string  @required
					payload  object  @required
				}
				insert {
					id: args.partitionId
					ownerUserId: actor.userId
					args.payload
				}
			}`,
		},
	}

	const actorUserId = "user-archivist"
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: actorUserId,
		Role:   auth.RoleWriter,
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fn, err := tryParseNewFunctionSyntax(tc.name, "mutation", tc.src, "test.memql", registry)
			require.NoError(t, err)
			require.NotNil(t, fn.MutationTemplate)

			engine := &MemQLEngine{}

			// Payload deliberately OMITS ownerUserId -- this is the
			// pre-#401 SPA caller shape, and the case the issue calls
			// out as silently failing under the @required check.
			mutation, err := engine.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
				"partitionId": "space-001",
				"payload": map[string]any{
					"name":   "Quarterly Review",
					"status": "archived",
					"active": false,
				},
			})
			require.NoError(t, err)
			require.Equal(t, "v1:cognition:space", mutation.Concept)
			require.Equal(t, "space-001", mutation.ID)
			require.NotEmpty(t, mutation.PayloadRaw)

			var payload map[string]any
			require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))

			require.Equal(t, actorUserId, payload["ownerUserId"],
				"%s must stamp ownerUserId from actor.userId; got %v", tc.name, payload["ownerUserId"])
			// Caller payload still lands -- the actor stamp doesn't displace it.
			require.Equal(t, "Quarterly Review", payload["name"])
			require.Equal(t, "archived", payload["status"])
			require.Equal(t, false, payload["active"])
		})
	}
}

// TestSpaceLifecycleMutations_ActorStampOverridesCallerPayload pins
// the precedence contract: when a (legacy) caller threads
// ownerUserId through args.payload AND the mutation stamps from
// actor.userId, the actor wins. This is the load-bearing safety
// property -- a malicious or buggy caller can't override the
// authz-relevant ownerUserId field by stuffing it in the payload.
//
// Per-row authz already gates that only the owner reaches the
// mutation, so payload.ownerUserId == actor.userId in practice, but
// the engine must enforce the rule regardless.
func TestSpaceLifecycleMutations_ActorStampOverridesCallerPayload(t *testing.T) {
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})

	src := `@actor
mutate space mutationArchiveSpace {
		args {
			partitionId  string  @required
			payload  object  @required
		}
		insert {
			id: args.partitionId
			ownerUserId: actor.userId
			args.payload
		}
	}`
	fn, err := tryParseNewFunctionSyntax("mutationArchiveSpace", "mutation", src, "test.memql", registry)
	require.NoError(t, err)

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "user-authentic",
		Role:   auth.RoleWriter,
	})

	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"partitionId": "space-001",
		"payload": map[string]any{
			"ownerUserId": "user-impostor", // legacy / hostile caller
			"name":        "Quarterly Review",
			"status":      "archived",
		},
	})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(mutation.PayloadRaw), &payload))

	// If this assertion ever flips, the engine started honoring the
	// caller's payload over the explicit `ownerUserId: actor.userId`
	// stamp and the authz contract is broken.
	require.Equal(t, "user-authentic", payload["ownerUserId"],
		"actor.userId stamp must override caller-supplied payload.ownerUserId")
}
