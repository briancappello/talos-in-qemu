# Cluster Bring-Up Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `tinq -up machine.yaml` turns a booted Talos VM into a working single-node Kubernetes cluster, announcing each step and why.

**Architecture:** A new `cluster` package drives Talos through `pkg/machinery`. `platform` (branch 1) gains ISO version detection. `cmd/tinq` gains a `-up` verb; `-apply` and `-destroy` are unchanged. Storage is a second serial-named QEMU disk plus `rancher/local-path-provisioner`.

**Tech Stack:** Go 1.26, `github.com/siderolabs/talos/pkg/machinery` v1.13.7, existing `k8s.io/client-go`. No other new dependencies.

## Global Constraints

- Go 1.26. Module `github.com/coglative/talos-in-qemu`.
- **Exactly one new dependency:** `github.com/siderolabs/talos/pkg/machinery v1.13.7`. Nothing else. (`go.sum` 45 → 84 modules is expected and accepted.)
- **No build tags.** Must vet clean for `darwin/arm64` and `linux/amd64` (use `go vet`, not `go build` — `go build` skips tests).
- `driverkit/` must NOT be modified.
- **`-apply` and `-destroy` behaviour must not change.** Omitting `spec.dataDisk` must produce byte-identical QEMU args to today, except the system disk gaining a serial.
- **`-destroy` must keep working with no hypervisor and no reachable node.** Teardown never requires a healthy cluster.
- **Bootstrap-only.** No upgrade, scaling, or steady-state reconciliation verbs. That scope is what keeps the CRD's provider-talos boundary true.
- Secrets files (`talosconfig`, `kubeconfig`, `secrets.yaml`) are mode `0600`.
- **Mutation-verify every guard.** Branch 1 shipped four tasks whose tests passed when the code they verified was deleted. For each new guard: delete it, prove a test fails, restore. Report per-guard kill/survive.

---

## File Structure

| File | Responsibility |
|---|---|
| `platform/image.go` | (extend) ISO Talos version from the PVD volume id |
| `cluster/version.go` | machinery-vs-image version guard |
| `cluster/config.go` | machine config + talosconfig generation |
| `cluster/client.go` | Talos client: maintenance + authenticated, readiness probes |
| `cluster/storage.go` | local-path-provisioner manifests via client-go |
| `cluster/up.go` | the 10-step orchestration, all output |
| `cmd/tinq/main.go` | `-up` verb, second disk, serials |
| `crd/talosmachine.yaml` | `dataDisk` field |

---

### Task 1: ISO Talos version detection

**Files:**
- Modify: `platform/image.go`
- Test: `platform/image_test.go`

**Interfaces:**
- Consumes: the existing ISO9660 PVD read in `InspectImageArch`.
- Produces: `func InspectImageVersion(path string) string` returning `"v1.13.7"`, or `""` when unknown.

The ISO9660 volume identifier sits at PVD offset 40, 32 bytes, space-padded. Talos writes `TALOS_V1_13_7` / `TALOS_V1_9_5`.

- [ ] **Step 1: Write the failing test**

```go
func TestInspectImageVersion(t *testing.T) {
	for _, tc := range []struct{ volID, want string }{
		{"TALOS_V1_13_7", "v1.13.7"},
		{"TALOS_V1_9_5", "v1.9.5"},
		{"TALOS_V1_0_0", "v1.0.0"},
		{"UBUNTU_24_04", ""},
		{"", ""},
		{"TALOS_V1_13", ""},      // too few components
		{"TALOS_VX_Y_Z", ""},     // non-numeric
	} {
		p := synthISOWithVolID(t, 0x8664, "VMLINUZ.;1", tc.volID)
		if got := InspectImageVersion(p); got != tc.want {
			t.Errorf("volID %q => %q, want %q", tc.volID, got, tc.want)
		}
	}
}

func TestInspectImageVersionUnknownIsSilent(t *testing.T) {
	dir := t.TempDir()
	notISO := filepath.Join(dir, "raw.img")
	if err := os.WriteFile(notISO, make([]byte, 4*isoSector), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := InspectImageVersion(notISO); got != "" {
		t.Errorf("non-ISO => %q, want empty", got)
	}
	if got := InspectImageVersion(filepath.Join(dir, "absent.iso")); got != "" {
		t.Errorf("missing file => %q, want empty", got)
	}
}

func TestInspectImageVersionRealISOs(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	for _, tc := range []struct{ name, want string }{
		{"talos-v1.9.5-amd64.iso", "v1.9.5"},
		{"talos-v1.13.7-amd64.iso", "v1.13.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(home, ".hvf", "images", tc.name)
			if _, err := os.Stat(p); err != nil {
				t.Skipf("%s not present", p)
			}
			if got := InspectImageVersion(p); got != tc.want {
				t.Errorf("%s => %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
```

