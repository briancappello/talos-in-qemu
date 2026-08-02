package cluster

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/compatibility"
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
	// APIAddress is the address a CLIENT DIALS to reach this machine, with no
	// port — it becomes both the apid certificate's subject alt name and the
	// talosconfig's endpoint.
	//
	// It is DERIVED from the Talos endpoint by the caller (see up.go's
	// apiAddress) rather than configured beside it. The certificate must name
	// what the client dials, and the endpoint IS what the client dials; two
	// independent fields could be set to disagree, and the failure is a TLS
	// handshake error that says nothing about the config that caused it.
	//
	// Under QEMU this is the loopback host side of a port forward. On hardware
	// it is the node's own address. The generated config is identical either
	// way, which is the whole point.
	APIAddress string
	// TalosVersion is the version read off the ISO. It is REQUIRED:
	// InspectImageVersion renders an unclassifiable image as "", and
	// GenerateConfig refuses that rather than substituting its own version,
	// because this value becomes the installer image tag on the node's disk.
	TalosVersion string
	// ConsoleArg is the console kernel argument for this platform, from
	// platform.Detect().
	ConsoleArg string
	// SystemDiskSerial is the serial of the install target.
	SystemDiskSerial string
	// DataDiskSerial is the serial of the PVC disk. Empty means there is no
	// data disk, and then no user volume is emitted at all.
	DataDiskSerial string
	// DisableKexec asks the node not to kexec when it reboots, via
	// kernel.kexec_load_disabled. It exists for ONE host platform — QEMU on
	// macOS, where the kexec path dies in the guest on arm64 — and the caller
	// decides, because whether the host is affected is a fact about the host
	// and this package does not know one from another.
	//
	// It is a bool rather than a GOOS because the config layer has no business
	// mapping operating systems to workarounds; up.go holds the platform and
	// makes that call.
	DisableKexec bool
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

// errUnknownTalosVersion is the refusal for an image whose Talos version could
// not be determined.
//
// It is ONE function because TWO callers refuse the same condition, and they
// must not drift into two different explanations of it. GenerateConfig refuses
// unconditionally — that is the refusal nothing can bypass. Up refuses at step
// 3, from the ISO alone, BEFORE it boots anything: the outcome is already
// provable there, and reaching this failure through GenerateConfig instead
// costs a booted VM, a state dir and a five-minute maintenance wait for a
// verdict the ISO's volume id gave for free.
func errUnknownTalosVersion() error {
	return fmt.Errorf(`could not determine the Talos version of this image

The installer image is pinned to the IMAGE's own version and cannot be guessed.
Left to default, Talos substitutes the config generator's version (%s), and a
fresh install silently becomes a cross-version upgrade: the maintenance system
already running either rejects the config outright as too new, or accepts it,
installs, and then hangs at /sbin/init with nothing on the console to say why.

  boot a stock Talos ISO, whose volume id encodes the version (e.g. TALOS_V1_13_7)`,
		GeneratorVersion())
}

