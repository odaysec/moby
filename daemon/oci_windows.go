package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Microsoft/hcsshim"
	coci "github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/log"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/daemon/container"
	"github.com/docker/docker/daemon/internal/image"
	"github.com/docker/docker/daemon/pkg/oci"
	"github.com/docker/docker/daemon/server/backend"
	"github.com/docker/docker/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	credentialSpecRegistryLocation = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Virtualization\Containers\CredentialSpecs`
	credentialSpecFileLocation     = "CredentialSpecs"
)

// setupContainerDirs sets up base container directories (root, ipc, tmpfs and secrets).
func (daemon *Daemon) setupContainerDirs(c *container.Container) ([]container.Mount, error) {
	// Note, unlike Unix, we do NOT call into SetupWorkingDirectory as
	// this is done in VMCompute. Further, we couldn't do it for Hyper-V
	// containers anyway.
	if err := daemon.setupSecretDir(c); err != nil {
		return nil, err
	}

	if err := daemon.setupConfigDir(c); err != nil {
		return nil, err
	}

	// If the container has not been started, and has configs or secrets
	// secrets, create symlinks to each config and secret. If it has been
	// started before, the symlinks should have already been created. Also, it
	// is important to not mount a Hyper-V  container that has been started
	// before, to protect the host from the container; for example, from
	// malicious mutation of NTFS data structures.
	if !c.HasBeenStartedBefore && (len(c.SecretReferences) > 0 || len(c.ConfigReferences) > 0) {
		// The container file system is mounted before this function is called,
		// except for Hyper-V containers, so mount it here in that case.
		if daemon.isHyperV(c) {
			if err := daemon.Mount(c); err != nil {
				return nil, err
			}
			defer daemon.Unmount(c)
		}
		if err := c.CreateSecretSymlinks(); err != nil {
			return nil, err
		}
		if err := c.CreateConfigSymlinks(); err != nil {
			return nil, err
		}
	}

	secretMounts, err := c.SecretMounts()
	if err != nil {
		return nil, err
	}

	var mounts []container.Mount
	if secretMounts != nil {
		mounts = append(mounts, secretMounts...)
	}

	if configMounts := c.ConfigMounts(); configMounts != nil {
		mounts = append(mounts, configMounts...)
	}

	return mounts, nil
}

func (daemon *Daemon) isHyperV(c *container.Container) bool {
	if c.HostConfig.Isolation.IsDefault() {
		// Container using default isolation, so take the default from the daemon configuration
		return daemon.defaultIsolation.IsHyperV()
	}
	// Container may be requesting an explicit isolation mode.
	return c.HostConfig.Isolation.IsHyperV()
}

