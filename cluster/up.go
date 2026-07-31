package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/coglative/talos-in-qemu/platform"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
)

// THE OUTPUT IS THE FEATURE. This file is a bring-up sequence, but what it
// exists to produce is a transcript an operator LEARNS TALOS from rather than
// trusts blindly. Every step announces the operation; the four non-obvious ones
// announce the reason, and each of those reasons is a failure this project has
// actually been bitten by:
//
//	install disk by SERIAL — a size matcher is a coin flip once a data disk
//	exists, and losing it installs the OS over the user's PVCs.
//
//	installer pinned to the IMAGE's version — left unset Talos substitutes the
//	config generator's, and a fresh install silently becomes a cross-version
//	upgrade that either gets rejected or hangs at /sbin/init.
//
//	console arg for THIS host — the installed system writes its own kernel
//	cmdline and inherits nothing from the ISO, so serial goes dead at exactly
//	the boot you need to watch, and the argument is arch-specific.
//
//	bootstrap fired while the node is `booting` — waiting for `running`
//	DEADLOCKS: a control-plane node cannot reach `running` until etcd exists,
//	and bootstrap is what creates etcd.
//
// Bring-up is BOOTSTRAP ONLY, like the rest of this package. It creates a
// cluster; it never upgrades, scales or reconciles one.

// Ten steps, and the count is printed in every line. It lives here so the
// transcript cannot claim a total the sequence does not have.
const upSteps = 10

// detailIndent lines continuation text up under the step's own text.
const detailIndent = "                        "

const (
	// maintenanceTimeout covers ISO boot to a serving maintenance API.
	maintenanceTimeout = 5 * time.Minute
	// installTimeout covers install, reboot and the installed system's apid
	// coming back with the cluster PKI. It is the longest wait in a bring-up.
	installTimeout = 10 * time.Minute
	// kubeconfigTimeout covers the apiserver starting far enough to mint an
	// admin kubeconfig, which is not immediate after bootstrap.
	kubeconfigTimeout = 5 * time.Minute
	// nodeReadyTimeout covers kubelet joining, the CNI landing and the node
	// reporting Ready.
	nodeReadyTimeout = 10 * time.Minute
)

// UpOptions is everything a bring-up depends on that this package cannot know
// for itself.
//
// The serials, the endpoints and the image path all come from package main,
// which owns the qemu invocation. Copying any of them into this package would
// compile, read correctly, and drift the first time main.go changed one.
type UpOptions struct {
	// ClusterName names the cluster and the talosconfig context.
	ClusterName string
	// ImagePath is the resolved boot ISO. Step 2 reads its Talos version from
	// the ISO volume id.
	ImagePath string
	// StateDir is the machine's existing state directory. The four artifacts
	// are written into it so -destroy sweeps them with everything else and the
	// secrets do not outlive the cluster.
	StateDir string
	// TalosEndpoint is the HOST side of the qemu forward to apid, host:port.
	TalosEndpoint string
	// KubeEndpoint is the Kubernetes API as seen from the host, a URL.
	KubeEndpoint string
	// SystemDiskSerial is the install target's serial.
	SystemDiskSerial string
	// DataDiskSerial is the PVC disk's serial. Empty means there is no data
	// disk, and then step 6 emits no user volume AND step 10 installs no
	// storage — the two halves cannot disagree because they read this one
	// field.
	DataDiskSerial string

	// Detect resolves this host's facts (platform.Detect). It is a function
	// rather than a value so the probe stays inside the operation that
	// announces it.
	Detect func() (*platform.Platform, error)
	// Boot starts the VM, or adopts one already running, and returns its pid.
	// Owned by package main: this package knows nothing about qemu.
	Boot func() (int, error)

	// Out receives the transcript. nil means os.Stdout.
	Out io.Writer

	// hooks are the operations that need a real VM and a real cluster. nil
	// means the real ones. It is UNEXPORTED on purpose: it is a test seam, not
	// an API, and package main has no business substituting a bring-up.
	hooks *upHooks
}

// upHooks is the seam that makes the transcript testable without booting
// anything. Every entry is one round trip to a node or a cluster; nothing that
// merely formats a line is in here, because that is the part under test.
type upHooks struct {
	detectVersion      func(imagePath string) string
	generateConfig     func(ConfigInput) (*Generated, error)
	waitMaintenance    func(ctx context.Context, endpoint string, timeout time.Duration) error
	applyConfig        func(ctx context.Context, endpoint string, config []byte) error
	waitBootstrapReady func(ctx context.Context, talosconfig []byte, endpoint string, timeout time.Duration) error
	bootstrap          func(ctx context.Context, talosconfig []byte, endpoint string) error
	kubeconfig         func(ctx context.Context, talosconfig []byte, endpoint string) ([]byte, error)
	waitNodeReady      func(ctx context.Context, kubeconfig []byte, timeout time.Duration) error
	installStorage     func(ctx context.Context, kubeconfig []byte) error
}