// GenerateConfig produces the machine config, client config and secrets bundle
// for a single-node cluster.
//
// It is bootstrap only: the output describes a machine that does not exist yet.
// Nothing here reconciles a running one.
func GenerateConfig(in ConfigInput) (*Generated, error) {
	version := in.TalosVersion

	// An unknown version DISABLES the guard (CheckVersion returns false, nil)
	// but must not be generated through. The two are not in tension: the guard
	// asks "may we generate at all", and refuses only images it can prove are
	// too new; the installer tag is a value that gets WRITTEN TO DISK, and
	// there is no safe value to write for an image nobody identified.
	// Substituting GeneratorVersion() here would hand-roll the exact default
	// the pin below exists to override.
	if version == "" {
		return nil, errUnknownTalosVersion()
	}

	// Refused here rather than at the handshake. An empty SAN list produces a
	// certificate that names nothing; the node installs, boots, serves apid,
	// and every authenticated call then fails minutes later with an error
	// about certificates and nothing pointing at this field.
	if in.APIAddress == "" {
		return nil, errors.New("no API address: this is the address a client dials to reach " +
			"the node, and it must be in apid's certificate or no authenticated call can " +
			"ever succeed")
	}

	// checked is deliberately discarded, and there are TWO ways it comes back
	// false — the comment has to cover both, because "ignoring this costs
	// something visible" was the whole reason for the second return value.
	//
	// (1) An unparseable IMAGE version. The only one InspectImageVersion
	// produces is "", refused above; anything else reaches
	// ParseContractFromVersion below, which names it in the error. Covered.
	//
	// (2) An unparseable GENERATOR version (version.go:74-77). Here the image
	// parses fine, the guard silently never runs, and a config is generated
	// with nothing downstream to notice — the one case where discarding
	// `checked` really does lose information. It is unreachable while
	// GeneratorVersion() is machinery's own compile-time version constant,
	// which is why it is documented rather than branched on: the day that
	// becomes a build flag or a runtime lookup, this is the line that has to
	// grow a branch.
	if _, err := CheckVersion(version); err != nil {
		return nil, err
	}

	contract, err := config.ParseContractFromVersion(version)
	if err != nil {
		return nil, fmt.Errorf("parsing Talos version %q: %w", version, err)
	}

	k8sVersion, err := kubernetesVersion(version)
	if err != nil {
		return nil, err
	}

	input, err := generate.NewInput(in.ClusterName, in.Endpoint, k8sVersion,
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
		// apid is dialled at THIS address, which must therefore be in its
		// certificate. Derived from the endpoint by the caller so the two
		// cannot disagree.
		generate.WithAdditionalSubjectAltNames([]string{in.APIAddress}),
		generate.WithEndpointList([]string{in.APIAddress}),
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

		// KEXEC IS DISABLED THROUGH A SYSCTL, and the sysctl is what makes this
		// work at all. Talos applies machine-config sysctls IN MAINTENANCE MODE,
		// so this lands on the ISO's running kernel before the install and
		// therefore before the reboot it needs to change. machined then reports
		// kexec support disabled via sysctl and reboots through firmware.
		//
		// Nothing else reaches that kernel: extraKernelArgs configures the
		// INSTALLED system, which in a failed kexec never boots, and the ISO's
		// own cmdline comes from its GRUB config. This is also exactly what
		// upstream's `talosctl cluster create` does.
		//
		// The value is the string "1" because sysctls is map[string]string.
		if in.DisableKexec {
			if c.MachineConfig.MachineSysctls == nil {
				c.MachineConfig.MachineSysctls = map[string]string{}
			}

			c.MachineConfig.MachineSysctls["kernel.kexec_load_disabled"] = "1"
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
		// Documents() builds its result with make() on every call
		// (container.go:693), so appending to it cannot alias the container's
		// own storage and there is nothing to clone.
		cfg, err = container.New(append(cfg.Documents(), volume)...)
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

// kubernetesVersion is the Kubernetes version to pin into a config generated
// for the given Talos image.
//
// It is NOT constants.DefaultKubernetesVersion. That constant is a property of
// the machinery this binary was built against, and CheckVersion deliberately
// admits any image at or BELOW the generator, so writing it into the config is
// the installer pin's bug one field over: a v1.12 image would be handed
// kubelet, apiserver, scheduler and controller-manager v1.36, which is outside
// Talos 1.12's supported window and fails on the node rather than here.
//
// machinery has no Talos -> Kubernetes MAPPING to ask for. What it has is a
// PREDICATE — compatibility.KubernetesVersion.SupportedWith(*TalosVersion),
// compatibility/kubernetes_version.go — and per-release bounds in
// compatibility/talos1XX; the switch that picks the right bounds for a version
// is unexported, so the bounds cannot be read directly without duplicating
// that table here. The predicate is therefore used as an ORACLE: start at the
// generator's default and step the MINOR down until machinery says yes. No
// version number from that table is copied into this repository, and the
// answer is visible in the generated config as the kubelet image tag.
func kubernetesVersion(talosVersion string) (string, error) {
	target, err := compatibility.ParseTalosVersion(&machineapi.VersionInfo{Tag: talosVersion})
	if err != nil {
		return "", fmt.Errorf("parsing Talos version %q: %w", talosVersion, err)
	}

	major, rest, _ := strings.Cut(constants.DefaultKubernetesVersion, ".")

	minorText, _, _ := strings.Cut(rest, ".")

	minor, err := strconv.Atoi(minorText)
	if err != nil {
		return "", fmt.Errorf("parsing the generator's Kubernetes version %q: %w",
			constants.DefaultKubernetesVersion, err)
	}

	// The generator's default is tried whole, patch included, so an image on
	// the generator's own Talos version gets exactly what talosctl would give
	// it. Only a version we have to step DOWN to loses its patch, and .0 is the
	// one patch every Kubernetes minor release ships.
	candidate := constants.DefaultKubernetesVersion

	var unsupported error

	for ; minor > 0; minor-- {
		k8s, err := compatibility.ParseKubernetesVersion(candidate)
		if err != nil {
			return "", fmt.Errorf("parsing Kubernetes version %q: %w", candidate, err)
		}

		if unsupported = k8s.SupportedWith(target); unsupported == nil {
			return candidate, nil
		}

		candidate = fmt.Sprintf("%s.%d.0", major, minor-1)
	}

	return "", fmt.Errorf(`no Kubernetes version works with a Talos %s image

The kubelet and every control-plane component are pinned BY VERSION in the
generated config, and this build's default (%s) is the config generator's, not
the image's. Walking down from it found nothing machinery accepts: %s

  boot an image this build has compatibility data for, or rebuild tinq against
  a machinery that covers %s`,
		talosVersion, constants.DefaultKubernetesVersion, unsupported, talosVersion)
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
