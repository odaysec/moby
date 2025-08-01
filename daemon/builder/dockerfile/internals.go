package dockerfile

// internals for handling commands. Covers many areas and a lot of
// non-contiguous functionality. Please read the comments.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/docker/docker/daemon/builder"
	"github.com/docker/docker/daemon/internal/image"
	"github.com/docker/docker/daemon/internal/stringid"
	networkSettings "github.com/docker/docker/daemon/network"
	"github.com/docker/docker/daemon/server/backend"
	"github.com/docker/go-connections/nat"
	"github.com/moby/go-archive"
	"github.com/moby/go-archive/chrootarchive"
	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

func (b *Builder) getArchiver() *archive.Archiver {
	return chrootarchive.NewArchiver(b.idMapping)
}

func (b *Builder) commit(ctx context.Context, dispatchState *dispatchState, comment string) error {
	if b.disableCommit {
		return nil
	}
	if !dispatchState.hasFromImage() {
		return errors.New("Please provide a source image with `from` prior to commit")
	}

	runConfigWithCommentCmd := copyRunConfig(dispatchState.runConfig, withCmdComment(comment, dispatchState.operatingSystem))
	id, err := b.probeAndCreate(ctx, dispatchState, runConfigWithCommentCmd)
	if err != nil || id == "" {
		return err
	}

	return b.commitContainer(ctx, dispatchState, id, runConfigWithCommentCmd)
}

func (b *Builder) commitContainer(ctx context.Context, dispatchState *dispatchState, id string, containerConfig *container.Config) error {
	if b.disableCommit {
		return nil
	}

	commitCfg := backend.CommitConfig{
		Author: dispatchState.maintainer,
		// TODO: this copy should be done by Commit()
		Config:          copyRunConfig(dispatchState.runConfig),
		ContainerConfig: containerConfig,
		ContainerID:     id,
	}

	imageID, err := b.docker.CommitBuildStep(ctx, commitCfg)
	dispatchState.imageID = string(imageID)
	return err
}

func (b *Builder) exportImage(ctx context.Context, state *dispatchState, layer builder.RWLayer, parent builder.Image, runConfig *container.Config) error {
	newLayer, err := layer.Commit()
	if err != nil {
		return err
	}

	parentImage, ok := parent.(*image.Image)
	if !ok {
		return errors.Errorf("unexpected image type")
	}

	platform := &ocispec.Platform{
		OS:           parentImage.OS,
		Architecture: parentImage.Architecture,
		Variant:      parentImage.Variant,
	}

	// add an image mount without an image so the layer is properly unmounted
	// if there is an error before we can add the full mount with image
	b.imageSources.Add(newImageMount(nil, newLayer), platform)

	newImage := image.NewChildImage(parentImage, image.ChildConfig{
		Author:          state.maintainer,
		ContainerConfig: runConfig,
		DiffID:          newLayer.DiffID(),
		Config:          copyRunConfig(state.runConfig),
	}, parentImage.OS)

	// TODO: it seems strange to marshal this here instead of just passing in the
	// image struct
	config, err := newImage.MarshalJSON()
	if err != nil {
		return errors.Wrap(err, "failed to encode image config")
	}

	// when writing the new image's manifest, we now need to pass in the new layer's digest.
	// before the containerd store work this was unnecessary since we get the layer id
	// from the image's RootFS ChainID -- see:
	// https://github.com/moby/moby/blob/8cf66ed7322fa885ef99c4c044fa23e1727301dc/image/store.go#L162
	// however, with the containerd store we can't do this. An alternative implementation here
	// without changing the signature would be to get the layer digest by walking the content store
	// and filtering the objects to find the layer with the DiffID we want, but that has performance
	// implications that should be called out/investigated
	exportedImage, err := b.docker.CreateImage(ctx, config, state.imageID, newLayer.ContentStoreDigest())
	if err != nil {
		return errors.Wrapf(err, "failed to export image")
	}

	state.imageID = exportedImage.ImageID()
	b.imageSources.Add(newImageMount(exportedImage, newLayer), platform)
	return nil
}

