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
	if *apply != "" || *destroyF != "" {
		path, verb := *apply, "apply"
		if *destroyF != "" {
			path, verb = *destroyF, "destroy"
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

	image, _ := spec["image"].(string)
	if image == "" {
		return 0, fmt.Errorf("spec.image is required")
	}
	// The claim carries a NEUTRAL PROFILE NAME (talos-nocloud.img), not a path.
	// Resolving it to a local artifact is substrate-local configuration and
	// belongs to the provider — same argument as GCP's project. An absolute path
	// in the claim would be a leak: it cannot travel to GCP or AWS, where the
	// same profile resolves to an image URI or an AMI.
	if !filepath.IsAbs(image) {
		image = filepath.Join(h.imageRoot, image)
	}
	if _, err := os.Stat(image); err != nil {
		return 0, fmt.Errorf("resolve profile %q under %s: %w", spec["image"], h.imageRoot, err)
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
	if _, err := os.Stat(diskPath); os.IsNotExist(err) {
		size := strings.TrimSuffix(str(spec["disk"], "16Gi"), "i")
		out, err := exec.Command("qemu-img", "create", "-f", "qcow2", diskPath, size).CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("qemu-img: %v: %s", err, out)
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
		// BIND ADDRESS is per-forward and defaults to loopback.
		//
		// Loopback is the safe default: on macOS it is what Local Network
		// Privacy exempts, so a browser on the same machine reaches it without a
		// permission prompt, and nothing is exposed to the network.
		//
		// But loopback is unreachable from ANOTHER DEVICE. A phone, a tablet, a
		// second laptop on the same Wi-Fi cannot see it at all — which is the
		// difference between "runs on my machine" and "runs in a demo". Set
		// hostAddr to 0.0.0.0 (or a specific interface address) to publish it.
		// That is a deliberate, per-port exposure decision, not a global switch.
		addr := str(h["hostAddr"], "127.0.0.1")
		switch strings.ToLower(str(h["protocol"], "tcp")) {
		case "udp":
			netdev += fmt.Sprintf(",hostfwd=udp:%s:%d-:%d", addr, hp, gp)
		case "both", "tcp+udp":
			netdev += fmt.Sprintf(",hostfwd=tcp:%s:%d-:%d", addr, hp, gp)
			netdev += fmt.Sprintf(",hostfwd=udp:%s:%d-:%d", addr, hp, gp)
		default:
			netdev += fmt.Sprintf(",hostfwd=tcp:%s:%d-:%d", addr, hp, gp)
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
		// the install target anyway — select by size (see README); qemu arg order
		// deciding a device name is not a contract worth resting on.
		"-drive", "if=none,id=sys,format=qcow2,file=" + diskPath,
		"-device", "virtio-blk-pci,drive=sys,bootindex=0",
		"-drive", "if=none,id=cd,media=cdrom,file=" + image,
		"-device", "virtio-blk-pci,drive=cd,bootindex=1",
		"-netdev", netdev,
		"-device", "virtio-net-pci,netdev=n0",
		"-display", "none",
		"-serial", "file:" + filepath.Join(dir, "serial.log"),
		"-pidfile", filepath.Join(dir, "qemu.pid"),
		"-daemonize",
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
