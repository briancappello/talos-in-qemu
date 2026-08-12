# Baremetal Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `cluster/` describe a Talos node rather than a QEMU guest, and add `tinq adopt` to bring up hardware this tool did not create.

**Architecture:** `cluster/` stops importing `platform` entirely — every host-derived value (console arg, kexec decision, version, transcript line) becomes an explicit `UpOptions` field filled by `cmd/tinq`. The certificate SAN is derived from `TalosEndpoint` so the two cannot drift. A new `adopt` verb resolves the node's version and disks over the maintenance API before entering the same ten-step sequence `up` uses.

**Tech Stack:** Go 1.26.5, `siderolabs/talos/pkg/machinery` v1.13.7, `cosi-project/runtime` v1.14.1, cobra, QEMU.

**Design spec:** `docs/superpowers/specs/2026-08-02-baremetal-foundation-design.md` (commit `731f859`). Decisions are referenced below as D1–D7.

**Nine tasks, strictly sequential.** Task 8 was added during Task 1's review: it is a Tier 1 instance of the very defect class this branch removes, found in `cmd/tinq`. The live gate is Task 9.

## Global Constraints

- **Go 1.26.5.** `new(false)` / `new(true)` (generic `new` with a value) is used in `config.go` and is valid — match that idiom, do not introduce `ptr()` helpers.
- **Secrets are never logged and never placed in an error.** `talosconfig`, `kubeconfig` and `secrets.yaml` are secret. Errors that could quote them use `errSecretParse` (`cluster/client.go:77`). Nothing in this plan may relax that.
- **`cluster/client.go` header rule (2): no probe in that file may compare, return or log a version.** New version-reading code goes in `cluster/nodeinfo.go`, never in `client.go`. See Task 4.
- **`cluster/` must not import `github.com/coglative/talos-in-qemu/platform` after Task 3.** That import is the thing being deleted.
- **Bootstrap only.** No upgrade, no scaling, no steady-state reconcile (`cluster/version.go:1-7`).
- **Every existing QEMU behaviour is a regression target.** The generated config for a QEMU machine must be byte-identical before and after Tasks 1–3, except where a test says otherwise.
- **Run `go build ./... && go vet ./... && go test ./...` before every commit.**

## File Structure

| File | Change | Responsibility |
|---|---|---|
| `cluster/config.go` | Modify | `ConfigInput.APIAddress`; delete `const loopback`; optional console arg; gate the UKI patch |
| `cluster/up.go` | Modify | `UpOptions` restructure; drop `platform` import; steps 1–3 read explicit fields |
| `cluster/nodeinfo.go` | **Create** | Ask a maintenance-mode node for facts: version, disks. NOT probes |
| `cluster/client.go` | Modify | Endpoint error text stops saying "qemu forward" |
| `cmd/tinq/main.go` | Modify | Compute node facts for QEMU; `adopt` verb; four verb refusals; `spec.baremetal` |
| `crd/talosmachine.yaml` | Modify | `baremetal` block; relax `required`; CEL validation |
| `README.md` | Modify | Document `adopt` |

Tests live beside their subjects in the existing `_test.go` files.

---

### Task 1: Derive the certificate SAN from the endpoint (D2)

**Files:**
- Modify: `cluster/config.go:75-78` (delete `loopback` and its comment), `cluster/config.go:32-59` (add field), `cluster/config.go:165-168`
- Modify: `cluster/up.go:461-469` (fill the field)
- Test: `cluster/config_test.go:85-95` (`testInput`), `cluster/config_test.go:386-398` (replace `TestGenerateConfigAddsLoopbackToMachineCertSANs`)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `ConfigInput.APIAddress string`; `cluster.apiAddress(endpoint string) (string, error)` in `up.go`.

**Existing test helpers — use these, do not invent new ones.** `cluster/config_test.go` already provides `testInput() ConfigInput` (:85), `mustGenerate(t, in) *Generated` (:96), `mustGenerateDefault(t) *Generated` (:112, shares one CA set), `redactErr(err) string` (:73), and `v1alpha1Doc(t, cp) string` (:176). Typed assertions use `configloader.NewFromBytes`, as at :387.

- [ ] **Step 1: Write the failing test**

First add the field to the shared fixture, `cluster/config_test.go:85` `testInput()` — every other test depends on it and Task 1's refusal makes an unset value fatal:

```go
		APIAddress:       "127.0.0.1",
```

Then **replace** `TestGenerateConfigAddsLoopbackToMachineCertSANs` (:386-398) entirely:

```go
func TestCertSANComesFromAPIAddress(t *testing.T) {
	// Both arms matter. 127.0.0.1 is the QEMU regression — the generated config
	// must not change. 192.168.1.50 is the one that was IMPOSSIBLE before this
	// task and is the whole reason for it.
	//
	// mustGenerate rather than mustGenerateDefault: a non-default input cannot
	// use the shared CA set, so this pays for two full generations. That is the
	// price of proving the address is threaded rather than hardcoded.
	for _, addr := range []string{"127.0.0.1", "192.168.1.50"} {
		in := testInput()
		in.APIAddress = addr

		cfg, err := configloader.NewFromBytes(mustGenerate(t, in).ControlPlane)
		if err != nil {
			t.Fatalf("generated config does not parse: %s", redactErr(err))
		}

		// Asserted through the TYPED API on purpose, exactly as the test this
		// replaces did: the address also appears under apiServer.certSANs,
		// where the endpoint puts it for free, so a substring match would pass
		// with the machine SAN missing entirely.
		if sans := cfg.Machine().Security().CertSANs(); !slices.Contains(sans, addr) {
			t.Errorf("machine certSANs = %v, want %s\n"+
				"  reason: the cert must name the address the CLIENT DIALS, or every "+
				"authenticated call fails the TLS handshake", sans, addr)
		}
	}
}

func TestGenerateConfigRefusesAnEmptyAPIAddress(t *testing.T) {
	in := testInput()
	in.APIAddress = ""

	if _, err := GenerateConfig(in); err == nil {
		t.Error("GenerateConfig accepted an empty APIAddress\n" +
			"  reason: an empty SAN list yields a cert naming nothing, which fails at " +
			"the handshake minutes later rather than here")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cluster -run 'TestCertSAN|TestGenerateConfigRefusesAnEmpty' -v`
Expected: FAIL — `in.APIAddress undefined (type ConfigInput has no field or method APIAddress)`.

- [ ] **Step 3: Add the field and delete the constant**

In `cluster/config.go`, delete lines 75-78 (the `loopback` const and its comment). Add to `ConfigInput`, after `Endpoint`:

```go
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
```

- [ ] **Step 4: Refuse an empty address, and use it**

In `GenerateConfig`, immediately after the `version == ""` refusal (config.go:118-120), add:

```go
	// Refused here rather than at the handshake. An empty SAN list produces a
	// certificate that names nothing; the node installs, boots, serves apid,
	// and every authenticated call then fails minutes later with an error
	// about certificates and nothing pointing at this field.
	if in.APIAddress == "" {
		return nil, errors.New("no API address: this is the address a client dials to reach " +
			"the node, and it must be in apid's certificate or no authenticated call can " +
			"ever succeed")
	}
```

Add `"errors"` to the import block. Then replace config.go:165-168:

```go
		// apid is dialled at THIS address, which must therefore be in its
		// certificate. Derived from the endpoint by the caller so the two
		// cannot disagree.
		generate.WithAdditionalSubjectAltNames([]string{in.APIAddress}),
		generate.WithEndpointList([]string{in.APIAddress}),
```

- [ ] **Step 5: Fill it from the endpoint in `up.go`**

Add to `cluster/up.go` (after `took`, near the other small helpers):