func (b *Builder) performCopy(ctx context.Context, req dispatchRequest, inst copyInstruction) error {
	state := req.state
	srcHash := getSourceHashFromInfos(inst.infos)

	var chownComment string
	if inst.chownStr != "" {
		chownComment = fmt.Sprintf("--chown=%s ", inst.chownStr)
	}
	commentStr := fmt.Sprintf("%s %s%s in %s ", inst.cmdName, chownComment, srcHash, inst.dest)

	// TODO: should this have been using origPaths instead of srcHash in the comment?
	runConfigWithCommentCmd := copyRunConfig(state.runConfig, withCmdCommentString(commentStr, state.operatingSystem))
	hit, err := b.probeCache(state, runConfigWithCommentCmd)
	if err != nil || hit {
		return err
	}

	imgMount, err := b.imageSources.Get(ctx, state.imageID, true, req.builder.platform)
	if err != nil {
		return errors.Wrapf(err, "failed to get destination image %q", state.imageID)
	}

	rwLayer, err := imgMount.NewRWLayer()
	if err != nil {
		return err
	}
	defer rwLayer.Release()

	destInfo, err := createDestInfo(state.runConfig.WorkingDir, inst, rwLayer)
	if err != nil {
		return err
	}

	uid, gid := b.idMapping.RootPair()
	id := identity{UID: uid, GID: gid}
	// if a chown was requested, perform the steps to get the uid, gid
	// translated (if necessary because of user namespaces), and replace
	// the root pair with the chown pair for copy operations
	if inst.chownStr != "" {
		id, err = parseChownFlag(ctx, b, state, inst.chownStr, destInfo.root, b.idMapping)
		if err != nil {
			if b.options.Platform != "windows" {
				return errors.Wrapf(err, "unable to convert uid/gid chown string to host mapping")
			}

			return errors.Wrapf(err, "unable to map container user account name to SID")
		}
	}

	for _, info := range inst.infos {
		opts := copyFileOptions{
			decompress: inst.allowLocalDecompression,
			archiver:   b.getArchiver(),
		}
		if !inst.preserveOwnership {
			opts.identity = &id
		}
		if err := performCopyForInfo(destInfo, info, opts); err != nil {
			return errors.Wrapf(err, "failed to copy files")
		}
	}
	return b.exportImage(ctx, state, rwLayer, imgMount.Image(), runConfigWithCommentCmd)
}

func createDestInfo(workingDir string, inst copyInstruction, rwLayer builder.RWLayer) (copyInfo, error) {
	// Twiddle the destination when it's a relative path - meaning, make it
	// relative to the WORKINGDIR
	dest, err := normalizeDest(workingDir, inst.dest)
	if err != nil {
		return copyInfo{}, errors.Wrapf(err, "invalid %s", inst.cmdName)
	}

	return copyInfo{root: rwLayer.Root(), path: dest}, nil
}

// For backwards compat, if there's just one info then use it as the
// cache look-up string, otherwise hash 'em all into one
func getSourceHashFromInfos(infos []copyInfo) string {
	if len(infos) == 1 {
		return infos[0].hash
	}
	var hashs []string
	for _, info := range infos {
		hashs = append(hashs, info.hash)
	}
	return hashStringSlice("multi", hashs)
}

func hashStringSlice(prefix string, slice []string) string {
	hasher := sha256.New()
	hasher.Write([]byte(strings.Join(slice, ",")))
	return prefix + ":" + hex.EncodeToString(hasher.Sum(nil))
}

type runConfigModifier func(*container.Config)

func withCmd(cmd []string) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Cmd = cmd
	}
}

func withArgsEscaped(argsEscaped bool) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.ArgsEscaped = argsEscaped
	}
}

// withCmdComment sets Cmd to a nop comment string. See withCmdCommentString for
// why there are two almost identical versions of this.
func withCmdComment(comment string, platform string) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Cmd = append(getShell(runConfig, platform), "#(nop) ", comment)
	}
}

// withCmdCommentString exists to maintain compatibility with older versions.
// A few instructions (workdir, copy, add) used a nop comment that is a single arg
// where as all the other instructions used a two arg comment string. This
// function implements the single arg version.
func withCmdCommentString(comment string, platform string) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Cmd = append(getShell(runConfig, platform), "#(nop) "+comment)
	}
}

func withEnv(env []string) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Env = env
	}
}

// withEntrypointOverride sets an entrypoint on runConfig if the command is
// not empty. The entrypoint is left unmodified if command is empty.
//
// The dockerfile RUN instruction expect to run without an entrypoint
// so the runConfig entrypoint needs to be modified accordingly. ContainerCreate
// will change a []string{""} entrypoint to nil, so we probe the cache with the
// nil entrypoint.
func withEntrypointOverride(cmd []string, entrypoint []string) runConfigModifier {
	return func(runConfig *container.Config) {
		if len(cmd) > 0 {
			runConfig.Entrypoint = entrypoint
		}
	}
}

// withoutHealthcheck disables healthcheck.
//
// The dockerfile RUN instruction expect to run without healthcheck
// so the runConfig Healthcheck needs to be disabled.
func withoutHealthcheck() runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Healthcheck = &container.HealthConfig{
			Test: []string{"NONE"},
		}
	}
}