Extend the existing `synthISO` helper into `synthISOWithVolID(t, machine uint16, kernelName, volID string)` writing `volID` into `pvd[40:72]` space-padded to 32 bytes; keep `synthISO` as a wrapper passing a default id so existing tests are untouched.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./platform/ -run TestInspectImageVersion -v`
Expected: FAIL — `undefined: InspectImageVersion`

- [ ] **Step 3: Write minimal implementation**

```go
// InspectImageVersion reports the Talos version of a boot ISO from its ISO9660
// volume identifier, or "" when it cannot tell. Like InspectImageArch it never
// errors: unknown disables the version guard rather than blocking an image we
// merely fail to classify.
//
// Talos writes the volume id as TALOS_V<major>_<minor>_<patch>, e.g.
// TALOS_V1_13_7. This is far cheaper than parsing the kernel and is the same
// string `file(1)` reports.
func InspectImageVersion(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	pvd := make([]byte, sectorSize)
	if _, err := f.ReadAt(pvd, 16*sectorSize); err != nil {
		return ""
	}
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		return ""
	}

	volID := strings.TrimSpace(string(pvd[40:72]))
	rest, ok := strings.CutPrefix(volID, "TALOS_V")
	if !ok {
		return ""
	}
	parts := strings.Split(rest, "_")
	if len(parts) != 3 {
		return ""
	}
	for _, p := range parts {
		if p == "" {
			return ""
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return ""
			}
		}
	}
	return "v" + strings.Join(parts, ".")
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./platform/ -v` — including the real-ISO subtests, which must RUN not skip.

- [ ] **Step 5: Mutation sweep**

Delete each guard in turn (`pvd[0] != 1`, `CD001`, `CutPrefix`, `len(parts) != 3`, the digit loop) and confirm a test fails. Report kill/survive per guard.

- [ ] **Step 6: Commit**

```bash
git add platform/image.go platform/image_test.go
git commit -m "feat(platform): read the Talos version from the ISO volume id"
```

---

### Task 2: The version guard

**Files:**
- Create: `cluster/version.go`, `cluster/version_test.go`
- Modify: `go.mod` (add machinery — the ONLY new dependency)

**Interfaces:**
- Consumes: `platform.InspectImageVersion`.
- Produces: `func GeneratorVersion() string`; `func CheckVersion(imageVersion string) error`.

**Why this exists:** `config.ParseContractFromVersion("v1.99.0")` returns `{1,99}` with **no error**, every `contract.XxxSupported()` predicate returns true because 99 outranks everything, and a plausible config is generated for a Talos that does not exist. Verified. Machinery's contract support is documented backwards-only, so the pin is a floor: machinery must be ≥ the image.

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/siderolabs/talos/pkg/machinery@v1.13.7
go mod tidy
```

Confirm exactly one new direct require. Report the module count delta.

- [ ] **Step 2: Write the failing test**