```go
// apiAddress is the host part of a host:port endpoint.
//
// This is the ONE place the certificate's subject alt name is decided, and it
// is decided BY the endpoint rather than beside it: apid's cert has to name
// whatever a client dials, and TalosEndpoint is what a client dials. A second
// configurable field would compile, read correctly, and be settable to
// something the client never contacts — which surfaces as a TLS failure on
// every authenticated call, minutes into a bring-up.
func apiAddress(endpoint string) (string, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("the Talos endpoint %q is not host:port: %w", endpoint, err)
	}

	if host == "" {
		return "", fmt.Errorf("the Talos endpoint %q has no host part, so apid's certificate "+
			"would name nothing", endpoint)
	}

	return host, nil
}
```

Add `"net"` to `up.go`'s imports. In `configure`, before the `hooks.generateConfig` call:

```go
	addr, err := apiAddress(opts.TalosEndpoint)
	if err != nil {
		return nil, err
	}
```

and add `APIAddress: addr,` to the `ConfigInput` literal.

- [ ] **Step 6: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS. Any other test asserting the literal `127.0.0.1` in a generated config (e.g. `config_test.go:551`) still passes, because `testInput()` should now set `APIAddress: "127.0.0.1"` — add that to the helper if it is not inherited.

- [ ] **Step 7: Commit**

```bash
git add cluster/config.go cluster/up.go cluster/config_test.go
git commit -m "cluster: the cert SAN is derived from the endpoint, not written beside it"
```

---

### Task 2: The console argument becomes optional (D5)

**Files:**
- Modify: `cluster/config.go:152-172` (option list), `cluster/config.go:188-199` (UKI patch)
- Test: `cluster/config_test.go`

**Interfaces:**
- Consumes: `ConfigInput.APIAddress` from Task 1.
- Produces: `ConfigInput.ConsoleArg == ""` now means "emit no console kernel argument and leave `InstallGrubUseUKICmdline` alone".

- [ ] **Step 1: Write the failing test**

**Assert against `v1alpha1Doc`, never against the raw `ControlPlane` bytes.** `v1alpha1Doc` (:176) runs `code()`, which strips comments — and machinery's encoder emits `# grubUseUKICmdline: true` as a **commented-out example** (see the fixture at config_test.go:152). A `strings.Contains` on the raw bytes matches that comment and reports a field that was never set. Follow the existing idiom at :331-341, which uses anchored regexps on the stripped doc.

```go
func TestEmptyConsoleArgEmitsNoKernelArgsAndLeavesUKIAlone(t *testing.T) {
	in := testInput()
	in.ConsoleArg = ""

	doc := v1alpha1Doc(t, mustGenerate(t, in).ControlPlane)

	if regexp.MustCompile(`(?m)^ {8}extraKernelArgs:`).MatchString(doc) {
		t.Errorf("install emitted extraKernelArgs with no console arg set\n"+
			"  reason: real hardware has a firmware-configured console; forcing one "+
			"derived from the HOST's architecture is how a node boots with a dead "+
			"console\n%s", redact(doc))
	}

	// The UKI switch exists ONLY to stop GRUB ignoring extraKernelArgs. With no
	// extraKernelArgs there is nothing to stop, and flipping it anyway changes a
	// node's boot path for no reason. Anchored to `: false` because the absent
	// case and the explicitly-false case are different facts.
	if regexp.MustCompile(`(?m)^ {8}grubUseUKICmdline: false`).MatchString(doc) {
		t.Errorf("grubUseUKICmdline was forced false with no console arg to protect\n"+
			"  reason: its only purpose is that GRUB's UKI cmdline and extraKernelArgs "+
			"cannot coexist — with no extraKernelArgs there is no conflict\n%s", redact(doc))
	}
}

```

**Do NOT add a console-arg-set test.** The console-set regression is already carried by `TestGenerateConfigCarriesConsoleArgToTheInstalledSystem` (config_test.go:331-344), and it is genuinely load-bearing: `mustGenerateDefault` generates from `testInput()`, which sets `ConsoleArg: "console=ttyS0"`, so that test runs on exactly the input a console-set test would use and dies under the same mutants (conditional wired backwards, UKI gate inverted).

*An earlier revision of this plan mandated such a test with a comment claiming the pre-existing one "would keep passing even if the conditional were wired backwards." That was false — the two would have been byte-identical regexps over identical input — and it cost a full five-CA generation in a package whose own comment (config_test.go:108-110) says config generation dominates its runtime.*

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cluster -run 'TestEmptyConsoleArg|TestConsoleArgStill' -v`
Expected: FAIL — `extraKernelArgs = [""], want none` (the current code passes a slice containing the empty string).

- [ ] **Step 3: Make the option conditional**

In `cluster/config.go`, replace the single `generate.NewInput(...)` call with a built slice. Remove the `WithInstallExtraKernelArgs` line from the literal and append it conditionally:

```go
	genOpts := []generate.Option{
		// Without a contract every version-gated default is generated for the
		// machinery's own version instead of the image's.
		generate.WithVersionContract(contract),
		// Pinned to the IMAGE. Left unset, Talos substitutes the generator's
		// version and a fresh install silently becomes a cross-version upgrade.
		generate.WithInstallImage("ghcr.io/siderolabs/installer:" + version),
		// A topology correction, not a security weakening: with the
		// control-plane taint in place a single-node cluster schedules nothing.
		generate.WithAllowSchedulingOnControlPlanes(true),
		// apid is dialled at THIS address, which must therefore be in its
		// certificate. Derived from the endpoint by the caller so the two
		// cannot disagree.
		generate.WithAdditionalSubjectAltNames([]string{in.APIAddress}),
		generate.WithEndpointList([]string{in.APIAddress}),
	}

	// OPTIONAL, and empty is a real answer rather than a missing one. Under
	// QEMU the console is the only way to watch a boot, and the installed
	// system inherits nothing from the ISO — so it must be named. On hardware
	// the firmware has already configured a console and there is usually a
	// display, so naming one derived from THIS laptop's architecture is not a
	// default, it is a guess that boots the node with a dead console.
	if in.ConsoleArg != "" {
		genOpts = append(genOpts, generate.WithInstallExtraKernelArgs([]string{in.ConsoleArg}))
	}

	input, err := generate.NewInput(in.ClusterName, in.Endpoint, k8sVersion, genOpts...)
```

- [ ] **Step 4: Gate the UKI patch on the console arg**

In the `PatchV1Alpha1` closure, change the condition at config.go:197:

```go
		// GATED ON A CONSOLE ARG ACTUALLY BEING PASSED. A 1.12+ contract turns
		// grubUseUKICmdline ON, which makes GRUB take its cmdline from the
		// installer's UKI and IGNORE extraKernelArgs — machinery rejects the two
		// together, so a config carrying a console arg does not even validate in
		// metal mode. Talos 1.8 dropped console=ttyS0 from the metal image's own
		// defaults (imager/quirks), so the arg has to come from here and the UKI
		// cmdline has to yield.
		//
		// With NO console arg there is no conflict to resolve, and switching a
		// node's boot path off the UKI cmdline anyway is a change made for
		// nothing. Only touched when machinery set it: the field is unknown to
		// older Talos, and the contract exists to avoid emitting fields a node
		// cannot parse.
		if in.ConsoleArg != "" && c.MachineConfig.MachineInstall.InstallGrubUseUKICmdline != nil {
			c.MachineConfig.MachineInstall.InstallGrubUseUKICmdline = new(false)
		}