func realHooks() *upHooks {
	return &upHooks{
		detectVersion:      platform.InspectImageVersion,
		generateConfig:     GenerateConfig,
		waitMaintenance:    WaitMaintenance,
		applyConfig:        applyConfiguration,
		waitBootstrapReady: WaitBootstrapReady,
		bootstrap:          bootstrapEtcd,
		kubeconfig:         fetchKubeconfig,
		waitNodeReady:      WaitNodeReady,
		installStorage:     InstallStorage,
	}
}

// Up turns a Talos ISO and a state directory into a working single-node
// Kubernetes cluster, announcing each of ten steps as it goes.
//
// It is not resumable. A failure part way through leaves a VM and a state dir,
// and recovery is -destroy followed by a retry — which the error says, because
// the obvious alternative (run -up again) waits out a five-minute maintenance
// timeout against a node that has left maintenance mode.
func Up(ctx context.Context, opts UpOptions) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}

	hooks := opts.hooks
	if hooks == nil {
		hooks = realHooks()
	}

	// Both endpoints are checked BEFORE anything is created, for the same
	// reason create() resolves host facts first: failing here costs nothing,
	// and failing later costs a VM nobody asked to keep. A missing forward is
	// otherwise discovered by a wait spending its entire budget on an address
	// that was never there.
	if opts.TalosEndpoint == "" {
		return errors.New("no Talos API endpoint: this is the host side of the qemu forward to " +
			"guest port 50000, so spec.hostForwards needs an entry for it")
	}

	if opts.KubeEndpoint == "" {
		return errors.New("no Kubernetes API endpoint: this is the host side of the qemu forward to " +
			"guest port 6443, so spec.hostForwards needs an entry for it — a kubeconfig pointing " +
			"anywhere else cannot be used from this host")
	}

	p := &printer{w: out}

	// ── 1/10 platform ───────────────────────────────────────────────────────
	host, err := opts.Detect()
	if err != nil {
		return err
	}

	p.step("platform", "%s/%s, %s, %s", runtime.GOOS, host.ImageArch, host.Accel, host.QEMUBinary)

	// ── 2/10 image ──────────────────────────────────────────────────────────
	// InspectImageVersion never errors: an image it cannot classify reads as
	// "", which is a real state and has to be printed as one rather than as an
	// empty version.
	imageVersion := hooks.detectVersion(opts.ImagePath)

	shown := imageVersion
	if shown == "" {
		shown = "UNKNOWN"
	}

	p.step("image", "%s -> %s (ISO volume id)", filepath.Base(opts.ImagePath), shown)

	// ── 3/10 version guard ──────────────────────────────────────────────────
	//
	// `checked` IS NOT DISCARDED, and that is the whole reason CheckVersion
	// returns it. There are three outcomes and only two of them fit in an
	// error: the guard ran and passed, the guard ran and refused, and — the
	// dangerous one — the guard could not run at all. A pre-release volume id
	// such as TALOS_V1_14_0_ALPHA reads as "" from InspectImageVersion, so the
	// guard is silently disabled for exactly the images most likely to break
	// config generation. `_, err :=` here would re-open that hole with nothing
	// visible to show for it.
	checked, err := CheckVersion(imageVersion)

	switch {
	case err != nil:
		p.step("version guard", "REFUSED: image %s is newer than machinery %s", imageVersion, GeneratorVersion())

		return err
	case checked:
		p.step("version guard", "machinery %s >= image %s  ok", GeneratorVersion(), imageVersion)
	default:
		// REFUSED, not a note, and refused HERE rather than four steps later.
		// GenerateConfig rejects an unidentified image unconditionally —
		// there is no branch of it that accepts an empty version, because the
		// installer tag is written to the node's disk and there is no safe
		// value to write. So this arm is already fatal; continuing merely
		// spends a booted VM, a state dir and the five-minute maintenance
		// budget before saying so. Same rule the refusal arm above obeys:
		// failing here costs nothing, failing after the disk exists leaves
		// residue.
		//
		// The lines still explain WHY the version is unknown, because that is
		// the part the shared refusal cannot know: a pre-release volume id
		// such as TALOS_V1_14_0_ALPHA reads as "" from InspectImageVersion,
		// and it is exactly the image an operator is most likely to be
		// holding when this fires.
		p.step("version guard", "REFUSED: this image's Talos version could not be determined")
		p.detail("!! nothing compared this image against machinery %s, and nothing can:", GeneratorVersion())
		p.detail("!! Talos config generation is BACKWARDS compatible only, and exceeding it does")
		p.detail("!! not fail loudly — it emits a plausible config for a Talos that does not exist.")
		p.detail("!! A pre-release volume id (TALOS_V1_14_0_ALPHA) reads as unknown, which is")
		p.detail("!! precisely the image most likely to break generation.")

		return errUnknownTalosVersion()
	}

	// ── 4/10 boot ───────────────────────────────────────────────────────────
	pid, err := opts.Boot()
	if err != nil {
		return err
	}

	p.step("boot", "pid %d, api %s", pid, opts.TalosEndpoint)

	// Everything from here leaves a VM and a state dir behind when it fails.
	fail := func(err error) error {
		return fmt.Errorf("%w\n\nbring-up is not resumable in v1: a failure part way through leaves a "+
			"running VM and a state dir, and re-running -up waits out the maintenance timeout against a "+
			"node that has left maintenance mode.\n\n  tinq -destroy <this file>, then try again", err)
	}

	// ── 5/10 maintenance ────────────────────────────────────────────────────
	// A REAL Talos API call, never a dial: a qemu hostfwd is accepted by the
	// HOST, so a TCP connect succeeds against a guest that never booted.
	started := time.Now()
	if err := hooks.waitMaintenance(ctx, opts.TalosEndpoint, maintenanceTimeout); err != nil {
		return fail(err)
	}

	p.step("maintenance", "reachable after %s", took(started))

	// ── 6/10 config ─────────────────────────────────────────────────────────
	generated, err := hooks.generateConfig(ConfigInput{
		ClusterName:      opts.ClusterName,
		Endpoint:         opts.KubeEndpoint,
		TalosVersion:     imageVersion,
		ConsoleArg:       host.ConsoleArg,
		SystemDiskSerial: opts.SystemDiskSerial,
		DataDiskSerial:   opts.DataDiskSerial,
	})
	if err != nil {
		return fail(err)
	}

	// Written before the config is applied: if the apply fails, the artifacts
	// that explain WHY are already on disk.
	if err := writeArtifacts(opts.StateDir, map[string][]byte{
		"controlplane.yaml": generated.ControlPlane,
		"talosconfig":       generated.Talosconfig,
		"secrets.yaml":      generated.Secrets,
	}); err != nil {
		return fail(err)
	}

	p.step("config", "wrote controlplane.yaml, talosconfig, secrets.yaml")
	p.detail("diskSelector: serial %s", opts.SystemDiskSerial)
	p.detail("  a size matcher is a coin flip once there are two large disks, and losing")
	p.detail("  it installs the OS over your PVCs")
	// imageVersion is non-empty by construction: step 3 refuses an
	// unidentified image and returns, so this line cannot print
	// "installer: ghcr.io/siderolabs/installer: (pinned to YOUR image)" —
	// a claim about a tag that is not there.
	p.detail("installer: ghcr.io/siderolabs/installer:%s (pinned to YOUR image)", imageVersion)
	p.detail("  left unset Talos substitutes THIS binary's version, and a fresh install")
	p.detail("  silently becomes a cross-version upgrade")
	p.detail("extraKernelArgs: %s (this host's serial)", host.ConsoleArg)
	p.detail("  the installed system writes its own cmdline and inherits nothing from the")
	p.detail("  ISO, so serial goes dead at exactly the boot you need to watch")

	if opts.DataDiskSerial != "" {
		p.detail("userVolume: %s on serial %s", userVolumeName, opts.DataDiskSerial)
		p.detail("  PVCs get their own disk, so a runaway one cannot wedge etcd on EPHEMERAL")
	}

	// ── 7/10 apply-config ───────────────────────────────────────────────────
	started = time.Now()
	if err := hooks.applyConfig(ctx, opts.TalosEndpoint, generated.ControlPlane); err != nil {
		return fail(err)
	}

	// The gate is the AUTHENTICATED API answering, and it is named
	// WaitBootstrapReady because that is what it is for: maintenance mode
	// cannot satisfy the cluster PKI, so success here proves the config landed,
	// the installed system booted, and apid is serving.
	if err := hooks.waitBootstrapReady(ctx, generated.Talosconfig, opts.TalosEndpoint, installTimeout); err != nil {
		return fail(err)
	}

	p.step("apply-config", "installing... rebooting... api back after %s", took(started))

	// ── 8/10 bootstrap ──────────────────────────────────────────────────────
	if err := hooks.bootstrap(ctx, generated.Talosconfig, opts.TalosEndpoint); err != nil {
		return fail(err)
	}

	p.step("bootstrap", "etcd bootstrapped")
	p.detail("fired while the node is 'booting', NOT 'running' — waiting for 'running'")
	p.detail("deadlocks: a control-plane node cannot reach running until etcd exists,")
	p.detail("and bootstrap is the call that creates etcd")

	// ── 9/10 kubeconfig ─────────────────────────────────────────────────────
	started = time.Now()

	kubeconfig, err := hooks.kubeconfig(ctx, generated.Talosconfig, opts.TalosEndpoint)
	if err != nil {
		return fail(err)
	}

	if err := writeArtifacts(opts.StateDir, map[string][]byte{"kubeconfig": kubeconfig}); err != nil {
		return fail(err)
	}

	// The KUBERNETES API, not the Talos one: the Talos API answers long before
	// kubelet has joined, so nothing on that side can report this.
	if err := hooks.waitNodeReady(ctx, kubeconfig, nodeReadyTimeout); err != nil {
		return fail(err)
	}

	p.step("kubeconfig", "wrote kubeconfig, node Ready after %s", took(started))

	// ── 10/10 storage ───────────────────────────────────────────────────────
	//
	// Gated on the SAME field as the user volume in step 6, so the two halves
	// of storage cannot disagree. The skip is ANNOUNCED because the way a data
	// disk goes missing is a typo: `dataDisk: 40` omits the unit, decodes as a
	// number, reads as "not set" and produces no disk and no error. Silence
	// here means the first sign of it is a Pending PVC an hour later.
	if opts.DataDiskSerial == "" {
		p.step("storage", "skipped (spec.dataDisk not set)")
		p.detail("no data disk means no user volume and no StorageClass, so a PVC with no")
		p.detail("storageClassName stays Pending forever. If you meant to have one, check")
		p.detail("the unit: `dataDisk: 40` is not a size and reads as unset, `dataDisk: 40Gi` is.")
	} else {
		if err := hooks.installStorage(ctx, kubeconfig); err != nil {
			return fail(err)
		}

		p.step("storage", "local-path-provisioner %s, default StorageClass", LocalPathVersion)
		p.detail("root %s", mountPath)
		p.detail("  Talos's root filesystem is read-only, so upstream's /opt path cannot work")
		p.detail("namespace local-path-storage labelled privileged")
	}

	p.summary(opts.StateDir, opts.DataDiskSerial != "")

	return nil
}