```go
package cluster

import "testing"

func TestCheckVersion(t *testing.T) {
	for _, tc := range []struct {
		name, image string
		wantErr     bool
	}{
		{"same as generator", GeneratorVersion(), false},
		{"older minor", "v1.9.5", false},
		{"much older", "v1.0.0", false},
		{"newer minor", "v1.99.0", true},
		{"newer major", "v2.0.0", true},
		{"unknown image version", "", false}, // guard disabled, never blocks
		{"unparseable", "not-a-version", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckVersion(tc.image)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckVersion(%q) err=%v, wantErr=%v", tc.image, err, tc.wantErr)
			}
		})
	}
}

// The message must name BOTH versions and say what to do — the failure it
// prevents is a config silently generated for a Talos that does not exist.
func TestCheckVersionMessageIsActionable(t *testing.T) {
	err := CheckVersion("v1.99.0")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"v1.99.0", GeneratorVersion(), "silently"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must contain %q, got: %s", want, msg)
		}
	}
}

func TestGeneratorVersionIsAVersion(t *testing.T) {
	v := GeneratorVersion()
	if !strings.HasPrefix(v, "v1.") {
		t.Errorf("GeneratorVersion() = %q, want a v1.x version", v)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cluster/ -v`
Expected: FAIL — `undefined: CheckVersion`

- [ ] **Step 4: Write minimal implementation**

```go
// Package cluster brings a booted Talos machine up to a working single-node
// Kubernetes cluster. It is deliberately BOOTSTRAP ONLY: no upgrade, scaling or
// steady-state reconciliation. Steady state belongs to provider-talos, per
// crd/talosmachine.yaml — this exists because provider-talos runs INSIDE a
// cluster and so cannot create your first one.
package cluster

import (
	"fmt"
	"strings"

	"github.com/siderolabs/talos/pkg/machinery/config"
	"github.com/siderolabs/talos/pkg/machinery/gendata"
)

// GeneratorVersion is the Talos version this binary's machinery can generate
// configs for. It is a compile-time constant, which is the whole reason for
// linking machinery rather than shelling out to talosctl: an ambient binary
// can change underneath us, a linked version cannot.
func GeneratorVersion() string { return gendata.VersionTag }

// CheckVersion refuses to generate a config for an image NEWER than the
// generator.
//
// Machinery's VersionContract is documented backwards-only: v1.13 machinery can
// target 1.0..1.13, never 1.14. The trap is that exceeding it does NOT error —
// ParseContractFromVersion("v1.99.0") returns {1,99}, every XxxSupported()
// predicate returns true because 99 outranks every comparison, and you get a
// plausible config for a Talos that does not exist. Verified empirically.
//
// An empty imageVersion means detection failed; the guard disables rather than
// blocking an image we merely could not classify.
func CheckVersion(imageVersion string) error {
	if imageVersion == "" {
		return nil
	}
	img, err := config.ParseContractFromVersion(imageVersion)
	if err != nil {
		return fmt.Errorf("cannot parse image Talos version %q: %w", imageVersion, err)
	}
	gen, err := config.ParseContractFromVersion(GeneratorVersion())
	if err != nil {
		return fmt.Errorf("cannot parse generator version %q: %w", GeneratorVersion(), err)
	}
	if img.Greater(gen) {
		return fmt.Errorf(`image is Talos %s but this build generates configs for %s

Talos config generation is BACKWARDS compatible only, and exceeding it does not
fail loudly — it silently produces a config for a version that does not exist,
which the node then rejects or, worse, accepts and misbehaves under.

  use an image of %s or older, or rebuild tinq against machinery %s`,
			imageVersion, GeneratorVersion(), GeneratorVersion(), imageVersion)
	}
	return nil
}
```

`gendata.VersionTag` is verified: with machinery v1.13.7 linked it evaluates to `"v1.13.7"`. Do NOT hardcode a version string literal — the whole point is that the generator's version is a property of the build, not of a constant someone can forget to bump.

- [ ] **Step 5: Run tests, then mutation-sweep**

