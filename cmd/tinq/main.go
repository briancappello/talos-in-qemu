// tinq — reconciles TalosMachine into a QEMU virtual machine.
//
// It runs HOST-RESIDENT, not as a pod, because a hardware accelerator is a
// KERNEL API on the machine the VM runs on with no network endpoint: HVF on
// macOS, /dev/kvm on Linux. A controller inside the cluster cannot reach either.
// That is the same shape Sidero's own omni-infra-provider-libvirt uses (a binary
// beside the hypervisor, talking to the control plane over the API). The
// provisioning layer is unaffected — it sees a resource, not a hypervisor.
//
// Which binary, machine type, accelerator and firmware this host needs is
// resolved at RUNTIME by the platform package, not by build tags — see its
// package comment for why.
//
// The GC contract lives in driverkit and is identical for every substrate. What
// is HERE is only what qemu decides for itself: its SCC (process + disk + pflash
// + state dir are ONE unit), where the site tag lives (a path component), and
// how a neutral profile name resolves to a local artifact.
//
// The `hvf` type and the ~/.hvf state root keep their names from when this was
// macOS-only. They are load-bearing — the state path and the
// machine.hvf.fleet.io API group are what installed machines already use — so
// renaming them is a migration, not a rename.
//
// tier: compute uses QEMU user-mode networking, which requires NO ROOT. Root is
// a vmnet requirement, so it arrives with tier fabric-sim, not before.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coglative/talos-in-qemu/cluster"
	"github.com/coglative/talos-in-qemu/driverkit"
	"github.com/coglative/talos-in-qemu/platform"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

type hvf struct {
	stateRoot string
	// Where neutral profile names resolve. Provider config, not claim content
	// (ARCHITECTURE.md D12).
	imageRoot string
	// platform.Detect execs `qemu-system-X -accel help` and walks two registry
	// directories. create() runs on EVERY reconcile tick where Observe reports
	// absent, so a VM that fails to start would re-run the whole probe forever
	// and re-write the multi-line accel diagnostic into a status condition each
	// pass. Host facts do not change while the process runs, so probe once.
	//
	// Deliberately NOT hoisted into main(): destroy must keep working on a host
	// with no usable accelerator. Teardown cannot require a live hypervisor.
	detect func() (*platform.Platform, error)
}

func main() {
	driverkit.Kubeconfig()
	stateRoot := flag.String("state-root", filepath.Join(os.Getenv("HOME"), ".hvf"), "per-machine state root")
	imageRoot := flag.String("image-root", filepath.Join(os.Getenv("HOME"), ".hvf", "images"), "root for resolving non-absolute spec.image profile names")
	interval := flag.Duration("interval", 5*time.Second, "reconcile interval")
	apply := flag.String("apply", "", "BOOTSTRAP: reconcile ONE TalosMachine read from this YAML file, with no control plane, then exit")
	destroyF := flag.String("destroy", "", "BOOTSTRAP: destroy the TalosMachine described by this YAML file, then exit")
	up := flag.String("up", "", "BOOTSTRAP: -apply, then bring the machine up to a single-node Kubernetes cluster, then exit")
	flag.Parse()

	if err := os.MkdirAll(*stateRoot, 0o755); err != nil {
		log.Fatalf("state root: %v", err)
	}
	d := &hvf{stateRoot: *stateRoot, imageRoot: *imageRoot, detect: sync.OnceValues(platform.Detect)}

	// BOOTSTRAP MODE — the chicken-and-egg door.
	//
	// This provider reconciles TalosMachine CRs, so it needs a control plane to
	// read them from. On a laptop with no cluster yet, that is circular: the
	// cluster is the thing we are trying to create. The escape used to be a
	// kind cluster, which drags in a container runtime purely to bootstrap a
	// hypervisor that does not need one.
	//
	// So: read ONE CR from a file and run it through the SAME Driver the
	// controller loop uses. Not a second way to make a VM — the identical
	// Observe/Create/Destroy, identical qemu invocation, identical SCC and
	// state layout. The only thing bypassed is where the CR came from.
	// Anything else would be two truths about how a machine gets built, and
	// they would drift.
	//
	// Once the first node is up and bootstrapped it becomes the management
	// cluster, and this same binary runs against it in controller mode for
	// every machine after.
	//
	// -up is -apply plus the cluster. It is the SAME create() and the same
	// state layout — the VM half is byte-for-byte what -apply builds — with
	// cluster.Up driving the Talos side afterwards. -apply stays VM-only, and
	// -destroy stays working with no hypervisor and no reachable node.
	if *apply != "" || *destroyF != "" || *up != "" {
		path, verb := *apply, "apply"
		switch {
		case *destroyF != "":
			path, verb = *destroyF, "destroy"
		case *up != "":
			path, verb = *up, "up"
		}
		if err := standalone(context.Background(), d, path, verb); err != nil {
			log.Fatalf("%s %s: %v", verb, path, err)
		}
		return
	}

	log.Fatal(driverkit.Run(context.Background(), driverkit.Config{
		GVR: schema.GroupVersionResource{
			Group: "machine.hvf.fleet.io", Version: "v1alpha1", Resource: "talosmachines",
		},
		Finalizer: "machine.hvf.fleet.io/vm",
		Interval:  *interval,
	}, d))
}