// printer owns every line of the transcript and, with it, the step numbering.
//
// The number is COUNTED rather than written at each call site: a hand-numbered
// sequence lets a step be inserted, removed or reordered while the transcript
// keeps claiming the old order, which is precisely the lie an operator would
// then debug against.
type printer struct {
	w io.Writer
	n int
}

func (p *printer) step(label, format string, a ...any) {
	p.n++

	fmt.Fprintf(p.w, "[%2d/%d] %-13s %s\n", p.n, upSteps, label, fmt.Sprintf(format, a...))
}

func (p *printer) detail(format string, a ...any) {
	fmt.Fprintf(p.w, "%s%s\n", detailIndent, fmt.Sprintf(format, a...))
}

func (p *printer) line(format string, a ...any) {
	fmt.Fprintf(p.w, format+"\n", a...)
}

// summary prints the two export lines the operator needs and the three
// hardened defaults a freshly bootstrapped Talos cluster has that `kind` does
// not. Each of the three is a deliberate decision, and each is the kind of
// thing that otherwise presents as "Kubernetes is broken".
func (p *printer) summary(stateDir string, storage bool) {
	p.line("")
	p.line("  export TALOSCONFIG=%s", filepath.Join(stateDir, "talosconfig"))
	p.line("  export KUBECONFIG=%s", filepath.Join(stateDir, "kubeconfig"))
	p.line("  kubectl get nodes")
	p.line("")
	p.line("notes — three defaults that differ from a kind cluster, each decided deliberately:")
	p.line("")
	p.line("  control-plane taint  REMOVED (allowSchedulingOnControlPlanes: true). Not a security")
	p.line("                       weakening but a topology correction: in production there would")
	p.line("                       be worker nodes, and on a single node the taint means nothing")
	p.line("                       can ever schedule.")
	p.line("")
	p.line("  PodSecurity          STILL ENFORCED at baseline, which is what a real cluster does")
	p.line("                       and what kind does not. A workload needing more is rejected")
	p.line("                       until you say so per namespace:")
	p.line("                         kubectl label namespace <ns> \\")
	p.line("                           pod-security.kubernetes.io/enforce=privileged")
	p.line("")

	if storage {
		p.line("  storage              local-path-provisioner %s is the default StorageClass, so a", LocalPathVersion)
		p.line("                       PVC with no storageClassName binds. Its data lives on the")
		p.line("                       data disk inside this VM and does not survive -destroy.")
	} else {
		p.line("  storage              no StorageClass is installed, because spec.dataDisk is not")
		p.line("                       set. A PVC with no storageClassName stays Pending. Set")
		p.line("                       spec.dataDisk (with a unit) and -destroy/-up again — and")
		p.line("                       note that PVC data does not survive -destroy either way.")
	}

	p.line("")
}