func (daemon *Daemon) createSpec(ctx context.Context, daemonCfg *configStore, c *container.Container, mounts []container.Mount) (*specs.Spec, error) {
	img, err := daemon.imageService.GetImage(ctx, string(c.ImageID), backend.GetImageOpts{})
	if err != nil {
		return nil, err
	}
	if err := image.CheckOS(img.OperatingSystem()); err != nil {
		return nil, err
	}

	s := oci.DefaultSpec()

	if err := coci.WithAnnotations(c.HostConfig.Annotations)(ctx, nil, nil, &s); err != nil {
		return nil, err
	}

	for _, mount := range mounts {
		m := specs.Mount{
			Source:      mount.Source,
			Destination: mount.Destination,
		}
		if !mount.Writable {
			m.Options = append(m.Options, "ro")
		}
		s.Mounts = append(s.Mounts, m)
	}

	linkedEnv, err := daemon.setupLinkedContainers(c)
	if err != nil {
		return nil, err
	}

	isHyperV := daemon.isHyperV(c)
	if isHyperV {
		s.Windows.HyperV = &specs.WindowsHyperV{}
	}

	// In s.Process
	s.Process.Cwd = c.Config.WorkingDir
	s.Process.Env = c.CreateDaemonEnvironment(c.Config.Tty, linkedEnv)
	s.Process.Terminal = c.Config.Tty

	if c.Config.Tty {
		s.Process.ConsoleSize = &specs.Box{
			Height: c.HostConfig.ConsoleSize[0],
			Width:  c.HostConfig.ConsoleSize[1],
		}
	}
	s.Process.User.Username = c.Config.User
	s.Windows.LayerFolders, err = daemon.imageService.GetLayerFolders(img, c.RWLayer, c.ID)
	if err != nil {
		return nil, errors.Wrapf(err, "GetLayerFolders failed: container %s", c.ID)
	}

	// Get endpoints for the libnetwork allocated networks to the container
	var epList []string
	AllowUnqualifiedDNSQuery := false
	gwHNSID := ""
	if c.NetworkSettings != nil {
		for n := range c.NetworkSettings.Networks {
			sn, err := daemon.FindNetwork(n)
			if err != nil {
				continue
			}

			ep, err := getEndpointInNetwork(c.Name, sn)
			if err != nil {
				continue
			}

			data, err := ep.DriverInfo()
			if err != nil {
				continue
			}

			if data["GW_INFO"] != nil {
				gwInfo := data["GW_INFO"].(map[string]interface{})
				if gwInfo["hnsid"] != nil {
					gwHNSID = gwInfo["hnsid"].(string)
				}
			}

			if data["hnsid"] != nil {
				epList = append(epList, data["hnsid"].(string))
			}

			if data["AllowUnqualifiedDNSQuery"] != nil {
				AllowUnqualifiedDNSQuery = true
			}
		}
	}

	var networkSharedContainerID string
	if c.HostConfig.NetworkMode.IsContainer() {
		networkSharedContainerID = c.NetworkSharedContainerID
		for _, ep := range c.SharedEndpointList {
			epList = append(epList, ep)
		}
	}

	if gwHNSID != "" {
		epList = append(epList, gwHNSID)
	}

	var dnsSearch []string
	if len(c.HostConfig.DNSSearch) > 0 {
		dnsSearch = c.HostConfig.DNSSearch
	} else if len(daemonCfg.DNSSearch) > 0 {
		dnsSearch = daemonCfg.DNSSearch
	}

	s.Windows.Network = &specs.WindowsNetwork{
		AllowUnqualifiedDNSQuery:   AllowUnqualifiedDNSQuery,
		DNSSearchList:              dnsSearch,
		EndpointList:               epList,
		NetworkSharedContainerName: networkSharedContainerID,
	}

	if err := daemon.createSpecWindowsFields(c, &s, isHyperV); err != nil {
		return nil, err
	}

	if log.G(ctx).Level >= log.DebugLevel {
		if b, err := json.Marshal(&s); err == nil {
			log.G(ctx).Debugf("Generated spec: %s", string(b))
		}
	}

	return &s, nil
}

