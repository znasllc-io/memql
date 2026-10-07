package pipelines

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// PublishedRelease is a portable, deliberately public projection of native
// publication evidence. It contains no owner, work/Library references, secrets,
// or connection trust. A digest identifies this projection; only a signature
// under independently configured publisher trust authenticates it.
type PublishedRelease struct {
	FormatVersion    int                    `json:"formatVersion"`
	Publisher        string                 `json:"publisher"`
	CandidateID      string                 `json:"candidateId"`
	ApprovalID       string                 `json:"approvalId"`
	WorkflowDigest   string                 `json:"workflowDigest"`
	ProvenanceDigest string                 `json:"provenanceDigest"`
	Components       []PublishedComponent   `json:"components"`
	Compatibility    []ReleaseCompatibility `json:"compatibility"`
}

type PublishedComponent struct {
	Name       string              `json:"name"`
	Version    string              `json:"version"`
	Repository string              `json:"repository"`
	Commit     string              `json:"commit"`
	Artifacts  []PublishedArtifact `json:"artifacts"`
}

type PublishedArtifact struct {
	Name        string              `json:"name"`
	Kind        string              `json:"kind"`
	Platform    string              `json:"platform"`
	Digest      string              `json:"digest"`
	Size        int64               `json:"size"`
	ImageDigest string              `json:"imageDigest,omitempty"`
	Locations   []PublishedLocation `json:"locations"`
}

type PublishedLocation struct {
	Kind       string `json:"kind"` // oci or github-release
	Origin     string `json:"origin"`
	Repository string `json:"repository"`
	ReleaseID  int64  `json:"releaseId,omitempty"`
	AssetID    int64  `json:"assetId,omitempty"`
	AssetName  string `json:"assetName,omitempty"`
	Tag        string `json:"tag,omitempty"`
}

const ReleaseCatalogPayloadType = "application/vnd.memql.published-release.v1+json"
const maxReleaseCatalogBytes = 1 << 20

// ReleaseProvenanceDigest binds the private, already verified evidence without
// publishing its owner or work/Library references. The signature attests to the
// verification; this digest alone does not prove success.
func ReleaseProvenanceDigest(c ReleaseCandidate) (string, error) {
	body, _, err := CanonicalReleaseCandidate(c)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(append([]byte("memql.release-provenance.v1\x00"), body...))
	return "sha256:" + hex.EncodeToString(hash[:]), nil
}

