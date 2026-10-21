package release

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
)

// The full approved configuration stays private. Historical catalog projection
// reads this frozen evidence, never today's operator configuration. Secret
// references are included in the approval hash; secret material is never stored.
type candidatePublicationTarget struct {
	Registry *candidateRegistryTarget `json:"registry,omitempty"`
	File     *candidateFileTarget     `json:"file,omitempty"`
	NPM      *candidateNPMTarget      `json:"npm,omitempty"`
}

func (t candidatePublicationTarget) validate(d pl.ReleaseDestination) error {
	var key, component, artifact, digest string
	var err error
	if t.Registry != nil && t.File == nil && t.NPM == nil {
		key, component, artifact = t.Registry.ID, t.Registry.Component, t.Registry.Artifact
		_, _, digest, err = t.Registry.snapshot()
	} else if t.File != nil && t.Registry == nil && t.NPM == nil {
		key, component, artifact = t.File.ID, t.File.Component, t.File.Artifact
		_, _, digest, err = t.File.snapshot()
	} else if t.NPM != nil && t.Registry == nil && t.File == nil {
		key, component, artifact = t.NPM.ID, t.NPM.Component, t.NPM.Artifact
		_, _, digest, err = t.NPM.snapshot()
	} else {
		return errors.New("publication must freeze exactly one target")
	}
	if err != nil {
		return err
	}
	if d.Operation != "publish" || key != d.TargetID || component != d.Component || artifact != d.Artifact || digest != d.TargetDigest {
		return errors.New("frozen publication target differs from approval")
	}
	return nil
}

func (r candidateTargetReader) publicationTarget(ctx context.Context, d pl.ReleaseDestination) (candidatePublicationTarget, error) {
	if _, ok := r.npm[d.TargetID]; ok {
		t, _, err := r.resolveNPM(ctx, d)
		return candidatePublicationTarget{NPM: &t}, err
	}
	if _, ok := r.files[d.TargetID]; ok {
		t, _, err := r.resolveFile(ctx, d)
		return candidatePublicationTarget{File: &t}, err
	}
	t, _, err := r.resolve(ctx, d)
	return candidatePublicationTarget{Registry: &t}, err
}

func (l *candidateLedger) freezePublicationTarget(ctx context.Context, intent candidatePublication, target candidatePublicationTarget) error {
	if err := target.validate(intent.Target); err != nil {
		return err
	}
	tx, owner, err := l.draftTransaction(ctx, intent.CandidateID, intent.ApprovalID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rec, err := readCandidate(ctx, tx, owner, intent.CandidateID)
	if err != nil {
		return err
	}
	expected, err := candidatePublicationFor(rec, intent.Target.TargetID, intent.Target.Component, intent.Target.Artifact)
	if err != nil {
		return err
	}
	if err := target.validate(expected.Target); err != nil {
		return err
	}
	var tagKey any
	if target.Registry != nil && target.Registry.Tag != "" {
		matched := false
		for _, component := range rec.Manifest.Components {
			if component.Name == expected.Target.Component && (target.Registry.Tag == component.Version || target.Registry.Tag == "v"+component.Version) {
				matched = true
			}
		}
		if !matched {
			return errors.New("registry tag differs from approved component version")
		}
		tagKey = registryTagKey(*target.Registry)
	}
	if target.NPM != nil {
		matched := false
		for _, component := range rec.Manifest.Components {
			if component.Name == expected.Target.Component && component.Version == target.NPM.Version {
				matched = true
			}
		}
		if !matched {
			return errors.New("npm version differs from approved component version")
		}
		tagKey = npmVersionKey(*target.NPM)
	}
	var body []byte
	var storedTagKey sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT target_snapshot,registry_tag_key FROM release_publication_intents WHERE effect_id=$1 AND candidate_id=$2 AND approval_id=$3 AND target_id=$4 AND component_name=$5 AND artifact_name=$6 FOR UPDATE`, intent.EffectID, rec.ID, intent.ApprovalID, expected.Target.TargetID, expected.Target.Component, expected.Target.Artifact).Scan(&body, &storedTagKey)
	if err != nil {
		return err
	}
	if len(body) != 0 {
		var stored candidatePublicationTarget
		if err := decodeCandidateObject(body, &stored); err != nil {
			return err
		}
		if err := stored.validate(expected.Target); err != nil {
			return err
		}
		if (tagKey == nil && storedTagKey.Valid) || (tagKey != nil && (!storedTagKey.Valid || tagKey != storedTagKey.String)) {
			return errors.New("stored tag reservation differs from approved destination")
		}
		return tx.Commit()
	}
	body, err = json.Marshal(target)
	if err != nil {
		return err
	}
	// The unique key permanently reserves a tag for one approved effect, before
	// any registry write. It serializes different candidates/owners/replicas
	// without a process-local lock or a transaction held during network I/O.
	_, err = tx.ExecContext(ctx, `UPDATE release_publication_intents SET target_snapshot=$2::jsonb,registry_tag_key=$3 WHERE effect_id=$1`, intent.EffectID, string(body), tagKey)
	if err != nil {
		return errors.New("publication target could not be reserved; a version tag may already belong to another release")
	}
	return tx.Commit()
}

func registryTagKey(t candidateRegistryTarget) string {
	// snapshot() has already validated the URL. Normalize aliases of the same
	// origin so spelling and the default port cannot evade the shared claim.
	u, _ := url.Parse(t.Origin)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "sha256:" + string(id.NewUntracked().FromString(u.Scheme+"://"+host+"/"+t.Repository+":"+t.Tag))
}

func readPublicationTarget(ctx context.Context, tx *sql.Tx, candidateID, approvalID string, d pl.ReleaseDestination) (candidatePublicationTarget, candidatePublicationReceipt, error) {
	var target candidatePublicationTarget
	var proof candidatePublicationReceipt
	var state, approval string
	var snapshot, receipt []byte
	err := tx.QueryRowContext(ctx, `SELECT state,approval_id,target_snapshot,receipt FROM release_publication_intents WHERE candidate_id=$1 AND target_id=$2 AND component_name=$3 AND artifact_name=$4`, candidateID, d.TargetID, d.Component, d.Artifact).Scan(&state, &approval, &snapshot, &receipt)
	if err != nil {
		return target, proof, err
	}
	if state != "complete" || approval != approvalID {
		return target, proof, errors.New("catalog requires every exact approved publication complete")
	}
	if err := decodeCandidateObject(snapshot, &target); err != nil {
		return target, proof, err
	}
	if err := target.validate(d); err != nil {
		return target, proof, err
	}
	if err := decodeCandidateObject(receipt, &proof); err != nil {
		return target, proof, err
	}
	return target, proof, nil
}