// Sets the Windows-specific fields of the OCI spec
func (daemon *Daemon) createSpecWindowsFields(c *container.Container, s *specs.Spec, isHyperV bool) error {
	s.Hostname = c.FullHostname()

	if len(s.Process.Cwd) == 0 {
		// We default to C:\ to workaround the oddity of the case that the
		// default directory for cmd running as LocalSystem (or
		// ContainerAdministrator) is c:\windows\system32. Hence docker run
		// <image> cmd will by default end in c:\windows\system32, rather
		// than 'root' (/) on Linux. The oddity is that if you have a dockerfile
		// which has no WORKDIR and has a COPY file ., . will be interpreted
		// as c:\. Hence, setting it to default of c:\ makes for consistency.
		s.Process.Cwd = `C:\`
	}

	if c.Config.ArgsEscaped {
		s.Process.CommandLine = c.Path
		if len(c.Args) > 0 {
			s.Process.CommandLine += " " + escapeArgs(c.Args)
		}
	} else {
		s.Process.Args = append([]string{c.Path}, c.Args...)
	}
	s.Root.Readonly = false // Windows does not support a read-only root filesystem
	if !isHyperV {
		if c.BaseFS == "" {
			return errors.New("createSpecWindowsFields: BaseFS of container " + c.ID + " is unexpectedly empty")
		}

		if daemon.UsesSnapshotter() {
			// daemon.Mount() for the snapshotters actually mounts the filesystem to the host
			// using containerd/mount.All and BaseFS is the directory where this is mounted.
			// This is consistent with Linux-based graphdriver implementations.
			// For the windowsfilter graphdriver, the underlying Get() call does not actually mount
			// the filesystem to a path, and BaseFS is the Volume GUID of the prepared/activated
			// filesystem.

			// The spec for Root.Path for Windows specifies that for Process-isolated containers,
			// it must be in the Volume GUID (\\?\\Volume{GUID} style), not a host-mounted directory.
			backingDevicePath, err := getBackingDeviceForContainerdMount(c.BaseFS)
			if err != nil {
				return errors.Wrapf(err, "createSpecWindowsFields: Failed to get backing device of BaseFS of container %s", c.ID)
			}
			s.Root.Path = backingDevicePath
		} else {
			s.Root.Path = c.BaseFS // This is not set for Hyper-V containers
		}
		if !strings.HasSuffix(s.Root.Path, `\`) {
			s.Root.Path = s.Root.Path + `\` // Ensure a correctly formatted volume GUID path \\?\Volume{GUID}\
		}
	}

	// First boot optimization
	s.Windows.IgnoreFlushesDuringBoot = !c.HasBeenStartedBefore

	setResourcesInSpec(c, s, isHyperV)

	// Read and add credentials from the security options if a credential spec has been provided.
	if err := daemon.setWindowsCredentialSpec(c, s); err != nil {
		return err
	}

	devices, err := setupWindowsDevices(c.HostConfig.Devices)
	if err != nil {
		return err
	}

	s.Windows.Devices = append(s.Windows.Devices, devices...)

	return nil
}

// escapeArgs makes a Windows-style escaped command line from a set of arguments
func escapeArgs(args []string) string {
	escapedArgs := make([]string, len(args))
	for i, a := range args {
		escapedArgs[i] = windows.EscapeArg(a)
	}
	return strings.Join(escapedArgs, " ")
}

// getBackingDeviceForContainerdMount extracts the backing device or directory mounted at mountPoint
// by containerd's mount.Mount implementation for Windows.
func getBackingDeviceForContainerdMount(mountPoint string) (string, error) {
	// NOTE: This relies on details of the behaviour of containerd's mount implementation for Windows,
	// and so is somewhat fragile.
	// TODO: Upstream this into the mount package.
	// The implementation would be the same, but it'll be better-encapsulated.

	// See containerd/containerd/mount/mount_windows.go
	// This is mostly just copied from mount.Unmount

	const sourceStreamName = "containerd.io-source"

	mountPoint = filepath.Clean(mountPoint)
	adsFile := mountPoint + ":" + sourceStreamName
	var layerPath string

	if _, err := os.Lstat(adsFile); err == nil {
		layerPathb, err := os.ReadFile(mountPoint + ":" + sourceStreamName)
		if err != nil {
			return "", fmt.Errorf("failed to retrieve layer source for mount %s: %w", mountPoint, err)
		}
		layerPath = string(layerPathb)
	}

	if layerPath == "" {
		return "", fmt.Errorf("no layer source for mount %s", mountPoint)
	}

	home, layerID := filepath.Split(layerPath)
	di := hcsshim.DriverInfo{
		HomeDir: home,
	}

	backingDevice, err := hcsshim.GetLayerMountPath(di, layerID)
	if err != nil {
		return "", fmt.Errorf("failed to retrieve backing device for layer %s: %w", mountPoint, err)
	}

	return backingDevice, nil
}

var errInvalidCredentialSpecSecOpt = errdefs.InvalidParameter(fmt.Errorf("invalid credential spec security option - value must be prefixed by 'file://', 'registry://', or 'raw://' followed by a non-empty value"))

// setWindowsCredentialSpec sets the spec's `Windows.CredentialSpec`
// field if relevant
func (daemon *Daemon) setWindowsCredentialSpec(c *container.Container, s *specs.Spec) error {
	if c.HostConfig == nil || c.HostConfig.SecurityOpt == nil {
		return nil
	}

	// TODO (jrouge/wk8): if provided with several security options, we silently ignore
	// all but the last one (provided they're all valid, otherwise we do return an error);
	// this doesn't seem like a great idea?
	credentialSpec := ""

	// TODO(thaJeztah): extract validating and parsing SecurityOpt to a reusable function.
	for _, secOpt := range c.HostConfig.SecurityOpt {
		k, v, ok := strings.Cut(secOpt, "=")
		if !ok {
			return errdefs.InvalidParameter(fmt.Errorf("invalid security option: no equals sign in supplied value %s", secOpt))
		}
		// FIXME(thaJeztah): options should not be case-insensitive
		if !strings.EqualFold(k, "credentialspec") {
			return errdefs.InvalidParameter(fmt.Errorf("security option not supported: %s", k))
		}

		scheme, value, ok := strings.Cut(v, "://")
		if !ok || value == "" {
			return errInvalidCredentialSpecSecOpt
		}
		var err error
		switch strings.ToLower(scheme) {
		case "file":
			credentialSpec, err = readCredentialSpecFile(c.ID, daemon.root, filepath.Clean(value))
			if err != nil {
				return errdefs.InvalidParameter(err)
			}
		case "registry":
			credentialSpec, err = readCredentialSpecRegistry(c.ID, value)
			if err != nil {
				return errdefs.InvalidParameter(err)
			}
		case "config":
			// if the container does not have a DependencyStore, then it
			// isn't swarmkit managed. In order to avoid creating any
			// impression that `config://` is a valid API, return the same
			// error as if you'd passed any other random word.
			if c.DependencyStore == nil {
				return errInvalidCredentialSpecSecOpt
			}

			csConfig, err := c.DependencyStore.Configs().Get(value)
			if err != nil {
				return errdefs.System(errors.Wrap(err, "error getting value from config store"))
			}
			// stuff the resulting secret data into a string to use as the
			// CredentialSpec
			credentialSpec = string(csConfig.Spec.Data)
		case "raw":
			credentialSpec = value
		default:
			return errInvalidCredentialSpecSecOpt
		}
	}

	if credentialSpec != "" {
		if s.Windows == nil {
			s.Windows = &specs.Windows{}
		}
		s.Windows.CredentialSpec = credentialSpec
	}

	return nil
}