// standalone runs one CR through the Driver with no control plane. It is
// deliberately thin: decode, Observe, then Create or Destroy. Every decision
// about WHAT a machine is stays in the driver, so bootstrap and steady state
// cannot disagree.
//
// Create is skipped when Observe reports present, which is the same
// already-exists rule the controller applies — so re-running is safe and does
// not start a second qemu against the same state dir.
func standalone(ctx context.Context, d *hvf, path, verb string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var obj map[string]interface{}
	if err := yaml.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	m := &unstructured.Unstructured{Object: obj}
	if m.GetUID() == "" {
		// The controller gets a UID from the API server; here there is none, so
		// derive a STABLE one from namespace/name. It keys the state dir, so it
		// must be identical across runs or a re-apply would orphan the first VM
		// and boot a second beside it.
		m.SetUID(types.UID(fmt.Sprintf("bootstrap-%s-%s", m.GetNamespace(), m.GetName())))
	}

	exists, status, err := d.Observe(ctx, m)
	if err != nil {
		return fmt.Errorf("observe: %w", err)
	}

	switch verb {
	case "destroy":
		if !exists {
			log.Printf("already gone: %s", d.dir(m))
			return nil
		}
		return d.Destroy(ctx, m)
	case "up":
		return bringUp(ctx, d, m, exists, status)
	default:
		if exists {
			log.Printf("already running: %v", status)
			return nil
		}
		if err := d.Create(ctx, m); err != nil {
			return err
		}
		_, status, err = d.Observe(ctx, m)
		if err != nil {
			return fmt.Errorf("observe after create: %w", err)
		}
		log.Printf("created: %v", status)
		return nil
	}
}

// The two guest ports a bring-up talks to. They are Talos's, not ours: apid
// serves on 50000 and kube-apiserver on 6443, so these are the guest sides that
// spec.hostForwards has to map onto the host.
const (
	talosAPIGuestPort = 50000
	kubeAPIGuestPort  = 6443
)

// hostForward reports the HOST port forwarded to guestPort, or 0 when the
// machine forwards nothing there.
//
// Everything reachable from the host goes through a qemu user-mode forward, so
// a missing entry is not a slow path — it is an address that will never answer.
// cluster.Up refuses an empty endpoint up front rather than spending a wait's
// whole budget discovering that.
func hostForward(m *unstructured.Unstructured, guestPort int) int {
	for _, hf := range nestedSlice(m, "spec", "hostForwards") {
		h, _ := hf.(map[string]interface{})
		if toInt(h["guestPort"]) == guestPort {
			return toInt(h["hostPort"])
		}
	}
	return 0
}

// talosEndpoint is the host side of the Talos API forward, host:port, or "".
func talosEndpoint(m *unstructured.Unstructured) string {
	if p := hostForward(m, talosAPIGuestPort); p > 0 {
		return fmt.Sprintf("127.0.0.1:%d", p)
	}
	return ""
}