// took reports how long a step ran, rounded to the second. A bring-up's waits
// are measured in tens of seconds against an installing node; sub-second
// precision here would be noise in the one place the transcript is read.
func took(started time.Time) time.Duration { return time.Since(started).Round(time.Second) }

// writeArtifacts writes generated material into the machine's state directory
// at 0600.
//
// Each file is REMOVED FIRST, and that is what makes the mode reliable.
// os.WriteFile's perm argument applies only when it creates the file, so an
// artifact already sitting there at 0644 — from an earlier build, or anything
// else — would keep its mode and leave a private key world-readable with
// nothing reporting it. Chmod'ing afterwards closes that but opens a window
// where the key is on disk readable by everyone; removing first has neither
// problem and needs no second syscall to be correct.
func writeArtifacts(dir string, artifacts map[string][]byte) error {
	for name, data := range artifacts {
		path := filepath.Join(dir, name)

		// The failure is deliberately NOT inspected: everything that can stop
		// this remove — EACCES on the directory, a non-empty directory in the
		// way — stops the write below too, and reports itself there with the
		// operation the caller actually cares about. A branch here would be a
		// guard no test can reach without the write reaching it first.
		_ = os.Remove(path)

		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}

	return nil
}

// applyConfiguration sends the machine config to a node in MAINTENANCE mode.
//
// Mode REBOOT states what maintenance mode does anyway — it installs and comes
// back as the installed system — rather than leaving it to AUTO, whose answer
// depends on what the node decides the config changed.
//
// The config is SECRET (five certificate authorities and the machine token) and
// is never logged; the node's own error is about fields and endpoints and is
// wrapped normally.
func applyConfiguration(ctx context.Context, endpoint string, config []byte) error {
	c, err := MaintenanceClient(ctx, endpoint)
	if err != nil {
		return err
	}

	defer c.Close() //nolint:errcheck

	if _, err := c.ApplyConfiguration(ctx, &machineapi.ApplyConfigurationRequest{
		Data: config,
		Mode: machineapi.ApplyConfigurationRequest_REBOOT,
	}); err != nil {
		return fmt.Errorf("applying the machine config: %w", err)
	}

	return nil
}