func setResourcesInSpec(c *container.Container, s *specs.Spec, isHyperV bool) {
	// In s.Windows.Resources
	cpuShares := uint16(c.HostConfig.CPUShares)
	cpuMaximum := uint16(c.HostConfig.CPUPercent) * 100
	cpuCount := uint64(c.HostConfig.CPUCount)
	if c.HostConfig.NanoCPUs > 0 {
		if isHyperV {
			cpuCount = uint64(c.HostConfig.NanoCPUs / 1e9)
			leftoverNanoCPUs := c.HostConfig.NanoCPUs % 1e9
			if leftoverNanoCPUs != 0 {
				cpuCount++
				cpuMaximum = uint16(c.HostConfig.NanoCPUs / int64(cpuCount) / (1e9 / 10000))
				if cpuMaximum < 1 {
					// The requested NanoCPUs is so small that we rounded to 0, use 1 instead
					cpuMaximum = 1
				}
			}
		} else {
			cpuMaximum = uint16(c.HostConfig.NanoCPUs / int64(runtime.NumCPU()) / (1e9 / 10000))
			if cpuMaximum < 1 {
				// The requested NanoCPUs is so small that we rounded to 0, use 1 instead
				cpuMaximum = 1
			}
		}
	}

	if cpuMaximum != 0 || cpuShares != 0 || cpuCount != 0 {
		if s.Windows.Resources == nil {
			s.Windows.Resources = &specs.WindowsResources{}
		}
		s.Windows.Resources.CPU = &specs.WindowsCPUResources{
			Maximum: &cpuMaximum,
			Shares:  &cpuShares,
			Count:   &cpuCount,
		}
	}

	memoryLimit := uint64(c.HostConfig.Memory)
	if memoryLimit != 0 {
		if s.Windows.Resources == nil {
			s.Windows.Resources = &specs.WindowsResources{}
		}
		s.Windows.Resources.Memory = &specs.WindowsMemoryResources{
			Limit: &memoryLimit,
		}
	}

	if c.HostConfig.IOMaximumBandwidth != 0 || c.HostConfig.IOMaximumIOps != 0 {
		if s.Windows.Resources == nil {
			s.Windows.Resources = &specs.WindowsResources{}
		}
		s.Windows.Resources.Storage = &specs.WindowsStorageResources{
			Bps:  &c.HostConfig.IOMaximumBandwidth,
			Iops: &c.HostConfig.IOMaximumIOps,
		}
	}
}