// kubeEndpoint is the host side of the Kubernetes API forward, as a URL, or "".
//
// It is written into the generated config as the control-plane endpoint AND
// into the kubeconfig, so it has to be the address the HOST can reach — the
// guest's own is unroutable from here without a bridge.
func kubeEndpoint(m *unstructured.Unstructured) string {
	if p := hostForward(m, kubeAPIGuestPort); p > 0 {
		return fmt.Sprintf("https://127.0.0.1:%d", p)
	}
	return ""
}

// bringUp is the -up verb: create (or adopt) the VM, then hand everything after
// that to cluster.Up, which owns all ten steps and every line of output.
func bringUp(ctx context.Context, d *hvf, m *unstructured.Unstructured, exists bool,
	status map[string]interface{}) error {
	opts, err := upOptions(d, m, exists, status)
	if err != nil {
		return err
	}
	return cluster.Up(ctx, opts)
}

// upOptions translates a TalosMachine into what cluster.Up needs.
//
// It is separate from bringUp so it can be ASSERTED without a hypervisor. Every
// field below is a value that would compile just as happily wrong — the state
// dir, the two endpoints, the two disk serials, the resolved image — and each
// one is only visibly wrong minutes into a bring-up, on a node that is by then
// half installed.
//
// The VM half is create(), unchanged and unbranched: -up must not become a
// second way to build a machine. What is HERE is only the translation, and
// package main is the only place that can do it — the serials, the qemu
// forwards and the profile resolution are all its.
func upOptions(d *hvf, m *unstructured.Unstructured, exists bool,
	status map[string]interface{}) (cluster.UpOptions, error) {
	spec, _, _ := unstructured.NestedMap(m.Object, "spec")

	image, err := d.resolveImage(spec)
	if err != nil {
		return cluster.UpOptions{}, err
	}

	// The MACHINE's state dir, never the state root: the artifacts carry the
	// identity they belong to, which is the property that makes -destroy sweep
	// them. Written one level up they would outlive the cluster whose keys
	// they are, and the residue check would not find them.
	dir := d.dir(m)

	return cluster.UpOptions{
		ClusterName:      m.GetName(),
		ImagePath:        image,
		StateDir:         dir,
		TalosEndpoint:    talosEndpoint(m),
		KubeEndpoint:     kubeEndpoint(m),
		SystemDiskSerial: DiskSerialSystem,
		DataDiskSerial:   dataDiskSerial(spec),
		Detect:           d.detect,
		Boot: func() (int, error) {
			// The same already-exists rule -apply applies, and it is what
			// makes `-apply` then `-up` work: a VM already sitting in
			// maintenance mode is ADOPTED, not duplicated. Starting a second
			// qemu against one state dir would corrupt the disk it shares.
			if exists {
				return toInt(status["pid"]), nil
			}
			return d.create(m, dir)
		},
	}, nil
}

// dir keys state by SITE then UID. The site is IN THE PATH on purpose: artifacts
// must carry the identity they belong to or they cannot be garbage-collected —
// the residue check greps for it, and it is the same property that makes gcp
// labels and aws tags work. UID underneath so a recreated resource never reuses
// a stale directory.
func (h *hvf) dir(m *unstructured.Unstructured) string {
	return filepath.Join(h.stateRoot, driverkit.Str(m, "spec", "site"), string(m.GetUID()))
}

// Observe reads the pidfile the hypervisor itself wrote and checks LIVENESS.
// Never trust a state file alone: talosctl's `cluster show` deserialises
// state.yaml and reports a long-dead cluster as present.
func (h *hvf) Observe(ctx context.Context, m *unstructured.Unstructured) (bool, map[string]interface{}, error) {
	dir := h.dir(m)
	pid := readPid(dir)
	if pid <= 0 || !processAlive(pid) {
		return false, nil, nil
	}
	api := ""
	for _, hf := range nestedSlice(m, "spec", "hostForwards") {
		hh, _ := hf.(map[string]interface{})
		if toInt(hh["guestPort"]) == 50000 {
			api = fmt.Sprintf("127.0.0.1:%d", toInt(hh["hostPort"]))
		}
	}
	return true, map[string]interface{}{
		"pid": int64(pid), "stateDir": dir, "apiEndpoint": api,
	}, nil
}