```

- [ ] **Step 5: Run the full suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS. `testInput()` sets a non-empty `ConsoleArg`, so every existing assertion about `console=ttyS0` is unaffected.

- [ ] **Step 6: Commit**

```bash
git add cluster/config.go cluster/config_test.go
git commit -m "cluster: no console arg is a real answer, and the UKI switch follows it"
```

---

### Task 3: `cluster/` stops importing `platform` (D1, D6)

This is the largest task and the one that makes every later one possible.

**Files:**
- Modify: `cluster/up.go:12` (import), `:76-128` (`UpOptions`, `upHooks`), `:130-142` (`realHooks`), `:186-210` (steps 1–2), `:441-469` (`configure`)
- Modify: `cmd/tinq/main.go:381-424` (`upOptions`)
- Test: `cluster/up_test.go`, `cmd/tinq/main_test.go`

**Interfaces:**
- Consumes: `apiAddress` (Task 1), optional `ConsoleArg` (Task 2).
- Produces: `UpOptions` without `ImagePath` / `Detect`, with `TalosVersion`, `VersionSource`, `Substrate`, `ConsoleArg`, `DisableKexec`. Every later task builds an `UpOptions`.

- [ ] **Step 1: Write the failing test**

The fixture is `newFixture(t)` (`cluster/up_test.go:201`), returning `*upFixture` with fields `opts`, `rec`, `out`, `dir`, `booted`, and methods `run(t)` / `mustRun(t) string`. **The fixture itself must change in this task**: `newFixture` currently sets `ImagePath` and `Detect` (:212, :219), both of which this task deletes, and `f.rec` is a `recorder` carrying `imageVersion` for the `detectVersion` hook that also goes away. Update the fixture to set the five new fields instead — that edit is part of Step 3, not optional cleanup.

```go
func TestUpRendersSubstrateAndVersionFromOptions(t *testing.T) {
	f := newFixture(t)
	f.opts.Substrate = "baremetal, 192.168.1.50"
	f.opts.TalosVersion = imageTalosVersion
	f.opts.VersionSource = "the node's maintenance API"

	out := f.mustRun(t)

	if !strings.Contains(out, "baremetal, 192.168.1.50") {
		t.Errorf("step 1 did not print the caller's substrate line\n"+
			"  reason: cluster/ no longer knows what a hypervisor is, so an "+
			"accelerator and an emulator binary cannot come from here\n%s", redact(out))
	}

	if !strings.Contains(out, "the node's maintenance API -> "+imageTalosVersion) {
		t.Errorf("step 2 did not print the caller's version source\n"+
			"  reason: a baremetal node has no ISO to read a volume id from\n%s", redact(out))
	}
}
```

Do **not** write a Go test asserting the absence of the `platform` import — a test cannot see its own package's import graph. Step 6 checks it with `go list`, which can.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cluster -run TestUpRendersSubstrateAndVersion -v`
Expected: FAIL — `f.opts.Substrate undefined`.

- [ ] **Step 3: Restructure `UpOptions` and `upHooks`**

In `cluster/up.go`, delete the `platform` import (line 12). Delete `ImagePath` and `Detect` from `UpOptions`. Add:

```go
	// TalosVersion is the node's Talos version, e.g. "v1.13.7". RESOLVED BY THE
	// CALLER, and that is what lets one sequence serve two substrates: a QEMU
	// bring-up reads it from the ISO's volume id before booting anything, and
	// an adopted node is asked directly because it is already running. Empty is
	// a real state and step 3 refuses it — see errUnknownTalosVersion.
	TalosVersion string
	// VersionSource says WHERE TalosVersion came from, for the transcript only.
	// "talos-v1.13.7-amd64.iso (ISO volume id)", or "the node's maintenance API".
	VersionSource string
	// Substrate is step 1's line, rendered by the caller. This package no
	// longer knows what a hypervisor is, and an accelerator or an emulator
	// binary is meaningless for a machine that is a machine.
	Substrate string
	// ConsoleArg is the console kernel argument for the NODE, or "" for none.
	//
	// It was derived from the HOST's architecture, which is sound only because
	// QEMU makes host arch and guest arch the same by construction. Driving a
	// node from a different machine breaks that identity, and nothing in the
	// type system noticed.
	ConsoleArg string
	// DisableKexec asks the node not to kexec on reboot. It exists for ONE
	// substrate — QEMU on macOS/arm64 — and the caller decides, because whether
	// the workaround applies is a fact about the host, which this package no
	// longer holds.
	DisableKexec bool
```

Remove `detectVersion` from `upHooks` and from `realHooks`.

- [ ] **Step 4: Rewrite steps 1–3 and `configure`**

Replace up.go:188-210 with:

```go
	// ── 1/10 platform ───────────────────────────────────────────────────────
	//
	// Rendered by the CALLER. This package no longer resolves host facts, and
	// the line differs by substrate: a hypervisor, an accelerator and an
	// emulator binary describe a QEMU guest and describe nothing at all about a
	// machine on a desk.
	p.step("platform", "%s", opts.Substrate)

	// ── 2/10 version ────────────────────────────────────────────────────────
	// Empty is a real state — an unclassifiable ISO and a node that reports no
	// tag both produce it — and it has to be printed as one rather than as an
	// empty version. Step 3 is what refuses it.
	shown := opts.TalosVersion
	if shown == "" {
		shown = "UNKNOWN"
	}

	p.step("version", "%s -> %s", opts.VersionSource, shown)
```

Then throughout steps 3–7, replace `imageVersion` with `opts.TalosVersion`. Change `configure`'s signature to drop both trailing parameters:

```go
func configure(ctx context.Context, hooks *upHooks, opts UpOptions, p *printer) ([]byte, error) {
```

Inside it, replace the `disableKexec` computation (up.go:443-459) with a comment pointing at the caller, and use `opts.DisableKexec`, `opts.ConsoleArg`, `opts.TalosVersion` in the `ConfigInput` literal and in every `p.detail` line that referenced `host.*` or `imageVersion`. Update the call site at up.go:343 to `configure(ctx, hooks, opts, p)`.

- [ ] **Step 5: Fill the new fields in `cmd/tinq`**

In `cmd/tinq/main.go`, `upOptions` — after `resolveImage`:

```go
	// Host facts resolved HERE, because this is the layer that owns QEMU. The
	// three values below are the node facts they imply, and the implication is
	// only valid for a guest: the README requires the image architecture to
	// match the host, which is what makes the host's console argument the
	// guest's console argument. Nothing outside this function may assume it.
	host, err := d.detect()
	if err != nil {
		return cluster.UpOptions{}, err
	}
```

and in the returned literal, replace `ImagePath` and `Detect` with:

```go
		TalosVersion:  platform.InspectImageVersion(image),
		VersionSource: fmt.Sprintf("%s (ISO volume id)", filepath.Base(image)),
		Substrate:     fmt.Sprintf("%s/%s, %s, %s", host.OS, host.ImageArch, host.Accel, host.QEMUBinary),
		ConsoleArg:    host.ConsoleArg,
		// KEXEC IS DISABLED ON macOS/arm64 ONLY. Talos kexecs straight into the
		// kernel it just installed; under QEMU on macOS that path dies in the
		// guest on arm64 and the node never boots what it installed. Elsewhere
		// it works and it is FASTER, so disabling it more widely is a tax paid
		// for another platform's bug. Upstream gates its own workaround on the
		// target ARCHITECTURE, so an Intel Mac has nothing to work around.
		DisableKexec: host.OS == "darwin" && host.ImageArch == "arm64",
```

- [ ] **Step 6: Verify the import is actually gone**

Run: `go list -deps ./cluster | grep talos-in-qemu/platform && echo "STILL IMPORTED — FAIL" || echo "clean"`
Expected: `clean`.

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cluster/up.go cluster/up_test.go cmd/tinq/main.go cmd/tinq/main_test.go
git commit -m "cluster: node facts are given, not derived from the host"
```

---

### Task 4: Ask a node its Talos version (D1)

**Files:**
- Create: `cluster/nodeinfo.go`, `cluster/nodeinfo_test.go`

**Interfaces:**
- Consumes: `MaintenanceClient` (`cluster/client.go:89`).
- Produces: `cluster.NodeVersion(ctx context.Context, endpoint string) (string, error)`.

**Why a new file.** `client.go`'s header rule (2) forbids any probe there from comparing, returning or logging a version, because a version string is not a liveness signal. This function is not a probe — it is asked *after* readiness is established, and its answer becomes the installer image tag written to the node's disk. Putting it in `client.go` would sit directly under a comment saying it must not exist.

- [ ] **Step 1: Write the failing test**

```go
package cluster