Delete `img.Greater(gen)`, the `imageVersion == ""` early return, and each error path; confirm a test fails for each. Report kill/survive.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum cluster/
git commit -m "feat(cluster): refuse images newer than the config generator"
```

---

### Task 3: Named disks and the optional data disk

**Files:**
- Modify: `cmd/tinq/main.go`, `crd/talosmachine.yaml`, `examples/bootstrap-machine.yaml`
- Test: `cmd/tinq/main_test.go`

**Interfaces:**
- Produces: `const DiskSerialSystem = "talos-system"`, `const DiskSerialData = "talos-data"`; `func specDataDisk(spec map[string]interface{}) string` returning `""` when unset.

**Why serials:** the README's `size: '> 10GB'` is a heuristic that only works while there is exactly one large disk. Adding a data disk makes it a coin flip between the OS target and the data disk — the same class of error the README warns about for `/dev/vdX`, through a different door. A serial is an identity.

- [ ] **Step 1: Write the failing test**

```go
func TestSpecDataDisk(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"set", "spec:\n  dataDisk: 40Gi\n", "40Gi"},
		{"absent", "spec:\n  disk: 20Gi\n", ""},
		{"empty", "spec:\n  dataDisk: \"\"\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var obj map[string]interface{}
			if err := yaml.Unmarshal([]byte(tc.yaml), &obj); err != nil {
				t.Fatal(err)
			}
			spec, _, _ := unstructured.NestedMap(obj, "spec")
			if got := specDataDisk(spec); got != tc.want {
				t.Errorf("specDataDisk = %q, want %q", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails.** `go test ./cmd/tinq/ -run TestSpecDataDisk -v`

- [ ] **Step 3: Implement**

Add the constants and `specDataDisk` beside the existing `specCPU`/`str`/`toInt` helpers.

In `create()`, add `serial=` to the existing system disk device, and conditionally add the data disk:

```go
	"-device", "virtio-blk-pci,drive=sys,serial=" + DiskSerialSystem + ",bootindex=0",
```

When `specDataDisk(spec) != ""`, create `data.qcow2` in the state dir (same `qemu-img` path as the system disk) and append:

```go
	args = append(args,
		"-drive", "if=none,id=data,format=qcow2,file="+dataPath,
		"-device", "virtio-blk-pci,drive=data,serial="+DiskSerialData)
```

No `bootindex` on the data disk — it must never be a boot candidate.

Add `dataDisk` to the CRD beside `disk`:

```yaml
                dataDisk:
                  type: string
                  description: >-
                    Optional second disk for PVCs, e.g. 40Gi. Selected by SERIAL
                    (talos-data), never by size — with two large disks a size
                    matcher becomes a coin flip between the OS target and this.
                    Omit for a single-disk machine with no StorageClass.
```

- [ ] **Step 4: Verify the no-dataDisk path is unchanged**

Print the QEMU args with and without `dataDisk`. Confirm that WITHOUT it the only difference from today is `serial=talos-system` on the system device — no extra `-drive`, no extra `-device`. Report both arg lists.

- [ ] **Step 5: Run full suite + cross-vet, then commit**

```bash
git add cmd/tinq/main.go crd/talosmachine.yaml examples/bootstrap-machine.yaml cmd/tinq/main_test.go
git commit -m "feat(tinq): name disks by serial and add an optional data disk"
```

---

### Task 4: Machine config generation

**Files:**
- Create: `cluster/config.go`, `cluster/config_test.go`

**Interfaces:**
- Consumes: `CheckVersion`, `GeneratorVersion`.
- Produces:

```go
type ConfigInput struct {
    ClusterName   string
    Endpoint      string // https://127.0.0.1:6443
    TalosVersion  string // from the ISO
    ConsoleArg    string // from platform.Detect().ConsoleArg
    DataDisk      bool
}

type Generated struct {
    ControlPlane []byte
    Talosconfig  []byte
    Secrets      []byte
}

func GenerateConfig(in ConfigInput) (*Generated, error)
```

Every option below corresponds to a documented failure mode. Do not drop any.

- [ ] **Step 1: Write the failing test**

```go
func TestGenerateConfigCarriesEveryLoadBearingOption(t *testing.T) {
	g, err := GenerateConfig(ConfigInput{
		ClusterName: "probe", Endpoint: "https://127.0.0.1:6443",
		TalosVersion: "v1.13.7", ConsoleArg: "console=ttyS0", DataDisk: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cp := string(g.ControlPlane)
	for _, want := range []struct{ substr, why string }{
		{"talos-system", "install target must select the system disk BY SERIAL, not by size"},
		{"installer:v1.13.7", "installer MUST be pinned to the image version or it defaults to ours"},
		{"console=ttyS0", "installed system writes its own cmdline and goes silent on serial without this"},
		{"allowSchedulingOnControlPlanes: true", "single-node cluster schedules nothing while tainted"},
		{"talos-data", "user volume must select the data disk by serial"},
	} {
		if !strings.Contains(cp, want.substr) {
			t.Errorf("controlplane.yaml missing %q\n  reason: %s", want.substr, want.why)
		}
	}
	if len(g.Talosconfig) == 0 || len(g.Secrets) == 0 {
		t.Error("talosconfig and secrets must both be produced")
	}
}

func TestGenerateConfigOmitsUserVolumeWithoutDataDisk(t *testing.T) {
	g, err := GenerateConfig(ConfigInput{
		ClusterName: "probe", Endpoint: "https://127.0.0.1:6443",
		TalosVersion: "v1.13.7", ConsoleArg: "console=ttyS0", DataDisk: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(g.ControlPlane), "talos-data") {
		t.Error("no dataDisk means no user volume; storage halves must not disagree")
	}
}

func TestGenerateConfigRejectsNewerImage(t *testing.T) {
	_, err := GenerateConfig(ConfigInput{
		ClusterName: "probe", Endpoint: "https://127.0.0.1:6443",
		TalosVersion: "v1.99.0", ConsoleArg: "console=ttyS0",
	})
	if err == nil {
		t.Fatal("must refuse an image newer than the generator")
	}
}

func TestGenerateConfigTargetsTheRequestedContract(t *testing.T) {
	old, err := GenerateConfig(ConfigInput{ClusterName: "p", Endpoint: "https://127.0.0.1:6443", TalosVersion: "v1.5.0", ConsoleArg: "console=ttyS0"})
	if err != nil {
		t.Fatal(err)
	}
	new, err := GenerateConfig(ConfigInput{ClusterName: "p", Endpoint: "https://127.0.0.1:6443", TalosVersion: "v1.13.7", ConsoleArg: "console=ttyS0"})
	if err != nil {
		t.Fatal(err)
	}
	// A 1.5 contract predates kubePrism; a 1.13 one has it. If these are equal
	// the contract is not being threaded through at all.
	if strings.Contains(string(old.ControlPlane), "kubePrism") {
		t.Error("v1.5 contract must not emit kubePrism")
	}
	if !strings.Contains(string(new.ControlPlane), "kubePrism") {
		t.Error("v1.13 contract must emit kubePrism")
	}
}
```

- [ ] **Step 2: Run to verify failure.** Expected: `undefined: GenerateConfig`.

- [ ] **Step 3: Implement**

Use `generate.NewInput(clusterName, endpoint, kubernetesVersion, opts...)` with:
- `generate.WithVersionContract(contract)` from `config.ParseContractFromVersion(in.TalosVersion)` — call `CheckVersion` FIRST and return its error.
- `generate.WithInstallImage("ghcr.io/siderolabs/installer:" + in.TalosVersion)` — pinned to the IMAGE, never to ours.
- `generate.WithInstallExtraKernelArgs([]string{in.ConsoleArg})`.
- `generate.WithAllowSchedulingOnControlPlanes(true)`.
- `generate.WithAdditionalSubjectAltNames([]string{"127.0.0.1"})`.

Then `in.Config(machine.TypeControlPlane)` → `.Bytes()`, and `in.Talosconfig()` → marshal.

The install **diskSelector by serial** and the **UserVolumeConfig** may not both be reachable through `generate.Option`. If not, apply them as config patches over the generated document, or set them on the v1alpha1 struct before marshalling. **Report which mechanism you used and why.**

Exact API shapes must be verified against the vendored source, not assumed. If a documented option does not exist under the name given, find the real one and report the difference.

- [ ] **Step 4: Run tests. Step 5: Mutation-sweep each option** (drop each `With...` in turn; the matching assertion must fail). **Step 6: Commit.**

---

### Task 5: Talos client and readiness probes

**Files:**
- Create: `cluster/client.go`, `cluster/client_test.go`

**Interfaces:**
- Produces: `func MaintenanceClient(ctx, endpoint string) (*client.Client, error)`; `func AuthenticatedClient(ctx context.Context, talosconfig []byte, endpoint string) (*client.Client, error)`; `func WaitMaintenance(ctx, endpoint string, timeout time.Duration) error`; `func WaitAPI(...) error`; `func WaitBootstrapReady(...) error`; `func WaitNodeReady(kubeconfig []byte, timeout time.Duration) error`.

**Three probes that look right and are not — encode all three:**

1. **A TCP dial to a forwarded port succeeds even when nothing listens in the guest** — QEMU accepts on the host. Probes MUST make a real Talos API call (`Version`), never a dial.
2. **`talosctl version` prints the CLIENT's tag**, not the node's — do not use a version string as a liveness signal.
3. **Bootstrap must fire while the node is `booting`, NOT `running`.** Waiting for `running` deadlocks: the node cannot reach `running` until etcd is bootstrapped. `WaitBootstrapReady` must wait for the API to answer *authenticated*, not for a `running` stage.

Maintenance mode uses `client.WithTLSConfig(&tls.Config{InsecureSkipVerify: true})` plus `client.WithEndpoints(endpoint)`. Verify the exact shape empirically against a real maintenance-mode VM — do not assume.

- [ ] **Step 1: Unit-test what is testable without a VM**

Test that `WaitMaintenance` against a listener that accepts and never responds **times out** rather than reporting success. This is the direct regression test for trap 1 and needs only a `net.Listen` stub:

```go
func TestWaitMaintenanceRejectsAnAcceptOnlyListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { for { c, err := ln.Accept(); if err != nil { return }; _ = c } }()

	start := time.Now()
	err = WaitMaintenance(context.Background(), ln.Addr().String(), 2*time.Second)
	if err == nil {
		t.Fatal("a socket that accepts but never speaks Talos must NOT count as ready — " +
			"qemu hostfwd accepts on the host even when nothing listens in the guest")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout not honoured: %v", elapsed)
	}
}
```

- [ ] **Step 2: Implement. Step 3: Verify against a REAL VM.**

Boot `talos-v1.13.7-amd64.iso` with `tinq -apply`, confirm `WaitMaintenance` returns success, then `-destroy`. Report the observed timing. Also run `talosctl get disks` if available (`sudo pacman -S talosctl` gives 1.13.7, matching) and **record whether the data disk's serial appears** — this settles the open `disk.serial` CEL question from the spec.

- [ ] **Step 4: Commit.**

---

### Task 6: Storage — local-path-provisioner

**Files:**
- Create: `cluster/storage.go`, `cluster/storage_test.go`

**Interfaces:**
- Produces: `func InstallStorage(ctx context.Context, kubeconfig []byte) error`; `const LocalPathVersion = "v0.0.31"`.

Apply via the `k8s.io/client-go` already in `go.mod` — no `kubectl`, `kustomize` or `helm` on the host. Embed the manifest with `go:embed` (pinned, offline, auditable) rather than fetching at runtime.

**The three Talos-specific patches, each of which is load-bearing:**

1. root path `/opt/local-path-provisioner` → `/var/mnt/local-path-provisioner`. **Talos's root filesystem is read-only**; `/opt` is not writable and the stock manifest simply fails. `/var` is the EPHEMERAL partition.
2. `storageclass.kubernetes.io/is-default-class: "true"` so an ordinary PVC with no `storageClassName` binds.
3. namespace `local-path-storage` labelled `pod-security.kubernetes.io/enforce: privileged` — Talos enforces PodSecurity at `baseline` and the provisioner's helper pod uses `hostPath`.

- [ ] **Step 1: Test the manifest content before any cluster exists**

Assert the embedded manifest contains all three patches, and that the ConfigMap path is `/var/mnt/local-path-provisioner` and NOT `/opt/...`. Mutation-verify: revert the path to `/opt` and the test must fail.

- [ ] **Step 2: Implement. Step 3: Verify against a real cluster** in Task 8. **Step 4: Commit.**

---

### Task 7: The `-up` orchestration

**Files:**
- Create: `cluster/up.go`
- Modify: `cmd/tinq/main.go`

**Interfaces:**
- Produces: `func Up(ctx context.Context, opts UpOptions) error`, driving all 10 steps and owning all output.

Ten steps exactly as the spec's transcript. Step 10 and the step-6 `UserVolumeConfig` are BOTH gated on `dataDisk`, so the storage halves cannot disagree.

Artifacts to the state dir at mode `0600`: `talosconfig`, `controlplane.yaml`, `kubeconfig`, `secrets.yaml`. Print the `export` lines at the end.

**Output is the feature, not decoration.** Each step names the operation; the non-obvious ones name the reason. Do not silently handle the diskSelector, installer pin, console arg, or bootstrap timing — announce each.

Print the three hardened-default notes at the end: taint removed and why; PodSecurity still enforced and how to label a namespace; storage installed (or absent) and that PVC data does not survive `-destroy`.

- [ ] **Step 1: Wire `-up` beside `-apply`/`-destroy`.** `-apply` and `-destroy` must be untouched.
- [ ] **Step 2: Implement the step sequence.**
- [ ] **Step 3: Verify `-destroy` still works with no cluster and no accelerator.**
- [ ] **Step 4: Commit.**

---

### Task 8: End-to-end verification and README

**Files:** `README.md`

- [ ] **Step 1: Full bring-up on the real host**

```bash
go run ./cmd/tinq -up /tmp/tinq-cluster.yaml   # with dataDisk: 40Gi
```

Report the complete step transcript.

- [ ] **Step 2: Prove the cluster works**

```bash
export KUBECONFIG=~/.hvf/<site>/<uid>/kubeconfig
kubectl get nodes                     # must be Ready
kubectl get storageclass              # local-path must be default
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: probe }
spec:
  accessModes: [ReadWriteOnce]
  resources: { requests: { storage: 1Gi } }
EOF
kubectl get pvc probe -w              # must BIND, not hang Pending
kubectl create deployment web --image=nginx
kubectl wait --for=condition=available deployment/web --timeout=180s
```

A PVC that stays Pending, or a Deployment that never schedules, is a FAILURE — those are the exact two symptoms the taint and StorageClass decisions exist to prevent. Report actual output.

- [ ] **Step 3: Confirm the install landed on the right disk**

`talosctl -n 127.0.0.1 get disks` — confirm the OS installed to `talos-system` and the user volume is on `talos-data`. This is the payoff for serial-naming; report the actual table.

- [ ] **Step 4: Tear down and prove nothing leaks**

`-destroy`, then `ls ~/.hvf/` shows only `images`, `pgrep qemu-system` empty, ports free. `~/.hvf/images/` must NOT be deleted.

- [ ] **Step 5: README**

Document `-up`, `dataDisk`, and the three hardened defaults. Replace the "**No one-command cluster**" Status bullet. **Correct the "Newer ISOs may not boot" bullet** — v1.13.7 is verified to boot on Linux/KVM; that observation was macOS/HVF/aarch64-specific. State plainly that macOS is unverified on this branch.

- [ ] **Step 6: Commit.**

---

## Self-Review

**Spec coverage:** version guard → T2; ISO version detection → T1; named disks + `dataDisk` → T3; config generation with all five load-bearing options → T4; the three readiness traps → T5; local-path with Talos's three patches → T6; 10-step transparent output + artifact layout → T7; end-to-end proof + README → T8. The `disk.serial` CEL open question is resolved in T5 step 3.

**Placeholder scan:** no TBD/TODO-as-instruction. Two tasks (T4 step 3, T5) explicitly instruct the implementer to verify API shapes against vendored source rather than trust this plan — that is deliberate, because machinery's exact option names were read from `go doc` and not exercised.

**Type consistency:** `ConfigInput`/`Generated` (T4) are consumed by T7. `DiskSerialSystem`/`DiskSerialData` (T3) are consumed by T4's selectors and T8's verification. `CheckVersion`/`GeneratorVersion` (T2) are consumed by T4. `InspectImageVersion` (T1) is consumed by T2's caller in T7.

**Known weakness:** T5, T6 and T7 cannot be fully unit-tested without a VM, so their real proof is T8. That is why T8's steps assert on cluster behaviour (PVC binds, Deployment schedules) rather than on exit codes.