func (h *hvf) Create(ctx context.Context, m *unstructured.Unstructured) error {
	_, err := h.create(m, h.dir(m))
	return err
}

// Destroy takes the WHOLE SCC: the process (which sweeps everything inside the
// VM) and the state dir (everything outside it). Idempotent — it is called on
// every delete tick until it succeeds.
func (h *hvf) Destroy(ctx context.Context, m *unstructured.Unstructured) error {
	return destroy(h.dir(m))
}

func readPid(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "qemu.pid"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// resolveImage turns spec.image into a path to a local artifact.
//
// The claim carries a NEUTRAL PROFILE NAME (talos-nocloud.img), not a path.
// Resolving it to a local artifact is substrate-local configuration and belongs
// to the provider — same argument as GCP's project. An absolute path in the
// claim would be a leak: it cannot travel to GCP or AWS, where the same profile
// resolves to an image URI or an AMI.
//
// It is a function of its own because -up needs the SAME answer create() uses:
// the ISO it boots is the ISO whose volume id pins the installer, and two
// resolutions of one profile name are two answers waiting to disagree.
func (h *hvf) resolveImage(spec map[string]interface{}) (string, error) {
	image, _ := spec["image"].(string)
	if image == "" {
		return "", fmt.Errorf("spec.image is required")
	}
	if !filepath.IsAbs(image) {
		image = filepath.Join(h.imageRoot, image)
	}
	if _, err := os.Stat(image); err != nil {
		return "", fmt.Errorf("resolve profile %q under %s: %w", spec["image"], h.imageRoot, err)
	}
	return image, nil
}

func (h *hvf) create(m *unstructured.Unstructured, dir string) (int, error) {
	spec, _, _ := unstructured.NestedMap(m.Object, "spec")

	// Resolve host facts BEFORE creating any state. Failing here costs nothing;
	// failing after the disk exists leaves residue behind.
	p, err := h.detect()
	if err != nil {
		return 0, err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}

	image, err := h.resolveImage(spec)
	if err != nil {
		return 0, err
	}
	// The symptom is NOT "no bootable media": Talos ISOs carry BOTH BOOTX64.EFI
	// and BOOTAA64.EFI (the very property that defeats ESP-based detection — see
	// platform.InspectImageArch), so UEFI does find a bootloader and GRUB is
	// what fails, visibly, on the serial console. Do not "fix" this message back
	// to the intuitive version; the wording below was observed, not guessed.
	// Warn only; detection returning "" must never block a valid image we simply
	// cannot classify.
	if got := platform.InspectImageArch(image); got != "" && got != p.ImageArch {
		log.Printf("warning: image is %s but host is %s\n"+
			"  the VM starts and UEFI loads a bootloader (Talos ships stubs for\n"+
			"  both arches), then GRUB stops at \"Failed to boot both default and\n"+
			"  fallback entries.\" — no kernel runs, so there is no Talos API.\n"+
			"  this is not a hang — it is the wrong image: %s", got, p.ImageArch, image)
	}

	cpu := specCPU(spec)
	mem := toMB(str(spec["memory"], "2Gi"))
	diskPath := filepath.Join(dir, "system.qcow2")
	if err := ensureQcow2(diskPath, str(spec["disk"], "16Gi")); err != nil {
		return 0, err
	}
	// The OPTIONAL second disk, for PVCs. It is created here beside the system
	// disk but WIRED IN BELOW as an append, never woven into the arg literal:
	// a machine with no dataDisk has to emit precisely the argv it emitted
	// before this field existed.
	dataPath := ""
	if size := specDataDisk(spec); size != "" {
		dataPath = filepath.Join(dir, "data.qcow2")
		if err := ensureQcow2(dataPath, size); err != nil {
			return 0, err
		}
	}

	varsPath := filepath.Join(dir, "efivars.fd")
	if err := ensureEFIVars(varsPath, p.FirmwareVars); err != nil {
		return 0, err
	}

	// user-mode networking: unprivileged by construction. hostfwd is how the
	// control plane reaches the Talos API without a bridge.
	netdev := "user,id=n0"
	for _, hf := range nestedSlice(m, "spec", "hostForwards") {
		h, _ := hf.(map[string]interface{})
		hp, gp := toInt(h["hostPort"]), toInt(h["guestPort"])
		if hp <= 0 || gp <= 0 {
			continue
		}
		// PROTOCOL IS PER-FORWARD, and defaults to tcp only.
		//
		// This emitted tcp unconditionally, which silently has no path for any
		// UDP service — QUIC, WebTransport, DNS. The failure is nasty because
		// the TCP half usually works: an HTTP/3 origin serves its page over h2
		// and then the browser's WebTransport dial goes nowhere, which presents
		// as a certificate rejection rather than a missing route.
		//
		// `both` is the common case for an HTTP/3 endpoint (h2 on TCP and H3 on
		// UDP at the same port), so it is spelled once here rather than forcing
		// two entries that must be kept in step.
		switch strings.ToLower(str(h["protocol"], "tcp")) {
		case "udp":
			netdev += fmt.Sprintf(",hostfwd=udp:127.0.0.1:%d-:%d", hp, gp)
		case "both", "tcp+udp":
			netdev += fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:%d", hp, gp)
			netdev += fmt.Sprintf(",hostfwd=udp:127.0.0.1:%d-:%d", hp, gp)
		default:
			netdev += fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:%d", hp, gp)
		}
	}

	args := []string{
		"-machine", p.Machine + ",accel=" + p.Accel, "-cpu", p.CPU,
		"-smp", strconv.Itoa(cpu),
		"-m", strconv.Itoa(mem),
		"-drive", "if=pflash,format=raw,readonly=on,file=" + p.FirmwareCode,
		"-drive", "if=pflash,format=raw,file=" + varsPath,
		// BOOT ORDER IS THE WHOLE INSTALL LIFECYCLE, and it has to be explicit.
		//
		// Talos ships a bootable ISO: you boot it, it installs to disk, and from
		// then on the machine must boot the DISK. If the ISO keeps winning, Talos
		// refuses to install-loop — it halts with
		//
		//   "Talos is already installed to disk but booted from another media
		//    and talos.halt_if_installed=1"
		//
		// which is a dead node, forever. The obvious fix (detach the ISO once
		// install finishes) needs the provider to track installed-ness, i.e. new
		// state that can disagree with reality. `bootindex` gets it for free and
		// STATELESSLY: firmware tries the disk first and only falls through to
		// the ISO while the disk is still blank. Install flips the behaviour
		// because the disk becomes bootable, not because anything recorded that
		// it did.
		//
		// Explicit `-device` for BOTH (rather than the `if=virtio` shorthand) is
		// required to carry bootindex, and it also pins guest enumeration: the
		// system disk is vda and the ISO is vdb. Do not depend on that order for
		// the install target anyway — select by SERIAL; qemu arg order deciding
		// a device name is not a contract worth resting on.
		//
		// THE SERIAL IS AN IDENTITY, AND THAT IS THE WHOLE POINT. `serial=`
		// surfaces in the guest as /sys/block/<dev>/serial, which is what
		// Talos's InstallDiskSelector.Serial reads. The alternative — matching
		// on size, which the README hands out as `size: '> 10GB'` — only
		// works while exactly one disk is large. Add a data disk and it
		// becomes a coin flip between the OS install target and the user's
		// data: the same failure the /dev/vdX warning above is about, arriving
		// through a different door.
		"-drive", "if=none,id=sys,format=qcow2,file=" + diskPath,
		"-device", "virtio-blk-pci,drive=sys,serial=" + DiskSerialSystem + ",bootindex=0",
		"-drive", "if=none,id=cd,media=cdrom,file=" + image,
		"-device", "virtio-blk-pci,drive=cd,bootindex=1",
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none",
		"-serial", "file:" + filepath.Join(dir, "serial.log"),
		"-pidfile", filepath.Join(dir, "qemu.pid"),
		"-daemonize",
	}

	// APPENDED, not spliced into the literal above, so the no-dataDisk argv is
	// unchanged down to the position of every element.
	//
	// NO bootindex, deliberately. Firmware tries every bootindex it is handed,
	// and this disk must never be a boot candidate: while the system disk is
	// still blank the only thing that may follow it is the install ISO. A
	// bootindex here would insert the PVC disk into that sequence.
	if dataPath != "" {
		args = append(args,
			"-drive", "if=none,id=data,format=qcow2,file="+dataPath,
			"-device", "virtio-blk-pci,drive=data,serial="+DiskSerialData)
	}

	cmd := exec.Command(p.QEMUBinary, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("qemu: %v: %s", err, strings.TrimSpace(string(out)))
	}
	b, err := os.ReadFile(filepath.Join(dir, "qemu.pid"))
	if err != nil {
		return 0, fmt.Errorf("pidfile: %w", err)
	}
	// The installed system writes its OWN kernel cmdline and does not inherit
	// the ISO's console, so the config patch has to name it — and the name is
	// architecture-specific (ttyS0 vs ttyAMA0). The README used to make the
	// reader work that out; we already resolved it, so say it.
	log.Printf("for the install config patch on this host: extraKernelArgs: [%s]", p.ConsoleArg)
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// ensureQcow2 creates a sparse qcow2 at path, and does NOTHING if one is
// already there.
//
// Never recreated, never resized: system.qcow2 holds the installed OS and
// data.qcow2 holds the user's PVCs. create() runs on EVERY reconcile tick where
// Observe reports absent, so a version of this that rewrote the file would wipe
// a machine the moment its qemu process died and the controller tried to bring
// it back — the exact case where the data matters most.
//
// The suffix trim is not cosmetic. Kubernetes quantities are Gi/Mi; qemu-img
// accepts only G/M and rejects the "i" outright with "Invalid image size
// specified!". Both spellings mean the same power-of-two bytes, so dropping the
// "i" is exact, not a rounding.
func ensureQcow2(path, size string) error {
	// Only ENOENT means "create it". EACCES, ENOTDIR or a symlink loop are
	// reported: treating them as "already there" skips creation and then
	// launches QEMU against a file that does not exist.
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	out, err := exec.Command("qemu-img", "create", "-f", "qcow2", path, strings.TrimSuffix(size, "i")).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img: %v: %s", err, out)
	}
	return nil
}

// destroy is idempotent — it is called on every delete tick until it succeeds.
func destroy(dir string) error {
	if b, err := os.ReadFile(filepath.Join(dir, "qemu.pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
			for i := 0; i < 50 && processAlive(pid); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if processAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// Reap the now-empty <stateRoot>/<site> parent. os.Remove fails harmlessly
	// while siblings remain, so the last machine of a site takes the site dir
	// with it. An empty directory is trivial residue — and trivial residue is
	// how "zero" quietly becomes "nearly zero".
	os.Remove(filepath.Dir(dir))
	return nil
}

func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// ensureEFIVars makes path a per-machine, writable copy of the firmware's own
// nvram template, VERBATIM. UEFI vars must be per-machine: a shared copy is how
// two VMs end up fighting over boot state.
//
// Verbatim, because the previous version padded to 64 MiB — correct on aarch64
// only by coincidence, since edk2's aarch64 vars template genuinely is 67108864
// bytes. The x86_64 template is 540672 bytes, and padding it makes QEMU refuse
// to start:
//
//	combined size of system firmware exceeds 8388608 bytes
//
// SIZE — not mere absence — IS THE REGENERATION TRIGGER. Any state dir the
// padding version touched still holds that poisoned file, and an absence-only
// check would preserve it forever: re-running would keep failing on exactly the
// bug this replaces.
//
// The heal is NOT universal, and the x86_64 case is the only one that needs it.
// On aarch64 the template is itself 67108864 bytes, so a file the padding
// version wrote MATCHES the template size and is left alone here. That is
// benign: the 8 MiB combined-firmware limit is an x86_64 limit, and a blank
// 64 MiB varstore is what edk2 reformats in-guest on first boot anyway.
//
// It is deliberately NOT regenerated unconditionally. The guest writes its own
// UEFI boot entries here, and discarding them on every re-create would lose real
// state. A size that disagrees with the template is the signal that the file did
// not come from this template and cannot be trusted; a size that agrees is left
// strictly alone.
func ensureEFIVars(path, template string) error {
	tmplSt, err := os.Stat(template)
	if err != nil {
		// Unreachable in practice: Detect resolves FirmwareVars by statting it.
		return fmt.Errorf("stat nvram template %s: %w", template, err)
	}
	if st, err := os.Stat(path); err == nil && st.Size() == tmplSt.Size() {
		return nil
	}
	b, err := os.ReadFile(template)
	if err != nil {
		return fmt.Errorf("read nvram template %s: %w", template, err)
	}
	return os.WriteFile(path, b, 0o644)
}

// ── tiny helpers ────────────────────────────────────────────────────────────

func str(v interface{}, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// specCPU resolves spec.cpu, defaulting to 2.
//
// sigs.k8s.io/yaml routes through JSON, so a bootstrap file yields float64 here
// and NEVER int64 — a bare .(int64) assertion silently dropped every
// user-specified cpu count back to the default. toInt takes both, which is also
// what the API server (int64) path needs.
//
// It lives out here so the resolution is testable through the real YAML decoder
// without dragging argv construction along.
func specCPU(spec map[string]interface{}) int {
	if v := toInt(spec["cpu"]); v > 0 {
		return v
	}
	return 2
}

// Disk serials are the machine's DISK NAMING CONTRACT, not decoration. QEMU
// passes `serial=` through to the guest as /sys/block/<dev>/serial, and Talos
// reads it back through InstallDiskSelector.Serial — so these two strings are
// what a generated machine config selects on. Changing one renames a disk out
// from under an already-installed node.
const (
	DiskSerialSystem = "talos-system"
	DiskSerialData   = "talos-data"
)

// specDataDisk resolves spec.dataDisk, the OPTIONAL second disk for PVCs.
//
// "" means no second disk, and that is the default on purpose: a machine
// without it must be the machine this tool built before the field existed —
// same qemu args, same devices, same guest.
//
// Same YAML-through-JSON caveat as specCPU, which is why it reads through str
// rather than asserting .(string) at the call site: a non-string value is "not
// set", never a panic.
func specDataDisk(spec map[string]interface{}) string {
	return str(spec["dataDisk"], "")
}

// dataDiskSerial is the serial cluster.Up selects the PVC volume on, or "" when
// this machine has no data disk.
//
// ONE field decides BOTH halves of storage — the UserVolumeConfig in the
// generated machine config and the StorageClass installed into the cluster — so
// they cannot disagree. It reads through specDataDisk, the same resolution
// create() uses to decide whether to make the disk at all: a `dataDisk: 40`
// with the unit left off is "not set" in both places, which is what makes the
// announced skip in step 10 the first visible sign of the typo rather than a
// Pending PVC an hour later.
func dataDiskSerial(spec map[string]interface{}) string {
	if specDataDisk(spec) == "" {
		return ""
	}
	return DiskSerialData
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func toMB(s string) int {
	s = strings.TrimSpace(s)
	mult := 1
	switch {
	case strings.HasSuffix(s, "Gi"), strings.HasSuffix(s, "G"):
		mult = 1024
	case strings.HasSuffix(s, "Mi"), strings.HasSuffix(s, "M"):
		mult = 1
	}
	n := strings.TrimRight(s, "GiMB")
	v, err := strconv.Atoi(n)
	if err != nil || v <= 0 {
		return 2048
	}
	return v * mult
}

func nestedSlice(m *unstructured.Unstructured, f ...string) []interface{} {
	v, _, _ := unstructured.NestedSlice(m.Object, f...)
	return v
}