func copyRunConfig(runConfig *container.Config, modifiers ...runConfigModifier) *container.Config {
	cfgCopy := *runConfig
	cfgCopy.Cmd = copyStringSlice(runConfig.Cmd)
	cfgCopy.Env = copyStringSlice(runConfig.Env)
	cfgCopy.Entrypoint = copyStringSlice(runConfig.Entrypoint)
	cfgCopy.OnBuild = copyStringSlice(runConfig.OnBuild)
	cfgCopy.Shell = copyStringSlice(runConfig.Shell)

	if cfgCopy.Volumes != nil {
		cfgCopy.Volumes = make(map[string]struct{}, len(runConfig.Volumes))
		for k, v := range runConfig.Volumes {
			cfgCopy.Volumes[k] = v
		}
	}

	if cfgCopy.ExposedPorts != nil {
		cfgCopy.ExposedPorts = make(nat.PortSet, len(runConfig.ExposedPorts))
		for k, v := range runConfig.ExposedPorts {
			cfgCopy.ExposedPorts[k] = v
		}
	}

	if cfgCopy.Labels != nil {
		cfgCopy.Labels = make(map[string]string, len(runConfig.Labels))
		for k, v := range runConfig.Labels {
			cfgCopy.Labels[k] = v
		}
	}

	for _, modifier := range modifiers {
		modifier(&cfgCopy)
	}
	return &cfgCopy
}

func copyStringSlice(orig []string) []string {
	if orig == nil {
		return nil
	}
	return append([]string{}, orig...)
}

// getShell is a helper function which gets the right shell for prefixing the
// shell-form of RUN, ENTRYPOINT and CMD instructions
func getShell(c *container.Config, os string) []string {
	if len(c.Shell) == 0 {
		return append([]string{}, defaultShellForOS(os)[:]...)
	}
	return append([]string{}, c.Shell[:]...)
}

func (b *Builder) probeCache(dispatchState *dispatchState, runConfig *container.Config) (bool, error) {
	cachedID, err := b.imageProber.Probe(dispatchState.imageID, runConfig, b.getPlatform(dispatchState))
	if cachedID == "" || err != nil {
		return false, err
	}
	_, _ = fmt.Fprintln(b.Stdout, " ---> Using cache")

	dispatchState.imageID = cachedID
	return true, nil
}

var defaultLogConfig = container.LogConfig{Type: "none"}

func (b *Builder) probeAndCreate(ctx context.Context, dispatchState *dispatchState, runConfig *container.Config) (string, error) {
	if hit, err := b.probeCache(dispatchState, runConfig); err != nil || hit {
		return "", err
	}
	return b.create(ctx, runConfig)
}

func (b *Builder) create(ctx context.Context, runConfig *container.Config) (string, error) {
	log.G(ctx).Debugf("[BUILDER] Command to be executed: %v", runConfig.Cmd)

	hostConfig := hostConfigFromOptions(b.options)
	ctr, err := b.containerManager.Create(ctx, runConfig, hostConfig)
	if err != nil {
		return "", err
	}
	for _, warning := range ctr.Warnings {
		_, _ = fmt.Fprintf(b.Stdout, " ---> [Warning] %s\n", warning)
	}
	_, _ = fmt.Fprintf(b.Stdout, " ---> Running in %s\n", stringid.TruncateID(ctr.ID))
	return ctr.ID, nil
}

func hostConfigFromOptions(options *build.ImageBuildOptions) *container.HostConfig {
	resources := container.Resources{
		CgroupParent: options.CgroupParent,
		CPUShares:    options.CPUShares,
		CPUPeriod:    options.CPUPeriod,
		CPUQuota:     options.CPUQuota,
		CpusetCpus:   options.CPUSetCPUs,
		CpusetMems:   options.CPUSetMems,
		Memory:       options.Memory,
		MemorySwap:   options.MemorySwap,
		Ulimits:      options.Ulimits,
	}

	// We need to make sure no empty string or "default" NetworkMode is
	// provided to the daemon as it doesn't support them.
	//
	// This is in line with what the ContainerCreate API endpoint does.
	networkMode := options.NetworkMode
	if networkMode == "" || networkMode == network.NetworkDefault {
		networkMode = networkSettings.DefaultNetwork
	}

	hc := &container.HostConfig{
		SecurityOpt: options.SecurityOpt,
		Isolation:   options.Isolation,
		ShmSize:     options.ShmSize,
		Resources:   resources,
		NetworkMode: container.NetworkMode(networkMode),
		// Set a log config to override any default value set on the daemon
		LogConfig:  defaultLogConfig,
		ExtraHosts: options.ExtraHosts,
	}
	return hc
}

func (b *Builder) getPlatform(state *dispatchState) ocispec.Platform {
	// May be nil if not explicitly set in API/dockerfile
	out := platforms.DefaultSpec()
	if b.platform != nil {
		out = *b.platform
	}

	if state.operatingSystem != "" {
		out.OS = state.operatingSystem
	}

	return out
}
