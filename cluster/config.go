package cluster

import (
	"fmt"
	"slices"

	"go.yaml.in/yaml/v4"

	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/config/container"
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/machine"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block"
	v1alpha1 "github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	blockres "github.com/siderolabs/talos/pkg/machinery/resources/block"
)

// ConfigInput is everything the generated machine config depends on that this
// package cannot know for itself.
//
// The two serials are INPUTS rather than constants because they are set on the
// QEMU devices by package main, which cannot be imported from here. Copying the
// literals across the boundary would compile, read correctly, and drift the
// first time either side is renamed — after which the install selector matches
// no disk and Talos installs nowhere, with nothing pointing at the cause.
type ConfigInput struct {
	ClusterName string
	// Endpoint is the Kubernetes API endpoint, e.g. https://127.0.0.1:6443.
	Endpoint string
	// TalosVersion is the version read off the ISO. It may be "" — an image
	// nobody could classify still has to boot.
	TalosVersion string
	// ConsoleArg is the console kernel argument for this platform, from
	// platform.Detect().
	ConsoleArg string
	// SystemDiskSerial is the serial of the install target.
	SystemDiskSerial string
	// DataDiskSerial is the serial of the PVC disk. Empty means there is no
	// data disk, and then no user volume is emitted at all.
	DataDiskSerial string
}

// Generated holds the three artifacts bring-up needs. All three contain
// secrets; none of them is safe to log.
type Generated struct {
	ControlPlane []byte
	Talosconfig  []byte
	Secrets      []byte
}

// userVolumeName fixes the mount point at /var/mnt/local-path-provisioner,
// which is the root path local-path-provisioner is patched to use. Talos's root
// filesystem is read-only, so the manifest's stock /opt path cannot work; the
// two must agree, and this constant is the agreement.
const userVolumeName = "local-path-provisioner"

// loopback is where the QEMU port forwards land. Both the Talos API and the
// Kubernetes API are reached through them, so it is a property of the forward
// rather than of ConfigInput.Endpoint.
const loopback = "127.0.0.1"