// mergeUlimits merge the Ulimits from HostConfig with daemon defaults, and update HostConfig
// It will do nothing on non-Linux platform
func (daemon *Daemon) mergeUlimits(c *containertypes.HostConfig, daemonCfg *config.Config) {
	return
}

// registryKey is an interface wrapper around `registry.Key`,
// listing only the methods we care about here.
// It's mainly useful to easily allow mocking the registry in tests.
type registryKey interface {
	GetStringValue(name string) (val string, valType uint32, err error)
	Close() error
}

var registryOpenKeyFunc = func(baseKey registry.Key, path string, access uint32) (registryKey, error) {
	return registry.OpenKey(baseKey, path, access)
}

// readCredentialSpecRegistry is a helper function to read a credential spec from
// the registry. If not found, we return an empty string and warn in the log.
// This allows for staging on machines which do not have the necessary components.
func readCredentialSpecRegistry(id, name string) (string, error) {
	key, err := registryOpenKeyFunc(registry.LOCAL_MACHINE, credentialSpecRegistryLocation, registry.QUERY_VALUE)
	if err != nil {
		return "", errors.Wrapf(err, "failed handling spec %q for container %s - registry key %s could not be opened", name, id, credentialSpecRegistryLocation)
	}
	defer key.Close()

	value, _, err := key.GetStringValue(name)
	if err != nil {
		if err == registry.ErrNotExist {
			return "", fmt.Errorf("registry credential spec %q for container %s was not found", name, id)
		}
		return "", errors.Wrapf(err, "error reading credential spec %q from registry for container %s", name, id)
	}

	return value, nil
}

// readCredentialSpecFile is a helper function to read a credential spec from
// a file. If not found, we return an empty string and warn in the log.
// This allows for staging on machines which do not have the necessary components.
func readCredentialSpecFile(id, root, location string) (string, error) {
	if filepath.IsAbs(location) {
		return "", fmt.Errorf("invalid credential spec: file:// path cannot be absolute")
	}
	base := filepath.Join(root, credentialSpecFileLocation)
	full := filepath.Join(base, location)
	if !strings.HasPrefix(full, base) {
		return "", fmt.Errorf("invalid credential spec: file:// path must be under %s", base)
	}
	bcontents, err := os.ReadFile(full)
	if err != nil {
		return "", errors.Wrapf(err, "failed to load credential spec for container %s", id)
	}
	return string(bcontents[:]), nil
}

func setupWindowsDevices(devices []containertypes.DeviceMapping) ([]specs.WindowsDevice, error) {
	var specDevices []specs.WindowsDevice
	for _, deviceMapping := range devices {
		if strings.HasPrefix(deviceMapping.PathOnHost, "class/") {
			specDevices = append(specDevices, specs.WindowsDevice{
				ID:     strings.TrimPrefix(deviceMapping.PathOnHost, "class/"),
				IDType: "class",
			})
		} else {
			idType, id, ok := strings.Cut(deviceMapping.PathOnHost, "://")
			if !ok {
				return nil, errors.Errorf("invalid device assignment path: '%s', must be 'class/ID' or 'IDType://ID'", deviceMapping.PathOnHost)
			}
			if idType == "" {
				return nil, errors.Errorf("invalid device assignment path: '%s', IDType cannot be empty", deviceMapping.PathOnHost)
			}
			specDevices = append(specDevices, specs.WindowsDevice{
				ID:     id,
				IDType: idType,
			})
		}
	}

	return specDevices, nil
}

// getUser is a no-op on Windows.
func getUser(c *container.Container, username string) (specs.User, error) {
	return specs.User{}, nil
}