// bootstrapEtcd issues the one call that creates the cluster's etcd.
//
// It is fired while the node is still `booting`; see WaitBootstrapReady for why
// waiting for `running` first is a deadlock rather than a slow path.
//
// talosconfig is SECRET and never reaches a log or an error.
func bootstrapEtcd(ctx context.Context, talosconfig []byte, endpoint string) error {
	c, err := AuthenticatedClient(ctx, talosconfig, endpoint)
	if err != nil {
		return err
	}

	defer c.Close() //nolint:errcheck

	if err := c.Bootstrap(ctx, &machineapi.BootstrapRequest{}); err != nil {
		return fmt.Errorf("bootstrapping etcd: %w", err)
	}

	return nil
}

// fetchKubeconfig asks the node for an admin kubeconfig.
//
// It RETRIES, through the same waitFor every other probe in this package uses.
// The Talos API answers immediately after bootstrap while the apiserver behind
// this call does not, so a single attempt fails on timing alone — and a
// bring-up that dies one step from the end because it asked half a second early
// is the least defensible failure in the sequence.
//
// Both the talosconfig and the answer are SECRET; neither is logged.
func fetchKubeconfig(ctx context.Context, talosconfig []byte, endpoint string) ([]byte, error) {
	c, err := AuthenticatedClient(ctx, talosconfig, endpoint)
	if err != nil {
		return nil, err
	}

	defer c.Close() //nolint:errcheck

	var kubeconfig []byte

	if err := waitFor(ctx, kubeconfigTimeout, "an admin kubeconfig from "+endpoint,
		func(ctx context.Context) error {
			var err error
			kubeconfig, err = c.Kubeconfig(ctx)

			return err
		}); err != nil {
		return nil, err
	}

	return kubeconfig, nil
}