// GenerateConfig produces the machine config, client config and secrets bundle
// for a single-node cluster.
//
// It is bootstrap only: the output describes a machine that does not exist yet.
// Nothing here reconciles a running one.
func GenerateConfig(in ConfigInput) (*Generated, error) {
	version := in.TalosVersion

	checked, err := CheckVersion(version)
	if err != nil {
		return nil, err
	}

	if !checked {
		// The version is unknown, which by design disables the guard rather
		// than blocking the image. Generation still needs a version for the
		// contract and the installer tag, and ours is the only one we have:
		// it is what machinery would default to anyway, but chosen out loud.
		version = GeneratorVersion()
	}

	contract, err := config.ParseContractFromVersion(version)
	if err != nil {
		return nil, fmt.Errorf("parsing Talos version %q: %w", version, err)
	}

	input, err := generate.NewInput(in.ClusterName, in.Endpoint, constants.DefaultKubernetesVersion,
		// Without a contract every version-gated default is generated for the
		// machinery's own version instead of the image's.
		generate.WithVersionContract(contract),
		// Pinned to the IMAGE. Left unset, Talos substitutes the generator's
		// version and a fresh install silently becomes a cross-version upgrade.
		generate.WithInstallImage("ghcr.io/siderolabs/installer:"+version),
		// The installed system writes its own kernel cmdline and inherits
		// nothing from the ISO, so without this it boots with no serial console.
		generate.WithInstallExtraKernelArgs([]string{in.ConsoleArg}),
		// A topology correction, not a security weakening: with the
		// control-plane taint in place a single-node cluster schedules nothing.
		generate.WithAllowSchedulingOnControlPlanes(true),
		// apid serves on the guest but is dialled at the forwarded loopback
		// address, which must therefore be in its certificate.
		generate.WithAdditionalSubjectAltNames([]string{loopback}),
		generate.WithEndpointList([]string{loopback}),
	)
	if err != nil {
		return nil, fmt.Errorf("preparing config generation: %w", err)
	}

	cfg, err := input.Config(machine.TypeControlPlane)
	if err != nil {
		return nil, fmt.Errorf("generating control plane config: %w", err)
	}

	// The install disk is selected by SERIAL, and there is no generate.Option
	// that reaches the selector — WithInstallDisk takes a device path, which is
	// exactly the identity we are avoiding. PatchV1Alpha1 is machinery's own
	// supported way in, and it preserves the other documents.
	cfg, err = cfg.PatchV1Alpha1(func(c *v1alpha1.Config) error {
		c.MachineConfig.MachineInstall.InstallDiskSelector = &v1alpha1.InstallDiskSelector{
			Serial: in.SystemDiskSerial,
		}

		// A 1.12+ contract turns grubUseUKICmdline ON, which makes GRUB take
		// its cmdline from the installer's UKI and IGNORE extraKernelArgs —
		// machinery rejects the two together, so a config carrying a console
		// arg does not even validate in metal mode. Talos 1.8 dropped
		// console=ttyS0 from the metal image's own defaults (imager/quirks),
		// so the arg has to come from here and the UKI cmdline has to yield.
		// Only touched when machinery set it: the field is unknown to older
		// Talos, and the contract exists to avoid emitting fields a node
		// cannot parse.
		if c.MachineConfig.MachineInstall.InstallGrubUseUKICmdline != nil {
			c.MachineConfig.MachineInstall.InstallGrubUseUKICmdline = new(false)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("patching the install section: %w", err)
	}

	if in.DataDiskSerial != "" {
		volume, err := userVolume(in.DataDiskSerial)
		if err != nil {
			return nil, err
		}

		// A UserVolumeConfig is a document of its own, not part of v1alpha1, so
		// it is appended to the container rather than patched in.
		cfg, err = container.New(append(slices.Clone(cfg.Documents()), volume)...)
		if err != nil {
			return nil, fmt.Errorf("adding the user volume: %w", err)
		}
	}

	controlPlane, err := cfg.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encoding control plane config: %w", err)
	}

	clientConfig, err := input.Talosconfig()
	if err != nil {
		return nil, fmt.Errorf("generating talosconfig: %w", err)
	}

	talosconfig, err := clientConfig.Bytes()
	if err != nil {
		return nil, fmt.Errorf("encoding talosconfig: %w", err)
	}

	// Marshalled with machinery's own yaml package so secrets.LoadBundle reads
	// back exactly what we wrote, byte for byte as talosctl writes it.
	secretsBundle, err := yaml.Marshal(input.Options.SecretsBundle)
	if err != nil {
		return nil, fmt.Errorf("encoding secrets bundle: %w", err)
	}

	return &Generated{
		ControlPlane: controlPlane,
		Talosconfig:  talosconfig,
		Secrets:      secretsBundle,
	}, nil
}

// userVolume describes the PVC volume on the data disk.
//
// It is a partition rather than a whole-disk volume, and it grows to fill the
// disk: the disk is dedicated, but a partitioned volume is the path Talos's own
// storage guide documents, and `grow` makes the distinction moot in practice.
func userVolume(serial string) (*block.UserVolumeConfigV1Alpha1, error) {
	// Selected by serial for the same reason the install disk is. `!system_disk`
	// is not enough: the boot ISO is a virtio-blk device too.
	match, err := cel.ParseBooleanExpression(fmt.Sprintf("disk.serial == %q", serial), celenv.DiskLocator())
	if err != nil {
		return nil, fmt.Errorf("building a disk selector for serial %q: %w", serial, err)
	}

	volume := block.NewUserVolumeConfigV1Alpha1()
	volume.MetaName = userVolumeName
	volume.VolumeType = new(blockres.VolumeTypePartition)
	volume.ProvisioningSpec = block.ProvisioningSpec{
		DiskSelectorSpec:    block.DiskSelector{Match: match},
		ProvisioningMinSize: block.MustByteSize("1GiB"),
		ProvisioningGrow:    new(true),
	}
	volume.FilesystemSpec = block.FilesystemSpec{FilesystemType: blockres.FilesystemTypeXFS}

	return volume, nil
}