import (
	"strings"
	"testing"
)

func TestNodeVersionRefusesAnEmptyEndpoint(t *testing.T) {
	_, err := NodeVersion(t.Context(), "")
	if err == nil {
		t.Fatal("NodeVersion accepted an empty endpoint\n" +
			"  reason: an empty address spends the caller's whole timeout proving " +
			"that \"\" is not an address")
	}

	if !strings.Contains(err.Error(), "host:port") {
		t.Errorf("error does not say what shape an endpoint has: %s", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cluster -run TestNodeVersion -v`
Expected: FAIL — `undefined: NodeVersion`.

- [ ] **Step 3: Write the implementation**

Create `cluster/nodeinfo.go`:

```go
package cluster

import (
	"context"
	"fmt"
)

// NODE FACTS, NOT PROBES. Everything in this file asks a maintenance-mode node
// a QUESTION and returns the answer. Nothing here decides whether a node is
// ready, and nothing here may be used to.
//
// That distinction is why this file exists at all rather than living in
// client.go, whose header rule (2) forbids any probe from comparing, returning
// or logging a version — because `talosctl version` prints a constant compiled
// into the binary and will do so with no node in sight. These functions run
// AFTER readiness has been established by a real round trip, and their answers
// become values written to the node's disk.

// NodeVersion asks a maintenance-mode node for its own Talos version.
//
// It NEVER errors on an unidentifiable version, only on a failed call: "" is a
// real answer and matches platform.InspectImageVersion's contract, so both
// sources of a version fail the same way and step 3's guard is the single place
// that refuses one. Returning an error here instead would put the refusal in
// two places and let them drift.
func NodeVersion(ctx context.Context, endpoint string) (string, error) {
	c, err := MaintenanceClient(ctx, endpoint)
	if err != nil {
		return "", err
	}

	defer c.Close() //nolint:errcheck

	resp, err := c.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("asking the node its Talos version: %w", err)
	}

	// One node, so one message — but ranging costs nothing and a nil Messages
	// slice is a real reply shape rather than a panic.
	for _, m := range resp.GetMessages() {
		if tag := m.GetVersion().GetTag(); tag != "" {
			return tag, nil
		}
	}

	return "", nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cluster -run TestNodeVersion -v`
Expected: PASS. `MaintenanceClient` already refuses `""` via `errNoEndpoint` (`client.go:84`), whose text contains `host:port`.

- [ ] **Step 5: Extend the real-node test**

In `cluster/client_test.go`, inside `TestAgainstARealNode` (after the existing maintenance assertions), add:

```go
	// RISK 1 FROM THE DESIGN SPEC, resolved against a live node: whether a
	// maintenance-mode node reports a populated version tag before any config
	// is applied. If this ever fails, spec.baremetal.talosVersion is the
	// documented fallback.
	version, err := NodeVersion(ctx, endpoint)
	if err != nil {
		t.Fatalf("NodeVersion: %s", redactErr(err))
	}

	if version == "" {
		t.Error("a maintenance-mode node reported no Talos version tag\n" +
			"  reason: adopt pins the installer image to this value; with no tag it " +
			"must fall back to spec.baremetal.talosVersion")
	}

	t.Logf("the node reports Talos %s", version)
```

- [ ] **Step 6: Commit**

```bash
git add cluster/nodeinfo.go cluster/nodeinfo_test.go cluster/client_test.go
git commit -m "cluster: ask a node its Talos version, where that is not a probe"
```

---

### Task 5: List a node's disks, and refuse without a serial (D7)

**Files:**
- Modify: `cluster/nodeinfo.go`, `cluster/nodeinfo_test.go`

**Interfaces:**
- Consumes: `MaintenanceClient`, and the COSI call proven at `cluster/client_test.go:847`.
- Produces: `cluster.Disk` struct; `cluster.ListDisks(ctx, endpoint) ([]Disk, error)`; `cluster.FormatDisks([]Disk) string`; `cluster.RequireDisk(disks []Disk, serial, what string) error`.

- [ ] **Step 1: Write the failing test**

```go
func testDisks() []Disk {
	return []Disk{
		{ID: "sda", Serial: "S1", Model: "Samsung SSD", Size: "500 GB", Transport: "sata"},
		{ID: "sdb", Serial: "", Model: "SanDisk Cruzer", Size: "32 GB", Transport: "usb", Readonly: true},
	}
}

func TestRequireDiskRefusesAnEmptySerialAndShowsTheTable(t *testing.T) {
	err := RequireDisk(testDisks(), "", "install target")
	if err == nil {
		t.Fatal("RequireDisk accepted an empty serial\n" +
			"  reason: nothing may install until a human has chosen a disk")
	}

	for _, want := range []string{"S1", "Samsung SSD", "500 GB", "SanDisk Cruzer", "readonly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not show %q:\n%s\n"+
				"  reason: the table IS the remedy — without it there is no way to "+
				"learn a serial without talosctl", want, err)
		}
	}
}

func TestRequireDiskRefusesAnUnmatchedSerialAsATypo(t *testing.T) {
	err := RequireDisk(testDisks(), "S9", "install target")
	if err == nil {
		t.Fatal("RequireDisk accepted a serial matching no disk\n" +
			"  reason: this is the realistic failure — a typo installs nowhere, and " +
			"Talos reports it as a hang")
	}

	if !strings.Contains(err.Error(), "S9") {
		t.Errorf("the refusal does not quote the serial that matched nothing: %s", err)
	}
}

func TestRequireDiskAcceptsAMatch(t *testing.T) {
	if err := RequireDisk(testDisks(), "S1", "install target"); err != nil {
		t.Fatalf("RequireDisk rejected a serial that matches: %s", err)
	}
}

// The boot medium is identified by READONLY, not CDROM: client_test.go:929-935
// records that a Talos ISO presents as a read-only virtio-blk device, and so
// does the squashfs loop device. A table flagging only cdrom shows the stick
// you booted from as an ordinary candidate.
func TestFormatDisksFlagsReadonlyNotJustCDROM(t *testing.T) {
	out := FormatDisks([]Disk{{ID: "sdb", Serial: "S2", Readonly: true}})
	if !strings.Contains(out, "readonly") {
		t.Errorf("readonly is not flagged:\n%s", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cluster -run 'TestRequireDisk|TestFormatDisks' -v`
Expected: FAIL — `undefined: Disk`, `undefined: RequireDisk`, `undefined: FormatDisks`.

- [ ] **Step 3: Write the implementation**

Append to `cluster/nodeinfo.go`, and add `"sort"`, `"strings"`, `"github.com/cosi-project/runtime/pkg/safe"` and `blockres "github.com/siderolabs/talos/pkg/machinery/resources/block"` to its imports:

```go
// Disk is one of a node's disks, reduced to what choosing an install target
// needs. It is a struct of our own rather than machinery's DiskSpec so the
// table below cannot drift with a field we never render.
type Disk struct {
	ID         string
	Serial     string
	Model      string
	Size       string
	Transport  string
	WWID       string
	Rotational bool
	Readonly   bool
	CDROM      bool
}

// ListDisks asks a maintenance-mode node what disks it has.
//
// Same COSI call TestAgainstARealNode has made against real hardware since the
// bring-up branch (client_test.go:847); this is that call given an exported
// caller, not new capability.
func ListDisks(ctx context.Context, endpoint string) ([]Disk, error) {
	c, err := MaintenanceClient(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	defer c.Close() //nolint:errcheck

	list, err := safe.StateListAll[*blockres.Disk](ctx, c.COSI)
	if err != nil {
		return nil, fmt.Errorf("listing the node's disks: %w", err)
	}

	out := make([]Disk, 0, list.Len())

	for d := range list.All() {
		s := d.TypedSpec()
		out = append(out, Disk{
			ID: d.Metadata().ID(), Serial: s.Serial, Model: s.Model,
			Size: s.PrettySize, Transport: s.Transport, WWID: s.WWID,
			Rotational: s.Rotational, Readonly: s.Readonly, CDROM: s.CDROM,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return out, nil
}

// FormatDisks renders the table that is the REMEDY for both refusals below.
// Without talosctl there is no other way to learn a serial, so this is not
// diagnostic decoration — it is the only path forward.
func FormatDisks(disks []Disk) string {
	var b strings.Builder

	fmt.Fprintf(&b, "  %-8s %-24s %-22s %-10s %s\n", "DEVICE", "SERIAL", "MODEL", "SIZE", "NOTES")

	for _, d := range disks {
		var notes []string
		// READONLY FIRST, and it is the one that matters: the medium you booted
		// from presents as a read-only virtio-blk device rather than a cdrom.
		if d.Readonly {
			notes = append(notes, "readonly — probably the medium you booted from")
		}
		if d.CDROM {
			notes = append(notes, "cdrom")
		}
		if d.Rotational {
			notes = append(notes, "rotational")
		}
		if d.Transport != "" {
			notes = append(notes, d.Transport)
		}
		if d.Serial == "" && d.WWID != "" {
			notes = append(notes, "no serial; wwid "+d.WWID)
		}

		serial := d.Serial
		if serial == "" {
			serial = "(none)"
		}

		fmt.Fprintf(&b, "  %-8s %-24s %-22s %-10s %s\n",
			d.ID, serial, d.Model, d.Size, strings.Join(notes, ", "))
	}

	return b.String()
}

// RequireDisk refuses unless serial names a disk this node actually has.
//
// TWO refusals, ONE table, because they are the same remedy. The empty case is
// a first run. The unmatched case is a TYPO, which is the realistic failure and
// the expensive one: Talos with a selector matching nothing installs nowhere
// and reports it as a hang, with nothing pointing at a mistyped serial.
//
// Auto-selecting by size was rejected — config.go already calls that "a coin
// flip once there are two large disks", and on hardware the losing side
// overwrites a disk that may hold data, which is the one failure here that
// re-running cannot repair.
func RequireDisk(disks []Disk, serial, what string) error {
	if serial == "" {
		return fmt.Errorf("no serial given for the %s, and one cannot be guessed\n\n"+
			"this node's disks:\n\n%s\n"+
			"  put one of those serials in the machine file, then run adopt again",
			what, FormatDisks(disks))
	}

	for _, d := range disks {
		if d.Serial == serial {
			return nil
		}
	}

	return fmt.Errorf("the %s serial %q matches none of this node's disks\n\n"+
		"this node's disks:\n\n%s\n"+
		"  a serial that matches nothing is almost always a typo. Talos does not "+
		"report it as one:\n  it installs nowhere and the bring-up hangs.",
		what, serial, FormatDisks(disks))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cluster -run 'TestRequireDisk|TestFormatDisks' -v`
Expected: PASS (all four).

- [ ] **Step 5: Prove `ListDisks` against a real node**

In `TestAgainstARealNode`, replace the hand-rolled `safe.StateListAll` block with a `ListDisks` call so the exported function is what hardware exercises:

```go
	found, err := ListDisks(ctx, endpoint)
	if err != nil {
		t.Fatalf("ListDisks: %s", redactErr(err))
	}

	if len(found) == 0 {
		t.Fatal("the node reports no disks at all")
	}

	t.Logf("disks:\n%s", FormatDisks(found))
```

Keep the existing CEL/`MatchDisks` assertions below it untouched — they answer a different question.

- [ ] **Step 6: Commit**

```bash
git add cluster/nodeinfo.go cluster/nodeinfo_test.go cluster/client_test.go
git commit -m "cluster: list a node's disks, and refuse to install without a serial"
```

---

### Task 6: The `adopt` verb, and four verbs that refuse (D3, D4)

**Files:**
- Modify: `cmd/tinq/main.go` — `newRootCmd`, `standalone`, plus new helpers
- Test: `cmd/tinq/main_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: `specBaremetal(m) map[string]interface{}`, `isBaremetal(m) bool`, `adoptMachine(ctx, d, path) error`, and the `adopt` cobra command.

- [ ] **Step 1: Write the failing test**

```go
func baremetalMachine() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "machine.hvf.fleet.io/v1alpha1",
		"kind":       "TalosMachine",
		"metadata":   map[string]interface{}{"name": "bm0", "namespace": "default"},
		"spec": map[string]interface{}{
			"site": "lab", "role": "talos-cp",
			"baremetal": map[string]interface{}{
				"endpoint":         "192.168.1.50",
				"systemDiskSerial": "S1",
			},
		},
	}}
}

func TestIsBaremetalKeysOnTheSpecBlock(t *testing.T) {
	if !isBaremetal(baremetalMachine()) {
		t.Error("a machine with spec.baremetal was not recognised as baremetal")
	}

	qemu := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"image": "talos.iso"},
	}}
	if isBaremetal(qemu) {
		t.Error("a machine with no spec.baremetal was treated as baremetal\n" +
			"  reason: presence of the block IS the discriminator")
	}
}

func TestBaremetalEndpointsUseTalosDefaultPorts(t *testing.T) {
	m := baremetalMachine()

	if got := baremetalTalosEndpoint(m); got != "192.168.1.50:50000" {
		t.Errorf("talos endpoint = %q, want 192.168.1.50:50000\n"+
			"  reason: there is no forward on hardware; apid serves its own default port", got)
	}

	if got := baremetalKubeEndpoint(m); got != "https://192.168.1.50:6443" {
		t.Errorf("kube endpoint = %q, want https://192.168.1.50:6443", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/tinq -run 'TestIsBaremetal|TestBaremetalEndpoints' -v`
Expected: FAIL — `undefined: isBaremetal`.

- [ ] **Step 3: Add the spec helpers**

In `cmd/tinq/main.go`, near `talosEndpoint`:

```go
// specBaremetal returns spec.baremetal, or nil when the machine is a VM.
//
// Its PRESENCE is the discriminator, not a mode field. A machine either
// describes hardware that already exists or a guest this tool creates, and
// there is no third thing — so an explicit `provider:` string would be a second
// source of truth that could contradict the block beside it.
func specBaremetal(m *unstructured.Unstructured) map[string]interface{} {
	v, _, _ := unstructured.NestedMap(m.Object, "spec", "baremetal")
	return v
}

func isBaremetal(m *unstructured.Unstructured) bool { return specBaremetal(m) != nil }

// The two endpoints of an adopted node. NO FORWARD IS INVOLVED: apid and
// kube-apiserver serve their own default ports on the node itself, so these are
// the same constants the guest side uses, applied to a real address.
func baremetalTalosEndpoint(m *unstructured.Unstructured) string {
	if a := str(specBaremetal(m)["endpoint"], ""); a != "" {
		return fmt.Sprintf("%s:%d", a, talosAPIGuestPort)
	}
	return ""
}

func baremetalKubeEndpoint(m *unstructured.Unstructured) string {
	if a := str(specBaremetal(m)["endpoint"], ""); a != "" {
		return fmt.Sprintf("https://%s:%d", a, kubeAPIGuestPort)
	}
	return ""
}
```

- [ ] **Step 4: Write the refusal test, then the refusals**

```go
func TestVMVerbsRefuseABaremetalMachine(t *testing.T) {
	for _, verb := range []string{"apply", "up", "stop", "destroy"} {
		err := refuseWrongSubstrate(baremetalMachine(), verb)
		if err == nil {
			t.Errorf("%s accepted a baremetal machine\n"+
				"  reason: destroy in particular would delete the only talosconfig "+
				"that can reach a node it cannot destroy", verb)
			continue
		}
		if !strings.Contains(err.Error(), "adopt") {
			t.Errorf("%s's refusal does not name the verb that does work: %s", verb, err)
		}
	}
}

func TestAdoptRefusesAQEMUMachine(t *testing.T) {
	qemu := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"image": "talos.iso"},
	}}
	if err := refuseWrongSubstrate(qemu, "adopt"); err == nil {
		t.Error("adopt accepted a machine with no spec.baremetal")
	}
}
```

Implementation:

```go
// refuseWrongSubstrate rejects a verb applied to the substrate it cannot serve.
//
// The four VM verbs are not merely inapplicable to hardware, they are unsafe on
// it. `destroy` is the sharp one: its contract is to take the entire SCC, and
// on a machine it did not create it can take only the artifacts — including the
// sole talosconfig that reaches a node it has no way to destroy. A verb that
// half-honours its contract while deleting the only credential to the surviving
// machine is worse than one that refuses.
func refuseWrongSubstrate(m *unstructured.Unstructured, verb string) error {
	bm := isBaremetal(m)

	if verb == "adopt" {
		if bm {
			return nil
		}
		return fmt.Errorf("`tinq adopt` needs spec.baremetal (the node's address and its "+
			"disk serials); %s describes a VM, so `tinq up` is the verb that builds it",
			m.GetName())
	}

	if !bm {
		return nil
	}

	return fmt.Errorf("`tinq %s` cannot act on %s: it has spec.baremetal, so it is a machine "+
		"this tool did not create and cannot power-cycle\n\n  `tinq adopt` is the verb that "+
		"brings it up\n\n(there is no `forget` verb yet, so clearing its state directory is "+
		"`rm -rf` for now)", verb, m.GetName())
}
```

Wire it into `standalone` immediately after the UID is set and **before** `d.Observe` — `Observe` stats `system.qcow2` and would report a baremetal machine as `Absent`, which is a meaningless answer:

```go
	if err := refuseWrongSubstrate(m, verb); err != nil {
		return err
	}
```

- [ ] **Step 5: Extract `readMachine` from `standalone` (do this BEFORE Step 6)**

`standalone` (main.go:245-256) and the `adoptMachine` of Step 6 both need read-file → unmarshal → set stable UID. Extract that block verbatim into a helper and call it from `standalone`:

```go
// readMachine loads one TalosMachine from a file and gives it a STABLE identity.
//
// The UID is derived rather than random because it keys the state dir: a
// re-run that minted a new one would orphan the first machine's artifacts and
// build a second beside it. Shared by every file-driven verb so the derivation
// cannot drift between them.
func readMachine(path string) (*unstructured.Unstructured, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var obj map[string]interface{}
	if err := yaml.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	m := &unstructured.Unstructured{Object: obj}
	if m.GetUID() == "" {
		m.SetUID(types.UID(fmt.Sprintf("bootstrap-%s-%s", m.GetNamespace(), m.GetName())))
	}

	return m, nil
}
```

Run: `go build ./... && go test ./cmd/tinq`
Expected: PASS — pure extraction, no behaviour change.

- [ ] **Step 6: Add `adoptMachine` and the cobra command**

```go
// adoptMachine is the `adopt` verb: bring up a node this tool did not create.
//
// It does NOT go through driverkit. Observe/Create/Stop/Destroy all describe a
// resource this program owns the lifecycle of, and none of the four has an
// honest implementation for a machine on a desk with no power control.
//
// Everything before cluster.Up is a PRE-FLIGHT that a QEMU bring-up does not
// need: the version and the disks both come from the node, so both require a
// maintenance-mode node to already be answering.
func adoptMachine(ctx context.Context, d *hvf, path string) error {
	m, err := readMachine(path) // extracted from standalone in Step 6
	if err != nil {
		return err
	}

	if err := refuseWrongSubstrate(m, "adopt"); err != nil {
		return err
	}

	spec := specBaremetal(m)

	endpoint := baremetalTalosEndpoint(m)
	if endpoint == "" {
		return errors.New("spec.baremetal.endpoint is required: it is the address this host " +
			"dials to reach the node, and it goes into apid's certificate")
	}

	dir := d.dir(m)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}

	log.Printf("waiting for the Talos maintenance API at %s", endpoint)

	if err := cluster.WaitMaintenance(ctx, endpoint, adoptMaintenanceTimeout); err != nil {
		return err
	}

	disks, err := cluster.ListDisks(ctx, endpoint)
	if err != nil {
		return err
	}

	systemSerial := str(spec["systemDiskSerial"], "")
	if err := cluster.RequireDisk(disks, systemSerial, "install target"); err != nil {
		return err
	}

	// Checked ONLY when asked for. An absent data disk is a legitimate choice
	// and step 10 announces what it costs; an absent one that was MEANT to be
	// present is a typo, which the same check catches.
	dataSerial := str(spec["dataDiskSerial"], "")
	if dataSerial != "" {
		if err := cluster.RequireDisk(disks, dataSerial, "data disk"); err != nil {
			return err
		}
	}

	// The node's own answer, with the spec as an override for the case Risk 1
	// of the design spec describes: a maintenance-mode node that reports no tag.
	version := str(spec["talosVersion"], "")
	source := "spec.baremetal.talosVersion"

	if version == "" {
		if version, err = cluster.NodeVersion(ctx, endpoint); err != nil {
			return err
		}
		source = "the node's maintenance API"
	}

	return cluster.Up(ctx, cluster.UpOptions{
		ClusterName:      m.GetName(),
		StateDir:         dir,
		TalosEndpoint:    endpoint,
		KubeEndpoint:     baremetalKubeEndpoint(m),
		SystemDiskSerial: systemSerial,
		DataDiskSerial:   dataSerial,
		TalosVersion:     version,
		VersionSource:    source,
		Substrate:        fmt.Sprintf("baremetal, %s", str(spec["endpoint"], "")),
		// EMPTY BY DEFAULT. Real hardware has a firmware-configured console and
		// usually a display; a console argument derived from THIS host's
		// architecture is a guess, and a wrong one is silent at exactly the
		// boot you would need it for.
		ConsoleArg: str(spec["consoleArg"], ""),
		// The kexec workaround is QEMU-on-macOS-specific. Hardware reboots
		// through its own firmware and has nothing to work around.
		DisableKexec: false,
		// ALREADY RUNNING, by definition — that is what adopt means. Returning
		// a pid of 0 is honest: this process did not start it and has no
		// handle on it.
		Boot: func() (int, error) { return 0, nil },
	})
}
```

Add near the other timeouts in `main.go`:

```go
// adoptMaintenanceTimeout covers a node that may still be booting when adopt is
// run. It is generous because the operator has just walked over and pressed a
// power button, and firmware on real hardware is slower than QEMU's.
const adoptMaintenanceTimeout = 10 * time.Minute
```

And in `newRootCmd`:

```go
	adopt := &cobra.Command{
		Use:   "adopt <machine.yaml>",
		Short: "Bring up a Talos node this tool did NOT create",
		Long: "Takes a machine that is already booted into maintenance mode — from a USB\n" +
			"stick, virtual media, or netboot — and drives it to a Ready single-node\n" +
			"cluster using the same ten steps `up` uses.\n\n" +
			"Requires spec.baremetal. Run it once with no systemDiskSerial and it prints\n" +
			"the node's disks and refuses; write one down and run it again.\n\n" +
			"It never powers anything on and never installs without an explicit serial.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := newDriver()
			if err != nil {
				return err
			}
			return adoptMachine(cmd.Context(), d, args[0])
		},
	}
```

Add `adopt` to `root.AddCommand(...)`.

- [ ] **Step 7: Run the suite**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add cmd/tinq/main.go cmd/tinq/main_test.go
git commit -m "tinq: an adopt verb, and four verbs that refuse hardware"
```

---

### Task 7: CRD surface for `spec.baremetal` (D3)

**Files:**
- Modify: `crd/talosmachine.yaml:72` (`required`), `:73-130` (properties)

**Interfaces:**
- Consumes: the field names from Task 6.
- Produces: nothing consumed by later tasks.

- [ ] **Step 1: Relax `required` and add the block**

`required: [site, role, image, cpu, memory, disk]` rejects every baremetal machine, because `image`, `cpu`, `memory` and `disk` describe a guest to build. Change to `required: [site, role]` and restore the real rule with CEL, which can express "one shape or the other" where `required` cannot:

```yaml
              required: [site, role]
              x-kubernetes-validations:
                - rule: "has(self.baremetal) || (has(self.image) && has(self.cpu) && has(self.memory) && has(self.disk))"
                  message: >-
                    a TalosMachine is either hardware (spec.baremetal) or a VM to build
                    (image, cpu, memory, disk). Relaxing `required` is what allows the
                    first; this rule is what stops it also allowing a VM with no disk.
                - rule: "!(has(self.baremetal) && has(self.hostForwards))"
                  message: >-
                    spec.hostForwards describes a qemu user-mode port forward and has no
                    meaning for hardware, which serves apid on its own address.
```

Add under `properties:`:

```yaml
                baremetal:
                  type: object
                  required: [endpoint]
                  description: >-
                    Present when this machine is HARDWARE that already exists. Its
                    presence is the discriminator: apply/up/stop/destroy refuse a
                    machine that has it, and `tinq adopt` requires it. Nothing here
                    powers a node on — it must already be in maintenance mode.
                  properties:
                    endpoint:
                      type: string
                      description: >-
                        The node's address, no port. Talos's own defaults are used
                        (50000 for apid, 6443 for kube-apiserver) because there is no
                        forward to describe. This value goes into apid's certificate.
                    systemDiskSerial:
                      type: string
                      description: >-
                        Serial of the install target. Omit on the first run and `adopt`
                        prints the node's disks and refuses — a size matcher is a coin
                        flip, and losing it overwrites a disk that may hold data.
                    dataDiskSerial:
                      type: string
                      description: >-
                        Serial of the PVC disk. Absent means no user volume AND no
                        StorageClass; one field gates both halves so they cannot
                        disagree.
                    consoleArg:
                      type: string
                      description: >-
                        Console kernel argument for the installed system. DEFAULT NONE —
                        hardware has a firmware-configured console, and a guessed one is
                        silent at exactly the boot you would need it for.
                    talosVersion:
                      type: string
                      description: >-
                        Override for the version `adopt` reads from the node. Only needed
                        if a maintenance-mode node reports no version tag.
```

- [ ] **Step 2: Verify the CRD still parses and validates**

Run: `go test ./... && python3 -c "import yaml,sys; d=list(yaml.safe_load_all(open('crd/talosmachine.yaml'))); print('documents:', len(d))"`
Expected: PASS, and a document count matching before the change.

- [ ] **Step 3: Commit**

```bash
git add crd/talosmachine.yaml
git commit -m "crd: spec.baremetal, and a validation rule that keeps VMs whole"
```

---

### Task 8: `talosEndpoint`/`kubeEndpoint` honour `hostAddr` (Tier 1, found during Task 1 review)

**Files:**
- Modify: `cmd/tinq/main.go:338-357` (`talosEndpoint`, `kubeEndpoint`, and `hostForward`)
- Test: `cmd/tinq/main_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `hostForward` returning the bind address alongside the port; both endpoint helpers using it.

**Why this is Tier 1 and not scope creep.** This branch exists to delete hardcoded `127.0.0.1` assumptions. `main.go:341` and `:353` are two more instances of exactly that class, in the same call chain that feeds `UpOptions.TalosEndpoint` — the field Task 1 made the sole source of the certificate SAN. `spec.hostForwards[].hostAddr` already exists (`main.go:874`) and already defaults to loopback, but these two functions ignore it: set `hostAddr: 192.168.1.165` and QEMU binds that address ONLY, while `tinq up` dials `127.0.0.1` where nothing is listening and spends the entire 5-minute maintenance budget before failing.

Fixing it also strengthens Task 9: `tinq up` on the rehearsal VM then proves the non-loopback path through the `up` verb independently of `adopt`.

- [ ] **Step 1: Write the failing test**

```go
func TestEndpointsHonourHostAddr(t *testing.T) {
	m := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"hostForwards": []interface{}{
			map[string]interface{}{"hostPort": int64(50000), "guestPort": int64(50000),
				"hostAddr": "192.168.1.165"},
			map[string]interface{}{"hostPort": int64(6443), "guestPort": int64(6443),
				"hostAddr": "192.168.1.165"},
		}},
	}}

	if got := talosEndpoint(m); got != "192.168.1.165:50000" {
		t.Errorf("talosEndpoint = %q, want 192.168.1.165:50000\n"+
			"  reason: qemu binds the forward to hostAddr ONLY, so dialling loopback "+
			"reaches nothing and spends the whole maintenance budget proving it", got)
	}

	if got := kubeEndpoint(m); got != "https://192.168.1.165:6443" {
		t.Errorf("kubeEndpoint = %q, want https://192.168.1.165:6443\n"+
			"  reason: this address is written into the kubeconfig AND becomes the "+
			"cert SAN, so a wrong one fails every kubectl call", got)
	}
}

// The default is the regression that matters: an entry with no hostAddr must
// still produce loopback, because that is what every existing machine file has.
func TestEndpointsDefaultToLoopbackWithoutHostAddr(t *testing.T) {
	m := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"hostForwards": []interface{}{
			map[string]interface{}{"hostPort": int64(50000), "guestPort": int64(50000)},
		}},
	}}

	if got := talosEndpoint(m); got != "127.0.0.1:50000" {
		t.Errorf("talosEndpoint = %q, want 127.0.0.1:50000", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/tinq -run TestEndpoints -v`
Expected: FAIL — `talosEndpoint = "127.0.0.1:50000", want 192.168.1.165:50000`.

- [ ] **Step 3: Thread the address through**

`hostForward` currently returns only the port. Return the address too, and use the SAME default the qemu argument builder uses at `main.go:874` — `str(h["hostAddr"], "127.0.0.1")` — so the dialled address and the bound address cannot disagree:

```go
// hostForward reports the HOST address and port forwarded to guestPort, or
// ("", 0) when the machine forwards nothing there.
//
// THE ADDRESS IS RETURNED, not assumed. qemu binds each forward to its own
// hostAddr (main.go:874) and binds it EXCLUSIVELY: with hostAddr set to a LAN
// address, nothing is listening on loopback at all. Returning a hardcoded
// 127.0.0.1 here sent every wait to an address that could never answer, and the
// symptom was a full maintenance timeout rather than a connection refusal.
func hostForward(m *unstructured.Unstructured, guestPort int) (string, int) {
	for _, hf := range nestedSlice(m, "spec", "hostForwards") {
		h, _ := hf.(map[string]interface{})
		if toInt(h["guestPort"]) == guestPort {
			return str(h["hostAddr"], "127.0.0.1"), toInt(h["hostPort"])
		}
	}
	return "", 0
}

func talosEndpoint(m *unstructured.Unstructured) string {
	if a, p := hostForward(m, talosAPIGuestPort); p > 0 {
		return fmt.Sprintf("%s:%d", a, p)
	}
	return ""
}

func kubeEndpoint(m *unstructured.Unstructured) string {
	if a, p := hostForward(m, kubeAPIGuestPort); p > 0 {
		return fmt.Sprintf("https://%s:%d", a, p)
	}
	return ""
}
```

Update `hostForward`'s other caller for the two-value return.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/tinq -run TestEndpoints -v` then `go build ./... && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/tinq/main.go cmd/tinq/main_test.go
git commit -m "tinq: dial the address the forward is actually bound to"
```

---

### Task 9: The live gate — QEMU regression and the non-loopback rehearsal

Nothing before this proves the change works against a real Talos node. Both runs below are required before the hardware attempt.

**Files:**
- Create: `examples/adopt-machine.yaml` (the VM that stands in for hardware), `examples/adopt-node.yaml` (the machine `adopt` is run against)
- Modify: `README.md`

- [ ] **Step 1: QEMU regression on loopback**

```bash
go run ./cmd/tinq destroy examples/bootstrap-machine.yaml
go run ./cmd/tinq up examples/bootstrap-machine.yaml
```

Expected: all ten steps pass to a Ready node with storage, unchanged from before this branch. Step 1 still prints `linux/amd64, kvm, qemu-system-x86_64`; step 2 now reads `version   talos-v1.13.7-amd64.iso (ISO volume id) -> v1.13.7`.

Record the wall-clock time and compare against the README's 3.5–4 minutes. A large regression here means the version resolution moved somewhere it blocks.

- [ ] **Step 2: Prove the real-node assertions, including Risk 1**

```bash
TINQ_NODE=127.0.0.1:50000 go test ./cluster -run TestAgainstARealNode -v
```

Expected: PASS, and the log shows a non-empty Talos version tag plus the disk table. **If the version tag is empty, Risk 1 has materialised** — `spec.baremetal.talosVersion` is the documented fallback and the README must say so before the hardware attempt.

- [ ] **Step 3: Create the rehearsal machine file**

`examples/adopt-machine.yaml` — a QEMU machine publishing its Talos API on this host's LAN address, so the bring-up dials something that is genuinely not loopback:

```yaml
# The REHEARSAL for a baremetal adopt, run against a VM.
#
# Everything `adopt` does differently from `up` is exercised here except the two
# things only hardware can show: a real NIC taking a DHCP lease, and real disk
# serials. What IS exercised is the part that silently broke before this branch —
# a certificate that must name an address other than 127.0.0.1.
#
# hostAddr is the existing per-forward bind address (main.go:874). Substitute
# this host's own LAN address; `ip -4 -br addr` prints it.
apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata:
  name: rehearsal-cp0
  namespace: default
spec:
  site: clvc-local
  role: talos-cp
  image: talos-v1.13.7-amd64.iso
  cpu: 4
  memory: 6Gi
  disk: 20Gi
  dataDisk: 40Gi
  hostForwards:
    - hostPort: 50000
      guestPort: 50000
      hostAddr: 192.168.1.165
    - hostPort: 6443
      guestPort: 6443
      hostAddr: 192.168.1.165
```

Then `examples/adopt-node.yaml` — the machine `adopt` is actually run against. It is a SEPARATE file from the one above and deliberately carries no `systemDiskSerial`, because the first run is meant to be refused:

```yaml
# The adopted node. Deliberately has NO systemDiskSerial: the first `tinq adopt`
# against a node is expected to REFUSE and print the node's disks, because a
# serial cannot be guessed and picking wrong overwrites a disk.
#
# For the rehearsal this points at the VM created by adopt-machine.yaml, so the
# address is this host's LAN address rather than a node's own. On real hardware
# it is the node's DHCP address and nothing else changes.
#
# A DIFFERENT metadata.name from adopt-machine.yaml, on purpose: the name keys
# the state directory, and sharing one would put an adopted node's talosconfig
# in the VM's state dir where `tinq destroy` would take it.
apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata:
  name: rehearsal-node0
  namespace: default
spec:
  site: clvc-local
  role: talos-cp
  baremetal:
    endpoint: 192.168.1.165
    # systemDiskSerial: talos-system   # added in Step 5, after the refusal shows it
    # dataDiskSerial: talos-data
```

- [ ] **Step 4: Boot the VM, then adopt it and expect a refusal**

```bash
go run ./cmd/tinq apply examples/adopt-machine.yaml
go run ./cmd/tinq adopt examples/adopt-node.yaml
```

Expected: **refusal**, printing the disk table with `talos-system` and `talos-data`, and flagging the boot ISO as `readonly — probably the medium you booted from`. This is the D7 refusal, and the readonly flag is the D7 correction, both working.

- [ ] **Step 5: Add the serials and adopt for real**

Uncomment `systemDiskSerial: talos-system` and `dataDiskSerial: talos-data` in `examples/adopt-node.yaml`, then re-run `go run ./cmd/tinq adopt examples/adopt-node.yaml`.

Expected: all ten steps to a Ready node. Then the assertion that matters most:

```bash
export TALOSCONFIG=~/.hvf/<state-dir>/talosconfig
export KUBECONFIG=~/.hvf/<state-dir>/kubeconfig
kubectl get nodes
```

`kubectl` must succeed against `https://192.168.1.165:6443`. **If the certificate still names only 127.0.0.1, this is where it fails** — and that is precisely the failure hardware would have shown.

- [ ] **Step 6: Verify the typo refusal**

Change `systemDiskSerial` to `talos-systemm` on a destroyed-and-recreated VM and confirm `adopt` refuses with the table rather than hanging.

- [ ] **Step 7: Document `adopt` in the README**

Add a section covering: what `adopt` is for, that the node must already be in maintenance mode, the two-run discover-then-adopt flow, the four verbs that refuse and why, and that clearing a baremetal state dir is `rm -rf` until a `forget` verb exists. Mark it *unverified on hardware* until the coordinated attempt happens — the README's existing convention is that *verified* means run on real hosts.

- [ ] **Step 8: Commit**

```bash
git add examples/adopt-machine.yaml examples/adopt-node.yaml README.md
git commit -m "docs: adopt, and the non-loopback rehearsal that stands in for hardware"
```

---

## Self-Review

**1. Spec coverage.** D1 → Task 3. D2 → Task 1. D3 → Tasks 6, 7. D4 → Task 6. D5 → Task 2. D6 → Task 3 (Step 5) and Task 6. D7 → Tasks 4, 5. Testing section → Tasks 1–7 unit steps plus Task 8 live gate. Risk 1 → Task 4 Step 5 and Task 8 Step 2, with the `talosVersion` override built in Task 6 and documented in Task 7. Risk 2 (blank serials) → `FormatDisks` renders WWID when a serial is absent. Risk 3 (DHCP) → documentation only, Task 8 Step 7. No gaps.

**2. Placeholder scan.** No TBD/TODO. Every code step carries real code. Task 8 is deliberately procedural rather than code-bearing because it is a live-run gate, and each step states its expected observation and what a failure means.

**3. Type consistency.** `ConfigInput.APIAddress` (Task 1) is consumed in Task 2's option list and Task 3's `configure`. `UpOptions` fields added in Task 3 are the exact names filled in Task 6's `adoptMachine`. `cluster.Disk` / `ListDisks` / `FormatDisks` / `RequireDisk` (Task 5) are used with those signatures in Task 6. `NodeVersion` (Task 4) is called in Task 6. `readMachine` is extracted in Task 6 **Step 5** and consumed by `adoptMachine` in Step 6 — the extraction precedes its first caller, so every step compiles in the order written.

**Ordering across tasks.** Tasks 1–3 are strictly sequential: Task 2's option list is written against the `APIAddress` field Task 1 adds, and Task 3's `configure` signature drops parameters both earlier tasks still reference. Tasks 4 and 5 share one file and must run in order. Task 6 depends on all of 1–5. Tasks 7 and 8 depend on 6. No task may be reordered or parallelised.
