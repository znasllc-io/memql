# Cluster, Concepts and Users section moves

This companion to the Profile/navigation change implements the approved
accounts-program app split (P36), using the Fleet details pattern (P7/P38).

| Destination | Retained behavior | Verification |
| --- | --- | --- |
| Cluster Overview and Cluster facts | Original facts and recorded/live distinctions; mail refresh; owner-only infrastructure read and refusal; no policy read or edit | Existing cluster tests, including planted-secret absence; new composition gates |
| Cluster Benchmarks | Existing measurement, absence, spread and refusal behavior | Existing benchmark tests through ClusterApp |
| Concepts Language | Existing current/deprecated forms, copy actions and source reads | Existing language tests through ConceptsApp |
| Users Access | Existing effective access, grant/revoke and operation feedback | Existing access tests through UsersApp |

The moved surfaces keep their original app and section capability names in the
companion registry change. Inline Overview facts check both original doors. A
facts-only or benchmarks-only visit never mounts Runtime; an Access-only visit
never mounts the Users directory. Access requests and audit/data destinations
are supplied by the companion Identity/Settings change. Policy lifetime and
range tests now render the same PolicyPanel directly, preserving that coverage.

Local component checks passed 61 tests in three files plus eight admission
checks. The four registry/navigation cases require the companion registry; the
full combined suite and rendered desktop/narrow light/dark review must be
recorded by the integration lane. No production or server authority changes.