// CanonicalPublishedRelease validates structure, exact artifact addresses and
// compatibility. It is not proof that anything was built or published.
func CanonicalPublishedRelease(input PublishedRelease) ([]byte, string, error) {
	b, err := json.Marshal(input)
	if err != nil || len(b) > maxReleaseCatalogBytes {
		return nil, "", errors.New("release catalog exceeds its bound")
	}
	var c PublishedRelease
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, "", err
	}
	if c.FormatVersion != 1 || !releaseName.MatchString(c.Publisher) || !releaseDigest(c.CandidateID) || !releaseIdentity(c.ApprovalID) || !releaseDigest(c.WorkflowDigest) || !releaseDigest(c.ProvenanceDigest) || len(c.Components) == 0 || len(c.Components) > 64 || len(c.Compatibility) > 256 {
		return nil, "", errors.New("invalid published release identity")
	}
	components := map[string]PublishedComponent{}
	locationCount := 0
	for ci := range c.Components {
		component := &c.Components[ci]
		if !releaseName.MatchString(component.Name) || components[component.Name].Name != "" || !releaseVersion.MatchString(component.Version) || !releaseSource.MatchString(component.Repository) || len(component.Repository) > 257 || !releaseCommit.MatchString(component.Commit) || len(component.Artifacts) == 0 || len(component.Artifacts) > 128 {
			return nil, "", errors.New("invalid published component")
		}
		components[component.Name] = *component
		names := map[string]bool{}
		for ai := range component.Artifacts {
			a := &component.Artifacts[ai]
			if !releaseName.MatchString(a.Name) || names[a.Name] || !releaseDigest(a.Digest) || a.Size < 0 || a.Size > 2<<30 || len(a.Locations) == 0 || len(a.Locations) > 1024 {
				return nil, "", errors.New("invalid published artifact")
			}
			names[a.Name] = true
			if (a.Kind == "oci" && (!releaseDigest(a.ImageDigest) || (a.Platform != "linux/amd64" && a.Platform != "linux/arm64"))) || (a.Kind == "file" && (a.ImageDigest != "" || !releaseIdentity(a.Platform))) || (a.Kind != "oci" && a.Kind != "file") {
				return nil, "", errors.New("invalid published artifact kind")
			}
			locations := map[string]bool{}
			for _, loc := range a.Locations {
				locationCount++
				u, e := url.Parse(loc.Origin)
				if e != nil || len(loc.Origin) > 2048 || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && u.Scheme != "http") || !releaseIdentity(loc.Repository) || strings.ContainsAny(loc.Repository, "?#@%\\") || strings.HasPrefix(loc.Repository, "/") || strings.HasSuffix(loc.Repository, "/") {
					return nil, "", errors.New("invalid published location")
				}
				for _, part := range strings.Split(loc.Repository, "/") {
					if part == "" || part == "." || part == ".." {
						return nil, "", errors.New("invalid published repository")
					}
				}
				if a.Kind == "oci" {
					if loc.Kind != "oci" || loc.ReleaseID != 0 || loc.AssetID != 0 || loc.AssetName != "" || loc.Tag != "" {
						return nil, "", errors.New("OCI location cannot contain release fields")
					}
				} else if loc.Kind != "github-release" || loc.ReleaseID <= 0 || loc.AssetID <= 0 || !releaseIdentity(loc.AssetName) || strings.ContainsAny(loc.AssetName, "/\\") || !releaseIdentity(loc.Tag) || loc.Repository != component.Repository {
					return nil, "", errors.New("file location requires immutable release and asset identities")
				}
				key := publishedLocationKey(loc)
				if locations[key] {
					return nil, "", errors.New("duplicate publication location")
				}
				locations[key] = true
			}
			slices.SortFunc(a.Locations, func(a, b PublishedLocation) int {
				return strings.Compare(publishedLocationKey(a), publishedLocationKey(b))
			})
		}
		slices.SortFunc(component.Artifacts, func(a, b PublishedArtifact) int { return strings.Compare(a.Name, b.Name) })
	}
	if locationCount > 1024 {
		return nil, "", errors.New("published locations exceed bound")
	}
	rules := map[string]bool{}
	for _, rule := range c.Compatibility {
		required, ok := components[rule.Requires]
		key := rule.Component + "/" + rule.Requires
		if !ok || components[rule.Component].Name == "" || rule.Component == rule.Requires || rules[key] || !releaseVersion.MatchString(rule.MinVersion) || !releaseVersion.MatchString(rule.MaxExclusive) || compareReleaseVersion(rule.MinVersion, rule.MaxExclusive) >= 0 || compareReleaseVersion(required.Version, rule.MinVersion) < 0 || compareReleaseVersion(required.Version, rule.MaxExclusive) >= 0 {
			return nil, "", errors.New("incompatible published components")
		}
		rules[key] = true
	}
	slices.SortFunc(c.Components, func(a, b PublishedComponent) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(c.Compatibility, func(a, b ReleaseCompatibility) int {
		return strings.Compare(a.Component+"/"+a.Requires, b.Component+"/"+b.Requires)
	})
	body, err := json.Marshal(c)
	if err != nil {
		return nil, "", err
	}
	hash := sha256.Sum256(append([]byte("memql.published-release.v1\x00"), body...))
	return body, "sha256:" + hex.EncodeToString(hash[:]), nil
}

func publishedLocationKey(l PublishedLocation) string { b, _ := json.Marshal(l); return string(b) }

type releaseEnvelope struct {
	PayloadType string             `json:"payloadType"`
	Payload     string             `json:"payload"`
	Signatures  []releaseSignature `json:"signatures"`
}
type releaseSignature struct {
	KeyID     string `json:"keyid"`
	Signature string `json:"sig"`
}

// DSSE v1 PAE authenticates the schema and exact payload bytes together.
// https://github.com/secure-systems-lab/dsse/blob/master/protocol.md
func releasePAE(payloadType string, body []byte) []byte {
	return append([]byte("DSSEv1 "+strconv.Itoa(len(payloadType))+" "+payloadType+" "+strconv.Itoa(len(body))+" "), body...)
}

