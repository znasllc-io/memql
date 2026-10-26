package pipelines

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"regexp"
	"slices"
)

const ImageVerificationMaxBytes = 512 << 10
const ImageVerificationMetadataMaxBytes = 64 << 10

// ImageVerificationReceipt records a completed full OCI export which was
// discarded locally. Its hash identifies that temporary output; it is not an
// OCI artifact receipt and conveys no publication authority.
type ImageVerificationReceipt struct {
	SchemaVersion   int               `json:"schemaVersion"`
	Kind            string            `json:"kind"`
	SourceSHA       string            `json:"sourceSHA"`
	Platform        string            `json:"platform"`
	Context         string            `json:"context"`
	Dockerfile      string            `json:"dockerfile"`
	Target          string            `json:"target"`
	Args            map[string]string `json:"args"`
	SuccessfulBuild bool              `json:"successfulBuild"`
	RuntimeProfile  string            `json:"runtimeProfile"`
	OCISHA256       string            `json:"ociSHA256"`
	OCIBytes        int64             `json:"ociBytes"`
	BuildMetadata   json.RawMessage   `json:"buildMetadata"`
}

// CheckImageVerificationStep is repeated at the portable execution seam.
// Only the fixed verification operation may use Fleet; arbitrary commands,
// ambient credentials and source-controlled outputs cannot borrow its grant.
func CheckImageVerificationStep(step Step) error {
	build := step.ImageBuild
	if build == nil || !build.VerificationOnly || step.Kind != StepCommand || step.Execution != ExecutionContainer || step.Platform != "linux/amd64" ||
		step.MemoryMiB < 512 || step.Run != "" || step.Image != "" || step.GitHubReadToken || step.CheckoutHistory != "" || step.ImagePullSecret != "" ||
		len(step.Needs) != 0 || len(step.Services) != 0 || len(step.Caches) != 0 || len(step.Secrets) != 0 || !slices.Equal(step.Artifacts, ImageBuildArtifactsFor(build)) {
		return errors.New("Fleet image verification requires its fixed Linux amd64 container contract and bounded evidence artifact")
	}
	return CheckImageBuild(build, step.Platform)
}

var imageVerificationSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)
var imageVerificationCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ValidateImageVerificationReceipt binds returned bytes to the exact runtime
// request before either cloud or Fleet evidence reaches durable artifact storage.
func ValidateImageVerificationReceipt(raw []byte, sha, platform string, build *ImageBuild) error {
	if len(raw) == 0 || len(raw) > ImageVerificationMaxBytes || build == nil || !build.VerificationOnly || !imageVerificationCommit.MatchString(sha) {
		return errors.New("image verification evidence exceeds its bound or has no exact request")
	}
	var receipt ImageVerificationReceipt
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("image verification evidence has trailing data")
	}
	if receipt.SchemaVersion != 1 || receipt.Kind != "image-verification" || receipt.SourceSHA != sha || receipt.Platform != platform ||
		receipt.Context != build.Context || receipt.Dockerfile != build.Dockerfile || receipt.Target != build.Target || !maps.Equal(receipt.Args, build.Args) ||
		!receipt.SuccessfulBuild || receipt.RuntimeProfile != "rootless-buildkit-v1" || !imageVerificationSHA.MatchString(receipt.OCISHA256) || receipt.OCIBytes <= 0 ||
		len(receipt.BuildMetadata) == 0 || len(receipt.BuildMetadata) > ImageVerificationMetadataMaxBytes {
		return errors.New("image verification evidence differs from its completed request")
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(receipt.BuildMetadata, &metadata) != nil || metadata == nil {
		return errors.New("image verification metadata must be a bounded object")
	}
	return nil
}
