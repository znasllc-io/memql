package pipelinesteps

import (
	"fmt"
	"path"
	"slices"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

const (
	ImageBuilderRootlessV1 = "rootless-buildkit-v1"
	imageBuildImage        = "docker.io/moby/buildkit@sha256:f8a833b2de9d68e27f0815e4a737abdfaf8a2e4c615650557df11025101557b4"
	imageBuildStateVolume  = "image-build-state"
	imageBuildStatePath    = "/home/user/.local/share/buildkit"
	imageBuildProfileAMD64 = "700baaefa1069f7a497a41c40d48ca56d928acef43ff17b8051c01b317b15d4a"
	imageBuildProfileARM64 = "6be44c311d1681aa336f133a6176cd4d9f48fd8a300891bf032e48dc16127efa"
)

func imageBuildProfile(platform string) string {
	arch, digest := "arm64", imageBuildProfileARM64
	if platform == "linux/amd64" {
		arch, digest = "amd64", imageBuildProfileAMD64
	}
	return "memql/pipelines/" + ImageBuilderRootlessV1 + "-" + arch + "-" + digest + ".json"
}

func checkImageBuildRun(cfg Config, run StepRun) error {
	if cfg.ImageBuilder != ImageBuilderRootlessV1 {
		return fmt.Errorf("image builds require the operator-installed %s profile", ImageBuilderRootlessV1)
	}
	if cfg.NodePool == "" {
		return fmt.Errorf("image builds require a dedicated operator-selected build pool")
	}
	if err := pl.CheckImageBuild(run.ImageBuild, run.Platform); err != nil {
		return err
	}
	if run.Image != "" || run.Command != "" || len(run.Secrets) != 0 || run.ImagePullSecret != "" ||
		len(run.Services) != 0 || len(run.Caches) != 0 || len(run.Needs) != 0 ||
		!slices.Equal(run.Artifacts, pl.ImageBuildArtifacts()) || run.MemoryMiB < 512 {
		return fmt.Errorf("image builds require their fixed artifacts, at least 512 MiB, and no command, image, secrets, services, caches or host needs")
	}
	return nil
}

// This container contains neither clone credentials nor publication authority.
// RUN processes share only this disposable container's PID namespace with the
// daemon; no service or shared cache is admitted. The collector has its own PID
// namespace and reads the export only after this producer has terminated.
func imageBuildContainer(cfg Config, run StepRun, jobName string) Container {
	compiled := run
	compiled.Image = imageBuildImage
	compiled.Command = imageBuildCommand(run.ImageBuild, run.Platform)
	compiled.Env = nil // arbitrary forwarded variables cannot configure BuildKit
	c := stepContainer(compiled, jobName, "", nil)
	c.Env = append(c.Env,
		plainVar("BUILDKITD_FLAGS", "--oci-worker-no-process-sandbox --oci-worker-snapshotter=native"),
		plainVar("BUILDKIT_HOST", ""),
	)
	c.SecurityContext = &SecurityContext{
		RunAsUser: ptr(int64(1000)), RunAsGroup: ptr(int64(1000)),
		AllowPrivilegeEscalation: ptr(true), // only the pinned image's newuidmap/newgidmap helpers
		Capabilities:             &Capabilities{Drop: []string{capAll}, Add: []string{"SETUID", "SETGID"}},
		SeccompProfile:           &SeccompProfile{Type: "Localhost", LocalhostProfile: imageBuildProfile(run.Platform)},
	}
	c.VolumeMounts = append(c.VolumeMounts, VolumeMount{Name: imageBuildStateVolume, MountPath: imageBuildStatePath})
	if run.CPUMilli == 0 {
		c.Resources.Requests["cpu"] = "1"
		c.Resources.Limits["cpu"] = "2"
	}
	c.Resources.Requests["ephemeral-storage"] = "1Gi"
	c.Resources.Limits["ephemeral-storage"] = cfg.WorkspaceLimit
	return c
}

// Arguments remain individual shell words; a Dockerfile or build argument can
// contain punctuation without becoming a shell program. BuildKit receives no
// entitlement, socket, remote cache, secret, SSH, push or registry-auth option.
func imageBuildCommand(build *pl.ImageBuild, platform string) string {
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	words := []string{"buildctl-daemonless.sh", "build", "--frontend=dockerfile.v0",
		"--local", "context=" + workspacePath + "/" + build.Context,
		"--local", "dockerfile=" + workspacePath + "/" + path.Dir(build.Dockerfile),
		"--opt", "filename=" + path.Base(build.Dockerfile), "--opt", "platform=" + platform,
		"--output", "type=oci,dest=" + workspacePath + "/" + pl.ImageBuildDirectory + "/image.oci.tar",
		"--metadata-file", workspacePath + "/" + pl.ImageBuildDirectory + "/metadata.json"}
	if build.Target != "" {
		words = append(words, "--opt", "target="+build.Target)
	}
	for _, key := range sortedKeys(build.Args) {
		words = append(words, "--opt", "build-arg:"+key+"="+build.Args[key])
	}
	for i, word := range words {
		words[i] = quote(word)
	}
	// Before executing any repository content, refuse context/Dockerfile links
	// that resolve outside the checkout. Removing a source-owned output symlink
	// removes that link itself, never the object it names.
	return "set -eu\n" +
		"for source_path in " + quote(workspacePath+"/"+build.Context) + " " + quote(workspacePath+"/"+build.Dockerfile) + "; do\n" +
		" resolved=$(readlink -f \"$source_path\")\n case \"$resolved\" in /workspace|/workspace/*) ;; *) echo 'image build path escapes its checkout' >&2; exit 2;; esac\ndone\n" +
		"rm -rf -- " + quote(pl.ImageBuildDirectory) + "\nmkdir -- " + quote(pl.ImageBuildDirectory) + "\n" +
		strings.Join(words, " ")
}