func SignPublishedRelease(c PublishedRelease, keyID string, key ed25519.PrivateKey) ([]byte, error) {
	if !releaseName.MatchString(keyID) || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid release signing key")
	}
	body, _, err := CanonicalPublishedRelease(c)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, releasePAE(ReleaseCatalogPayloadType, body))
	return json.Marshal(releaseEnvelope{PayloadType: ReleaseCatalogPayloadType, Payload: base64.StdEncoding.EncodeToString(body), Signatures: []releaseSignature{{KeyID: keyID, Signature: base64.StdEncoding.EncodeToString(sig)}}})
}

// VerifiedPublishedRelease can only be constructed by signature verification.
// Zero values are invalid. Access returns a copy, so caller mutations cannot
// alter evidence already accepted by an installation preparer.
type VerifiedPublishedRelease struct {
	body   []byte
	digest string
}

func (v VerifiedPublishedRelease) Release() (PublishedRelease, error) {
	var c PublishedRelease
	if len(v.body) == 0 {
		return c, errors.New("no verified publication")
	}
	err := json.Unmarshal(v.body, &c)
	return c, err
}
func (v VerifiedPublishedRelease) Digest() string { return v.digest }

func VerifyPublishedRelease(envelope []byte, publisher string, trusted map[string]ed25519.PublicKey) (VerifiedPublishedRelease, error) {
	var out VerifiedPublishedRelease
	if !releaseName.MatchString(publisher) || len(trusted) == 0 || len(trusted) > 32 || len(envelope) > 2*maxReleaseCatalogBytes {
		return out, errors.New("release verification requires bounded publisher trust")
	}
	var e releaseEnvelope
	if err := decodeReleaseJSON(envelope, &e); err != nil {
		return out, err
	}
	if e.PayloadType != ReleaseCatalogPayloadType || len(e.Signatures) == 0 || len(e.Signatures) > 32 {
		return out, errors.New("unsupported release envelope")
	}
	body, err := releaseBase64(e.Payload)
	if err != nil || len(body) > maxReleaseCatalogBytes {
		return out, errors.New("invalid release payload")
	}
	verified := false
	for _, s := range e.Signatures {
		key := trusted[s.KeyID]
		sig, err := releaseBase64(s.Signature)
		if err == nil && len(key) == ed25519.PublicKeySize && ed25519.Verify(key, releasePAE(e.PayloadType, body), sig) {
			verified = true
		}
	}
	if !verified {
		return out, errors.New("release signature does not match configured trust")
	}
	var c PublishedRelease
	if err := decodeReleaseJSON(body, &c); err != nil {
		return out, err
	}
	canonical, digest, err := CanonicalPublishedRelease(c)
	if err != nil {
		return out, err
	}
	if c.Publisher != publisher || !bytes.Equal(body, canonical) {
		return out, errors.New("release payload differs from its canonical publisher contract")
	}
	return VerifiedPublishedRelease{body: canonical, digest: digest}, nil
}

func releaseBase64(s string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding.Strict(), base64.URLEncoding.Strict(), base64.RawStdEncoding.Strict(), base64.RawURLEncoding.Strict()} {
		if b, e := encoding.DecodeString(s); e == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

func decodeReleaseJSON(body []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing release JSON")
	}
	// Exact field names and unique fields: compare generic JSON with the typed
	// form. Duplicate keys require a separate token pass; a map loses them.
	tokens := json.NewDecoder(bytes.NewReader(body))
	if err := uniqueReleaseJSON(tokens, 0); err != nil {
		return err
	}
	var raw any
	d = json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if err := d.Decode(&raw); err != nil {
		return err
	}
	typed, err := json.Marshal(into)
	if err != nil {
		return err
	}
	var normalized any
	d = json.NewDecoder(bytes.NewReader(typed))
	d.UseNumber()
	if err := d.Decode(&normalized); err != nil {
		return err
	}
	a, _ := json.Marshal(raw)
	b, _ := json.Marshal(normalized)
	if !bytes.Equal(a, b) {
		return errors.New("noncanonical release JSON fields")
	}
	return nil
}

func uniqueReleaseJSON(d *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("release JSON nesting exceeds bound")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := key.(string)
			if !ok || seen[strings.ToLower(s)] {
				return errors.New("duplicate release JSON key")
			}
			seen[strings.ToLower(s)] = true
			if e := uniqueReleaseJSON(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := uniqueReleaseJSON(d, depth+1); e != nil {
				return e
			}
		}
	default:
		return fmt.Errorf("unexpected release JSON delimiter %q", delimiter)
	}
	_, err = d.Token()
	return err
}
