# Static Network Configuration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A baremetal TalosMachine may declare a static address, gateway, nameservers and NIC, and the installed system keeps them across the install reboot.

**Architecture:** A node has two addresses in its life. `spec.baremetal.maintenanceEndpoint` is where it answers before the install; `spec.baremetal.network.address` is where it answers after. The first drives steps 5-7 of `cluster.Up`; the second drives steps 8-10, the apid certificate SAN, the talosconfig and the kubeconfig. One `netip.Prefix.Contains` call refuses the case where the two are on different segments, because that node cannot be reached again and the run cannot be resumed.

**Tech Stack:** Go 1.26.5, `github.com/siderolabs/talos/pkg/machinery@v1.13.7`, `net/netip`, COSI (`github.com/cosi-project/runtime/pkg/safe`), `sigs.k8s.io/yaml`, `k8s.io/apimachinery` unstructured.

**Design spec:** `docs/superpowers/specs/2026-08-02-static-network-design.md`. Read it before starting.

## Global Constraints

- **Module path is `github.com/coglative/talos-in-qemu`.** Imports are `.../cluster`, `.../driverkit`, `.../platform`.
- **`go.mod` and `go.sum` MUST stay byte-identical.** Machinery already carries every type this needs. If you find yourself adding a dependency, you have taken a wrong turn.
- **Baseline is green.** `go build ./...`, `go test ./...` and `go vet ./...` all pass on `feat/lifecycle` at commit `3864ebb`. Any failure you see is one you caused.
- **Assert against `v1alpha1Doc(t, …)` (cluster/config_test.go:177), NEVER raw config bytes.** `v1alpha1Doc` runs `code()`, which strips comments. Machinery's encoder emits commented-out examples, so a `strings.Contains` on raw bytes matches a comment and reports a field that was never set. This has produced false-passing assertions in this repo twice.
- **Test the seam, not just the function.** A correct function that nothing calls passes its own tests. When a struct gains a field, assert that the caller actually threads it, with a fixture value that could not appear by accident.
- **Secrets never reach a log or an error.** `talosconfig`, `kubeconfig` and `secrets.yaml` are secret. `cluster` has `redact()` and `redactErr()`; use them in every `t.Errorf`/`t.Fatalf` that touches generated material.
- **Comment style.** This repo writes comments that say WHY, in full sentences, often naming the failure the code prevents. Match it. Do not write `// set the address`.
- **Commit style: conventional commits, no AI attribution, no `Co-Authored-By`.**
- **IPv6 is out of scope** and is refused explicitly, not accidentally.
- **Run `go build ./... && go test ./... && go vet ./...` before every commit.**

## File Structure

| File | Responsibility |
|---|---|
| `cluster/network.go` (new) | The `Network` value, its refusals, and the machinery option that renders it |
| `cluster/network_test.go` (new) | `CheckNetwork` table, `Network.IP`, the rendered option |
| `cluster/config.go` | `ConfigInput.Network`, one `genOpts` append |
| `cluster/up.go` | `UpOptions.InstalledEndpoint`, endpoint routing, the `CheckNetwork` refusal, one transcript line |
| `cluster/nodeinfo.go` | `Link`, `ListLinks`, `FormatLinks`, `RequireLink` — mirrors the disk trio |
| `cmd/tinq/adopt.go` | Renamed readers, `network` parsing, pre-flight, endpoint derivation, the `ip=` line |
| `crd/talosmachine.yaml` | The rename and the `network` sub-schema |

## Task Order

The rename lands first so every later task edits already-renamed code. Task 10 is the Tier 2 defect fix and is independent of tasks 2-9.

---

### Task 1: Rename `spec.baremetal.endpoint` to `maintenanceEndpoint`

Hard rename, no alias. The API is `v1alpha1` with one operator, and carrying both spellings would leave two fields whose names do not say which one outlives the install. Nothing behaves differently after this task; it is pure renaming so that later tasks are not editing two spellings at once.

**Files:**
- Modify: `crd/talosmachine.yaml:134-144`
- Modify: `cmd/tinq/adopt.go:76, 83, 197, 239, 245, 260`
- Modify: `cmd/tinq/crd_test.go:108, 114-119, 126`
- Modify: `cmd/tinq/adopt_test.go:29, 210, 293`
- Modify: `examples/adopt-node.yaml:33`
- Modify: `README.md:548`
- Modify: `cluster/client.go:111` and `cluster/up.go:205` (prose references only)

**Interfaces:**
- Consumes: nothing.
- Produces: the manifest key `spec.baremetal.maintenanceEndpoint`, read by `baremetalTalosEndpoint` and `baremetalKubeEndpoint` in `cmd/tinq/adopt.go`. Every later task assumes this spelling.

- [ ] **Step 1: Update the failing tests first**

In `cmd/tinq/adopt_test.go`, change the fixture at line 29 and the two inline YAML manifests:

```go
			"baremetal": map[string]interface{}{
				"maintenanceEndpoint": "192.168.1.50",
				"systemDiskSerial":    "S1",
			},
```

At line 210 and line 293 the manifests are raw YAML strings; change `endpoint:` to `maintenanceEndpoint:` in both. Line 293's value stays `10.0.0.5:50000` — that test asserts a port is refused.

In `cmd/tinq/crd_test.go`, change three places:

```go
	if got, want := crdStrings(t, baremetal["required"], "spec.baremetal.required"), []string{"maintenanceEndpoint"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spec.baremetal.required is %v, want %v — adopt.go cannot dial a node with no address", got, want)
	}
```

```go
	if got := fmt.Sprint(crdMap(t, props["maintenanceEndpoint"], "spec.baremetal.maintenanceEndpoint")["minLength"]); got != "1" {
		t.Errorf("spec.baremetal.maintenanceEndpoint minLength is %s, want 1 — `required` alone accepts an empty string", got)
	}
```

```go
	for _, f := range []string{"maintenanceEndpoint", "systemDiskSerial", "dataDiskSerial", "consoleArg", "talosVersion"} {
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/tinq -run 'TestCRDGuardsWhatTheGoCodeAssumes|TestBaremetalEndpointsUseTalosDefaultPorts|TestAdoptRefusesAnEndpointCarryingAPort' -v`
Expected: FAIL — `spec.baremetal.required is [endpoint], want [maintenanceEndpoint]`, and the endpoint helpers return `""` because they read a key that is no longer in the fixture.

- [ ] **Step 3: Rename in the CRD**

In `crd/talosmachine.yaml`, replace lines 127 and 134-144:

```yaml
                  required: [maintenanceEndpoint]
```

```yaml
                    maintenanceEndpoint:
                      type: string
                      # minLength CARRIES THE WEIGHT `required` LOOKS LIKE IT DOES.
                      # `required: [maintenanceEndpoint]` is key presence only, so
                      # `maintenanceEndpoint: ""` satisfies it and travels all the way to
                      # adopt.go, which then rejects it. The schema should refuse what it
                      # appears to refuse.
                      minLength: 1
                      description: >-
                        Where the node answers RIGHT NOW, in maintenance mode. No port:
                        Talos's own defaults are used (50000 for apid, 6443 for
                        kube-apiserver) because there is no forward to describe.
                        With no network block this is also where the node answers
                        FOREVER, and it goes into apid's certificate. With one, it is
                        transient — spec.baremetal.network.address takes over the moment
                        the node reboots into what it installed.
```

- [ ] **Step 4: Rename in adopt.go**

`cmd/tinq/adopt.go`, the two readers:

```go
func baremetalTalosEndpoint(m *unstructured.Unstructured) string {
	if a := str(baremetalFields(m)["maintenanceEndpoint"], ""); a != "" {
		return fmt.Sprintf("%s:%d", a, talosAPIGuestPort)
	}
	return ""
}

func baremetalKubeEndpoint(m *unstructured.Unstructured) string {
	if a := str(baremetalFields(m)["maintenanceEndpoint"], ""); a != "" {
		return fmt.Sprintf("https://%s:%d", a, kubeAPIGuestPort)
	}
	return ""
}
```

In `adoptMachine`, the malformed-block message at line 239, the required message at line 245, and the port refusal at line 259-263:

```go
		return fmt.Errorf("%s has spec.baremetal, but it is not a block of fields — a scalar "+
			"or an empty `baremetal:` cannot carry an endpoint or a disk serial\n\n"+
			"  it must be a mapping:\n\n    baremetal:\n      maintenanceEndpoint: 192.168.1.50\n"+
			"      systemDiskSerial: S1", m.GetName())
```

```go
	endpoint := baremetalTalosEndpoint(m)
	if endpoint == "" {
		return errors.New("spec.baremetal.maintenanceEndpoint is required: it is the address this host " +
			"dials to reach the node while it is in maintenance mode, and with no network block " +
			"it is also where the node answers afterwards")
	}
```

```go
	if addr := str(spec["maintenanceEndpoint"], ""); strings.Contains(addr, ":") {
		return fmt.Errorf("spec.baremetal.maintenanceEndpoint %q must be a bare address with no port: "+
			"apid's %d and kube-apiserver's %d are Talos's own and are added for you\n\n"+
			"  (an IPv6 literal lands here too, and is not supported yet)",
			addr, talosAPIGuestPort, kubeAPIGuestPort)
	}
```

Also update the prose at `adopt.go:33` and `adopt.go:197` that names `spec.baremetal.endpoint`.

- [ ] **Step 5: Rename in the remaining prose and examples**

`examples/adopt-node.yaml:33` becomes `maintenanceEndpoint: 192.168.1.165`, and the comment block above it that explains "NO PORT on endpoint" becomes "NO PORT on maintenanceEndpoint".

`README.md:548` becomes `maintenanceEndpoint: 192.168.1.50      # the node's address, NO port`.

`cluster/client.go:111` and `cluster/up.go:205` mention `spec.baremetal.endpoint` in comments; both become `spec.baremetal.maintenanceEndpoint`.

- [ ] **Step 6: Verify everything passes and nothing else references the old key**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: all packages `ok`, vet silent.

Run: `rg -n 'baremetal\.endpoint|"endpoint"' --glob '!docs/superpowers/**' .`
Expected: no matches outside `docs/superpowers/`. The design and requirements specs keep their historical spelling; do not edit them.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "crd: an endpoint that stops being true needs a name that says when"
```

---

### Task 2: `cluster.Network` and the refusals that make it safe

The value and its validation. No machinery imports in this task — everything here is `net/netip` and `net`, which keeps the refusals testable with nothing running.

**Why the Go checks and not just the CRD:** `tinq adopt some-file.yaml` reads a local YAML file with no apiserver anywhere. The CRD's `required` and `pattern` are enforced only when a manifest goes through Kubernetes, which is not the path the operator uses. Every requirement the schema states, this function must state too, or it is not enforced at all on the standalone path.

**Files:**
- Create: `cluster/network.go`
- Create: `cluster/network_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Network struct { Address, Gateway string; Nameservers []string; HardwareAddr string }`
  - `func (n *Network) IP() (string, error)` — host part of `Address`, prefix stripped
  - `func CheckNetwork(n *Network, maintenanceAddr string) error` — nil `n` returns nil

- [ ] **Step 1: Write the failing test**

Create `cluster/network_test.go`:

```go
package cluster

import (
	"strings"
	"testing"
)

// validNetwork is the block the target machine actually uses, and every case
// below mutates ONE field of it. A fixture built per-case would let a test pass
// because of a second mistake nobody noticed.
func validNetwork() *Network {
	return &Network{
		Address:      "192.168.2.10/24",
		Gateway:      "192.168.2.1",
		Nameservers:  []string{"1.1.1.1"},
		HardwareAddr: "84:47:09:47:35:f9",
	}
}

func TestCheckNetworkAcceptsANodeThatDoesNotMove(t *testing.T) {
	// The no-DHCP case: the operator gave the node its final address at the
	// GRUB prompt, so the two are equal and Contains is trivially true.
	if err := CheckNetwork(validNetwork(), "192.168.2.10"); err != nil {
		t.Errorf("a node adopted at the address it will keep was refused: %s", err)
	}
}

func TestCheckNetworkAcceptsASameWireRePin(t *testing.T) {
	// Adopted over DHCP at .186, installed with a static .10 on the SAME
	// segment. The node reboots, re-pins itself and is still reachable, so
	// nothing physical has to happen and the run can finish.
	n := validNetwork()
	n.Address = "192.168.1.50/24"
	n.Gateway = "192.168.1.1"

	if err := CheckNetwork(n, "192.168.1.186"); err != nil {
		t.Errorf("a same-wire re-pin was refused: %s", err)
	}
}

func TestCheckNetworkRefusesAnAddressOnAnotherWire(t *testing.T) {
	// THE REFUSAL THIS WHOLE FEATURE IS BUILT AROUND. The node would come back
	// on an address that does not exist on the wire it is plugged into, the run
	// cannot finish, and a re-run cannot repair it because the node will never
	// serve the maintenance API again.
	err := CheckNetwork(validNetwork(), "192.168.1.186")
	if err == nil {
		t.Fatal("an address on a different segment than the maintenance address was accepted\n" +
			"  reason: after the install reboot that node is unreachable, and adopt cannot " +
			"resume because maintenance mode is gone")
	}

	for _, want := range []string{"192.168.2.10/24", "192.168.1.186"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s, so it cannot be acted on:\n%s", want, err)
		}
	}
}

func TestCheckNetworkRefusesAGatewayOffTheSegment(t *testing.T) {
	n := validNetwork()
	n.Gateway = "192.168.3.1"

	if err := CheckNetwork(n, "192.168.2.10"); err == nil {
		t.Error("a gateway outside the node's own prefix was accepted\n" +
			"  reason: the node has no route to it, so it has no route off its segment")
	}
}

func TestCheckNetworkRefusesANetworkAddress(t *testing.T) {
	n := validNetwork()
	n.Address = "192.168.2.0/24"

	if err := CheckNetwork(n, "192.168.2.10"); err == nil {
		t.Error("192.168.2.0/24 was accepted as a node address, but it names the segment")
	}
}

func TestCheckNetworkRefusesWhatItCannotParse(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Network)
	}{
		{"a bare address with no prefix", func(n *Network) { n.Address = "192.168.2.10" }},
		{"an address that is not an address", func(n *Network) { n.Address = "not-an-address/24" }},
		{"an IPv6 prefix", func(n *Network) { n.Address = "2001:db8::10/64" }},
		{"a gateway that is not an address", func(n *Network) { n.Gateway = "192.168.2" }},
		{"no nameservers", func(n *Network) { n.Nameservers = nil }},
		{"an empty nameserver", func(n *Network) { n.Nameservers = []string{""} }},
		{"no hardware address", func(n *Network) { n.HardwareAddr = "" }},
		{"a hardware address that is not a MAC", func(n *Network) { n.HardwareAddr = "84:47:09" }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := validNetwork()
			c.set(n)

			if err := CheckNetwork(n, "192.168.2.10"); err == nil {
				t.Errorf("%s was accepted", c.name)
			}
		})
	}
}

func TestCheckNetworkPassesTheDHCPCase(t *testing.T) {
	// Absent is the answer every machine gave before this feature existed, and
	// it must stay an answer rather than becoming a missing field.
	if err := CheckNetwork(nil, "192.168.1.50"); err != nil {
		t.Errorf("a machine with no network block was refused: %s", err)
	}
}

func TestCheckNetworkRefusesAMaintenanceAddressItCannotParse(t *testing.T) {
	// A host:port here is the realistic mistake — every other endpoint in this
	// package carries a port, and this one must not.
	if err := CheckNetwork(validNetwork(), "192.168.2.10:50000"); err == nil {
		t.Error("a maintenance address with a port was accepted, but it cannot be compared to a prefix")
	}
}

func TestNetworkIPStripsThePrefix(t *testing.T) {
	// This value becomes the certificate SAN, the talosconfig endpoint and the
	// kubeconfig server. A prefix left on it names a host nobody can dial.
	got, err := validNetwork().IP()
	if err != nil {
		t.Fatalf("IP(): %s", err)
	}

	if got != "192.168.2.10" {
		t.Errorf("IP() = %q, want 192.168.2.10", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cluster -run TestCheckNetwork -v`
Expected: FAIL to compile — `undefined: Network`, `undefined: CheckNetwork`.

- [ ] **Step 3: Write the implementation**

Create `cluster/network.go`:

```go
package cluster

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// A NODE HAS TWO ADDRESSES IN ITS LIFE, and this file is what keeps them from
// disagreeing.
//
// Before the install it answers at the maintenance address — from DHCP, or from
// an `ip=` kernel argument typed at a GRUB prompt on a segment that serves no
// DHCP at all. After the install it answers at Network.Address, because the
// installed system writes its own kernel command line and inherits nothing from
// the ISO.
//
// The certificate SAN, the talosconfig endpoint and the kubeconfig server are
// all baked at generation time from the SECOND of those. That is why a mismatch
// here is not merely a node that is hard to find: it is a node whose PKI names
// an address nobody dials, which no amount of re-pointing a client repairs.

// Network is a static address for a node, or nil for DHCP.
//
// All four fields are required together. There is no useful half-configured
// state: a static block with no nameservers is a node that cannot resolve a
// registry, and therefore cannot pull the image it was just told to install.
type Network struct {
	// Address is the node's address after the install, in CIDR —
	// 192.168.2.10/24. The prefix is not decoration: it is how the node knows
	// which addresses are on its own segment, and it is what CheckNetwork
	// compares the maintenance address against.
	Address string
	// Gateway is the default route's next hop. It must be inside Address's
	// prefix, because a node has no route to a gateway off its own segment.
	Gateway string
	// Nameservers is at least one resolver. On a segment with no DHCP there is
	// no other source of one.
	Nameservers []string
	// HardwareAddr is the MAC of the NIC to configure.
	//
	// A MAC rather than an interface name, for the reason the install disk is
	// selected by serial rather than by size: a stable identity, not an
	// enumeration artifact. Predictable interface names come from slot and
	// firmware indices and change when the card moves.
	HardwareAddr string
}

// IP is the host part of Address with the prefix stripped.
//
// This value becomes the apid certificate's subject alt name, the talosconfig
// endpoint and the kubeconfig server, so cmd/tinq uses it to build BOTH
// post-install endpoints.
func (n *Network) IP() (string, error) {
	p, err := netip.ParsePrefix(n.Address)
	if err != nil {
		return "", fmt.Errorf("the static address %q is not an address with a prefix length: %w", n.Address, err)
	}

	return p.Addr().String(), nil
}

// CheckNetwork refuses a static block that would strand the node.
//
// ONE function with TWO callers, for the same reason errUnknownTalosVersion is
// one: Up calls it before Boot, so nothing can bypass it, and adopt calls it
// first, so the answer arrives in a second rather than after a ten-minute
// maintenance wait. Two copies would drift into two explanations of one refusal.
//
// A nil Network passes. Absent is the answer every machine gave before this
// feature existed, and it has to stay an answer rather than become a missing
// field.
//
// maintenanceAddr is a BARE address with no port, for the same reason
// ConfigInput.APIAddress is one: what has to sit inside the prefix is a host,
// not a socket.
func CheckNetwork(n *Network, maintenanceAddr string) error {
	if n == nil {
		return nil
	}

	prefix, err := netip.ParsePrefix(n.Address)
	if err != nil {
		return fmt.Errorf("spec.baremetal.network.address %q is not an address with a prefix "+
			"length\n\n  it must be CIDR, e.g. 192.168.2.10/24 — the prefix is how the node "+
			"knows which\n  addresses are on its own segment, and there is no default that is "+
			"right for two networks", n.Address)
	}

	// Refused by NAME rather than by accident. Left to fall through, a v6
	// prefix would fail the containment check below against a v4 maintenance
	// address and report a mismatch, which is a true statement about the wrong
	// problem.
	if !prefix.Addr().Is4() {
		return fmt.Errorf("spec.baremetal.network.address %q is IPv6, which is not supported yet\n\n"+
			"  spec.baremetal.maintenanceEndpoint cannot carry a v6 literal either — see the "+
			"refusal\n  in adopt for why", n.Address)
	}

	if prefix.Addr() == prefix.Masked().Addr() {
		return fmt.Errorf("spec.baremetal.network.address %q names the SEGMENT, not a node on it\n\n"+
			"  the host part is all zeroes. A node needs its own address inside that prefix, "+
			"e.g. %s",
			n.Address, netip.PrefixFrom(prefix.Masked().Addr().Next(), prefix.Bits()))
	}

	gateway, err := netip.ParseAddr(n.Gateway)
	if err != nil {
		return fmt.Errorf("spec.baremetal.network.gateway %q is not an address", n.Gateway)
	}

	if !prefix.Contains(gateway) {
		return fmt.Errorf("spec.baremetal.network.gateway %s is outside %s\n\n"+
			"  a node has no route to a gateway off its own segment, so this one is "+
			"unreachable\n  from the moment the node boots — and with it, everything past it",
			n.Gateway, n.Address)
	}

	if len(n.Nameservers) == 0 {
		return errors.New("spec.baremetal.network.nameservers is required alongside a static address\n\n" +
			"  a segment with no DHCP offers no resolver either, and a node that cannot resolve\n" +
			"  a registry cannot pull the image it was just told to install")
	}

	for i, ns := range n.Nameservers {
		if _, err := netip.ParseAddr(ns); err != nil {
			return fmt.Errorf("spec.baremetal.network.nameservers[%d] %q is not an address", i, ns)
		}
	}

	// PRESENCE IS REFUSED HERE, from the file, and the remedy cannot be "run
	// adopt to see the table" — this check fires before the node is dialled, so
	// there is no table yet. The realistic failure is not an omitted MAC but a
	// MISTYPED one, and that is the case RequireLink catches with the node's
	// real links printed.
	if n.HardwareAddr == "" {
		return errors.New("spec.baremetal.network.hardwareAddr is required alongside a static address\n\n" +
			"  the node's own console lists its interfaces and their MACs, and you are already\n" +
			"  standing at it — the ip= kernel argument was typed on that same screen.\n\n" +
			"  A MAC that is well-formed but wrong is refused later, with this node's real\n" +
			"  links printed for you to copy from.")
	}

	if _, err := net.ParseMAC(n.HardwareAddr); err != nil {
		return fmt.Errorf("spec.baremetal.network.hardwareAddr %q is not a MAC address\n\n"+
			"  it looks like 84:47:09:47:35:f9, and adopt prints this node's real ones", n.HardwareAddr)
	}

	maintenance, err := netip.ParseAddr(maintenanceAddr)
	if err != nil {
		return fmt.Errorf("the maintenance address %q is not a bare address\n\n"+
			"  a port does not belong here: what has to sit inside %s is a host",
			maintenanceAddr, n.Address)
	}

	// THE REFUSAL THIS FEATURE IS BUILT AROUND, and it is last because every
	// check above has to have passed for this one to mean anything.
	//
	// Inside the prefix, the node re-pins itself on the same wire and comes
	// back reachable — nothing physical happens and the run finishes. Outside
	// it, the node boots with an address that does not exist on the wire it is
	// plugged into. Nothing on this side can reach it, and re-running cannot
	// repair it either: adopt needs the maintenance API, and a node that has
	// installed never serves it again.
	if !prefix.Contains(maintenance) {
		return fmt.Errorf("this node is adopted at %s, but %s would put it on another segment\n\n"+
			"  after the install reboot it would answer at neither: not at %s, which it no "+
			"longer\n  holds, and not at %s, which does not exist on the wire it is plugged "+
			"into.\n\n  Re-running cannot repair that — adopt needs the maintenance API, and an "+
			"installed\n  node never serves it again. Move the machine to its final segment "+
			"FIRST, give it\n  the address there with an ip= kernel argument, and adopt it at "+
			"that address.",
			maintenanceAddr, n.Address, maintenanceAddr, n.Address)
	}

	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cluster -run 'TestCheckNetwork|TestNetworkIP' -v`
Expected: PASS, every subtest.

- [ ] **Step 5: Full suite and vet**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: all `ok`, vet silent, and `git diff --stat go.mod go.sum` empty.

- [ ] **Step 6: Commit**

```bash
git add cluster/network.go cluster/network_test.go
git commit -m "cluster: an address the node cannot come back on is refused, not warned about"
```

---

### Task 3: Render the block into the machine config

`generate.WithNetworkOptions` is machinery's supported entry point (`config/generate/options.go:88`), so no `PatchV1Alpha1` is needed — the install disk selector keeps that path to itself because no generate option reaches it.

**Note on deprecation:** the fields of `v1alpha1.NetworkConfig` carry `Deprecated: use multi-doc network config` comments in v1.13.7. They are deprecated, not removed, `machine.network` is still honoured, and `WithNetworkConfig` is still the only generate-time route to it. Do not chase the multi-doc form; it is not reachable from `generate.Options`.

**Files:**
- Modify: `cluster/network.go` (append)
- Modify: `cluster/config.go:33-91` (the `ConfigInput` struct) and `cluster/config.go:212-214` (beside the console-arg append)
- Modify: `cluster/config_test.go`

**Interfaces:**
- Consumes: `cluster.Network` from Task 2.
- Produces: `ConfigInput.Network *Network`. Task 4 sets it from `UpOptions`.

- [ ] **Step 1: Write the failing tests**

Append to `cluster/config_test.go`:

```go
// testNetwork is the block the target machine uses. Its values are deliberately
// unlike anything else in this file: a fixture that reused 127.0.0.1 could pass
// on a config that carried the loopback endpoint and no network at all.
func testNetwork() *Network {
	return &Network{
		Address:      "192.168.2.10/24",
		Gateway:      "192.168.2.1",
		Nameservers:  []string{"1.1.1.1"},
		HardwareAddr: "84:47:09:47:35:f9",
	}
}

// EVERY assertion here is against v1alpha1Doc, never the raw bytes. Machinery's
// encoder emits commented-out examples, several of which mention hardwareAddr
// and addresses, so a Contains against the raw config matches a comment and
// reports a field that was never set.
func TestGenerateConfigWritesTheStaticNetwork(t *testing.T) {
	in := testInput()
	in.Network = testNetwork()

	doc := v1alpha1Doc(t, mustGenerate(t, in).ControlPlane)

	for _, want := range []string{
		"hardwareAddr: 84:47:09:47:35:f9",
		"192.168.2.10/24",
		"gateway: 192.168.2.1",
		"1.1.1.1",
		"dhcp: false",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the generated config does not carry %q\n"+
				"  reason: the installed system writes its own kernel cmdline and inherits\n"+
				"  nothing from the ISO, so an address that is not in this config is gone\n"+
				"  the moment the node reboots", want)
		}
	}

	// The DEFAULT ROUTE, not merely a gateway value. A gateway on a route to
	// some other network reads identically field by field and leaves the node
	// with no way off its segment.
	if !defaultRoute.MatchString(doc) {
		t.Errorf("no default route through the gateway:\n%s", redact(doc))
	}
}

// defaultRoute matches a route whose destination is everything. The pairing is
// the point: `gateway:` anywhere in the file proves nothing about which
// destination it serves.
var defaultRoute = regexp.MustCompile(`network: 0\.0\.0\.0/0\n\s+gateway: 192\.168\.2\.1`)

// REQUIREMENT 2, and the regression target for every machine that existed
// before this feature. Absent means DHCP, which means machinery's own defaults
// and not one byte from this branch.
func TestGenerateConfigEmitsNoNetworkWithoutABlock(t *testing.T) {
	doc := v1alpha1Doc(t, mustGenerateDefault(t).ControlPlane)

	// Three markers that can ONLY come from networkOption. A `network:` key by
	// itself is machinery's, and asserting on that would fail for a reason
	// this branch never caused.
	for _, never := range []string{"hardwareAddr:", "dhcp: false", "0.0.0.0/0"} {
		if strings.Contains(doc, never) {
			t.Errorf("a machine with no network block still got %q in its config\n"+
				"  reason: every QEMU machine and every DHCP node takes this path, and a\n"+
				"  static interface appearing in it is a node that stops answering", never)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cluster -run TestGenerateConfig -v`
Expected: FAIL to compile — `in.Network undefined`.

- [ ] **Step 3: Append `networkOption` to `cluster/network.go`**

Add these imports to the existing block:

```go
	"github.com/siderolabs/talos/pkg/machinery/config/generate"
	"github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
```

```go
// networkOption renders a Network into the one generate option that reaches
// machine.network.
//
// generate.WithNetworkOptions is machinery's supported entry point, so this
// needs no PatchV1Alpha1 — unlike the install disk selector, which has no
// generate option at all and is why that patch exists.
//
// The fields of v1alpha1.NetworkConfig are marked deprecated in favour of a
// multi-doc network config. They are deprecated, not removed, machine.network
// is still honoured, and the multi-doc form is not reachable from
// generate.Options at all. When it becomes reachable, this function is the one
// place that has to change.
func networkOption(n *Network) generate.Option {
	// A POINTER TO FALSE, not a nil pointer. Nil omits the key, and the config
	// then says nothing about DHCP on an interface whose whole purpose is not
	// using it — true in effect, silent in the artifact an operator reads.
	dhcp := false

	return generate.WithNetworkOptions(v1alpha1.WithNetworkConfig(&v1alpha1.NetworkConfig{
		NameServers: n.Nameservers,
		NetworkInterfaces: v1alpha1.NetworkDeviceList{{
			// A SELECTOR rather than DeviceInterface. The two are mutually
			// exclusive in machinery, and the name is the identity that moves
			// when the card does.
			DeviceSelector: &v1alpha1.NetworkDeviceSelector{
				NetworkDeviceHardwareAddress: n.HardwareAddr,
			},
			DeviceAddresses: []string{n.Address},
			DeviceDHCP:      &dhcp,
			// The DEFAULT route. Talos derives the on-link route from the
			// address's prefix by itself; what it cannot derive is the way off
			// the segment, and that is the half that makes a node able to pull
			// an image.
			DeviceRoutes: []*v1alpha1.Route{{
				RouteNetwork: "0.0.0.0/0",
				RouteGateway: n.Gateway,
			}},
		}},
	}))
}
```

- [ ] **Step 4: Add the field to `ConfigInput` and the append**

In `cluster/config.go`, after `DisableKexec` in the `ConfigInput` struct:

```go
	// Network is the node's static addressing, or nil for DHCP.
	//
	// NIL IS A REAL ANSWER, not a missing field: with nil, no machine.network
	// section is emitted at all and the node behaves exactly as every machine
	// did before this field existed.
	//
	// Its address is NOT the same question as APIAddress, and the caller is
	// what resolves one to the other. This package is handed the address a
	// client dials; whether that address came from a static block or from the
	// endpoint the node already answers on is the caller's knowledge.
	Network *Network
```

Beside the console-arg append at `config.go:212-214`:

```go
	// OPTIONAL, like the console argument above and for the same shape of
	// reason: a machine on a segment that serves DHCP needs nothing here, and
	// emitting a network section for it would replace working defaults with a
	// guess about its NIC.
	if in.Network != nil {
		genOpts = append(genOpts, networkOption(in.Network))
	}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cluster -run TestGenerateConfig -v`
Expected: PASS.

If `TestGenerateConfigWritesTheStaticNetwork` fails on the `defaultRoute` regex, print the doc to see the encoder's actual indentation before adjusting: add `t.Log(redact(doc))` temporarily. Adjust the regex's whitespace class, never the field names.

- [ ] **Step 6: Full suite and vet**

Run: `go build ./... && go test ./... && go vet ./...`
Expected: all `ok`. `git diff --stat go.mod go.sum` empty.

- [ ] **Step 7: Commit**

```bash
git add cluster/network.go cluster/config.go cluster/config_test.go
git commit -m "cluster: a static address the installed system keeps"
```

---

### Task 4: Route the two addresses through `Up`

**DEVIATION FROM THE DESIGN SPEC, deliberate.** The design proposed `UpOptions.InstalledEndpoint string`. Do not add it. A second field holding the post-install endpoint would compile, read correctly, and be settable to an address the node will never hold — the exact defect class `CheckNetwork` exists to remove, reintroduced one layer down. `Up` derives it instead, from `Network.IP()` and the port the caller already dials, because apid serves the same port before and after the install. Update the design doc's D2 code block to match as part of this task.

**Files:**
- Modify: `cluster/up.go` — `UpOptions` (add `Network`), `Up` (:196-220 refusals, :331-377 the two branches, :389-460 steps 8-10), `configure` (:472-560)
- Modify: `cluster/up_test.go` — `recorder` (:60-107), `newFixture` (:189-222), new tests

**Interfaces:**
- Consumes: `cluster.Network`, `CheckNetwork` (Task 2); `ConfigInput.Network` (Task 3).
- Produces:
  - `UpOptions.Network *Network` — set by `cmd/tinq/adopt.go` in Task 7
  - `func installedEndpoint(endpoint string, n *Network) (addr, hostPort string, err error)` — unexported

- [ ] **Step 1: Extend the recorder to capture endpoints**

In `cluster/up_test.go`, add a field to `recorder` and a second recording method. The existing `call` records payloads; endpoints are a different question and get their own map so a test can assert on both.

```go
	// endpoint is the address each operation was pointed at. A node with a
	// static block ANSWERS AT TWO ADDRESSES over its life, and every hook below
	// takes a string — so pointing bootstrap at the maintenance address still
	// COMPILES, and fails minutes later against a node that stopped holding it.
	endpoint map[string]string
```

```go
func (r *recorder) at(name, endpoint string, payload ...[]byte) error {
	if r.endpoint == nil {
		r.endpoint = map[string]string{}
	}

	r.endpoint[name] = endpoint

	return r.call(name, payload...)
}
```

Then change the five endpoint-taking hooks in `hooks()` to record it:

```go
		waitMaintenance: func(_ context.Context, endpoint string, _ time.Duration) error {
			return r.at("waitMaintenance", endpoint)
		},
		applyConfig: func(_ context.Context, endpoint string, config []byte) error {
			return r.at("applyConfig", endpoint, config)
		},
		waitBootstrapReady: func(_ context.Context, talosconfig []byte, endpoint string, _ time.Duration) error {
			return r.at("waitBootstrapReady", endpoint, talosconfig)
		},
		bootstrap: func(_ context.Context, talosconfig []byte, endpoint string) error {
			return r.at("bootstrap", endpoint, talosconfig)
		},
		kubeconfig: func(_ context.Context, talosconfig []byte, endpoint string) ([]byte, error) {
			if err := r.at("kubeconfig", endpoint, talosconfig); err != nil {
				return nil, err
			}

			return []byte(fakeKubeconfig), nil
		},
```

- [ ] **Step 2: Write the failing tests**

Append to `cluster/up_test.go`:

```go
// The two fixture addresses are deliberately UNRELATED to each other and to
// every other value in this file. Both are inside one /24 so CheckNetwork
// accepts them — a same-wire re-pin, which is the only kind that can finish —
// and they differ in the last octet so a hook handed the wrong one cannot pass.
const (
	fakeMaintenanceEndpoint = "10.99.0.186:50000"
	fakeInstalledEndpoint   = "10.99.0.7:50000"
)

func fakeStaticNetwork() *Network {
	return &Network{
		Address:      "10.99.0.7/24",
		Gateway:      "10.99.0.1",
		Nameservers:  []string{"10.99.0.53"},
		HardwareAddr: "84:47:09:47:35:f9",
	}
}

// THE SEAM, not the function. Everything about the two addresses is correct in
// cluster/network.go and worth nothing if Up hands the wrong one to a hook —
// which compiles, because all five take a string.
func TestUpDialsMaintenanceBeforeTheInstallAndTheStaticAddressAfter(t *testing.T) {
	f := newFixture(t)
	f.opts.TalosEndpoint = fakeMaintenanceEndpoint
	f.opts.Network = fakeStaticNetwork()

	f.mustRun(t)

	before := []string{"waitMaintenance", "applyConfig"}
	for _, op := range before {
		if got := f.rec.endpoint[op]; got != fakeMaintenanceEndpoint {
			t.Errorf("%s dialled %q, want %q\n"+
				"  reason: before the install the node holds ONLY the maintenance address",
				op, got, fakeMaintenanceEndpoint)
		}
	}

	after := []string{"waitBootstrapReady", "bootstrap", "kubeconfig"}
	for _, op := range after {
		if got := f.rec.endpoint[op]; got != fakeInstalledEndpoint {
			t.Errorf("%s dialled %q, want %q\n"+
				"  reason: the node rebooted into what it installed and stopped holding %s;\n"+
				"  a wait pointed there can only spend its whole budget",
				op, got, fakeInstalledEndpoint, fakeMaintenanceEndpoint)
		}
	}
}

// The certificate names what the client dials, and after the install that is
// the static address. Named wrong, the node installs, boots, serves apid, and
// every authenticated call fails on a certificate nobody can point at.
func TestUpPutsTheStaticAddressInTheCertificate(t *testing.T) {
	f := newFixture(t)
	f.opts.TalosEndpoint = fakeMaintenanceEndpoint
	f.opts.Network = fakeStaticNetwork()

	f.mustRun(t)

	if got := f.rec.input.APIAddress; got != "10.99.0.7" {
		t.Errorf("APIAddress = %q, want 10.99.0.7\n"+
			"  reason: this becomes apid's subject alt name AND the talosconfig endpoint,\n"+
			"  both baked at generation time and unrepairable afterwards", got)
	}

	if f.rec.input.Network == nil {
		t.Error("the network block never reached ConfigInput\n" +
			"  reason: a correct networkOption that nothing calls emits nothing")
	} else if got := f.rec.input.Network.Address; got != "10.99.0.7/24" {
		t.Errorf("ConfigInput.Network.Address = %q, want 10.99.0.7/24", got)
	}
}

// EVERY MACHINE THAT EXISTED BEFORE THIS FEATURE takes this path, including
// every QEMU one. With no block the node does not move, so both halves dial the
// same address and the config carries no network at all.
func TestUpDialsOneAddressForAMachineWithNoNetworkBlock(t *testing.T) {
	f := newFixture(t)

	f.mustRun(t)

	for op, got := range f.rec.endpoint {
		if got != f.opts.TalosEndpoint {
			t.Errorf("%s dialled %q, want %q — a DHCP node does not move",
				op, got, f.opts.TalosEndpoint)
		}
	}

	if f.rec.input.Network != nil {
		t.Error("ConfigInput.Network is set for a machine with no network block")
	}
}

// Refused where the two endpoint refusals already are: BEFORE Boot. Failing
// here costs nothing; failing after costs a VM nobody asked to keep and, on
// hardware, a node that has already been told to install.
func TestUpRefusesAnUnreachableStaticAddressBeforeBooting(t *testing.T) {
	f := newFixture(t)
	f.opts.TalosEndpoint = "192.168.1.186:50000"
	f.opts.Network = fakeStaticNetwork() // 10.99.0.7/24 — another segment entirely

	err := f.run(t)
	if err == nil {
		t.Fatal("Up accepted a static address on a different segment than the node it is adopting")
	}

	if f.booted != 0 {
		t.Error("Up booted the machine before refusing\n" +
			"  reason: the refusal is provable from the options alone, and reaching it later\n" +
			"  leaves residue for a verdict that was free")
	}

	if !strings.Contains(err.Error(), "10.99.0.7/24") {
		t.Errorf("the refusal does not name the address that caused it: %s", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./cluster -run 'TestUpDials|TestUpPutsTheStatic|TestUpRefusesAnUnreachable' -v`
Expected: FAIL to compile — `f.opts.Network undefined`.

- [ ] **Step 4: Add the field and the derivation to `cluster/up.go`**

In `UpOptions`, after `DisableKexec`:

```go
	// Network is the node's static addressing, or nil for DHCP.
	//
	// There is NO companion field for the address the node answers on
	// afterwards, and there must not be one: it is derived below. A second
	// field holding it would compile, read correctly, and be settable to an
	// address the node will never hold — which is the defect CheckNetwork
	// exists to refuse, reintroduced one layer down.
	Network *Network
```

Add the derivation beside `apiAddress` at the bottom of the file:

```go
// installedEndpoint is where the node answers AFTER the install reboot, as both
// a bare address — for apid's certificate and the talosconfig — and a dialable
// host:port.
//
// With no static block the node does not move, and both are the maintenance
// endpoint it already answers on. With one, the host changes and the PORT DOES
// NOT: apid serves 50000 before and after the install, so reusing the caller's
// port is not a shortcut, it is the fact.
func installedEndpoint(endpoint string, n *Network) (addr, hostPort string, err error) {
	if addr, err = apiAddress(endpoint); err != nil {
		return "", "", err
	}

	if n == nil {
		return addr, endpoint, nil
	}

	if addr, err = n.IP(); err != nil {
		return "", "", err
	}

	_, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("the Talos endpoint %q is not host:port: %w", endpoint, err)
	}

	return addr, net.JoinHostPort(addr, port), nil
}
```

- [ ] **Step 5: Wire it through `Up` and `configure`**

In `Up`, immediately after the `opts.KubeEndpoint == ""` refusal at line 215-220 and BEFORE `p := &printer{w: out}`:

```go
	// Refused here, beside the two above, and for the same reason: it is
	// provable from the options alone. Reaching it after Boot spends a VM and a
	// state dir on a verdict that was free — and on hardware it would spend a
	// node that has already been told to install.
	//
	// CheckNetwork is given the MAINTENANCE address, because that is the one
	// that has to sit inside the static prefix. Handing it the installed
	// address instead would compare a value to itself and pass everything.
	maintenanceAddr, err := apiAddress(opts.TalosEndpoint)
	if err != nil {
		return err
	}

	if err := CheckNetwork(opts.Network, maintenanceAddr); err != nil {
		return err
	}

	installedAddr, installed, err := installedEndpoint(opts.TalosEndpoint, opts.Network)
	if err != nil {
		return err
	}
```

`err` is declared here for the first time in `Up`, so the later `checked, err := CheckVersion(...)` still compiles unchanged — `checked` is new on that line. Confirm with `go vet`.

Then replace `opts.TalosEndpoint` with `installed` at exactly these call sites:
- line 355, the already-configured branch: `hooks.waitBootstrapReady(ctx, talosconfig, installed, installTimeout)`
- line 389: `hooks.bootstrap(ctx, talosconfig, installed)`
- the `kubeconfig` call in step 9 (search for `hooks.kubeconfig(`)

Leave `hooks.waitMaintenance(ctx, opts.TalosEndpoint, maintenanceTimeout)` at line 368 alone — that is the before half.

Change `configure`'s signature and its two uses of the endpoint:

```go
func configure(ctx context.Context, hooks *upHooks, opts UpOptions, p *printer, installedAddr, installed string) ([]byte, error) {
	// ── 6/10 config ─────────────────────────────────────────────────────────
	generated, err := hooks.generateConfig(ConfigInput{
		ClusterName:      opts.ClusterName,
		Endpoint:         opts.KubeEndpoint,
		APIAddress:       installedAddr,
		TalosVersion:     opts.TalosVersion,
		ConsoleArg:       opts.ConsoleArg,
		SystemDiskSerial: opts.SystemDiskSerial,
		DataDiskSerial:   opts.DataDiskSerial,
		DisableKexec:     opts.DisableKexec,
		// The address a client dials AFTER the install is derived from this
		// block by the caller, so the certificate above and the address below
		// cannot name two different hosts.
		Network: opts.Network,
	})
```

Delete the `addr, err := apiAddress(opts.TalosEndpoint)` block at the top of `configure`; `Up` owns that call now. Update the call site at line 374 to `configure(ctx, hooks, opts, p, installedAddr, installed)`.

The `waitBootstrapReady` at line 553 inside `configure` becomes `installed`. The `applyConfig` at line 545 stays `opts.TalosEndpoint`.

- [ ] **Step 6: Announce it in the transcript**

In `configure`, after the `DisableKexec` detail block and before the `DataDiskSerial` one:

```go
	if opts.Network != nil {
		p.detail("network: %s via %s on %s, dhcp off", opts.Network.Address,
			opts.Network.Gateway, opts.Network.HardwareAddr)
		p.detail("  the installed system writes its own cmdline and inherits nothing from the")
		p.detail("  ISO, so an address that is not in this config is gone at the install reboot")

		// PRINTED ONLY WHEN IT IS TRUE. On a segment with no DHCP the operator
		// gave the node its final address at the GRUB prompt and nothing moves;
		// saying it moved would be a claim about an address change that is not
		// happening.
		if installed != opts.TalosEndpoint {
			p.detail("  this node MOVES: adopted at %s, answers at %s from the reboot onward",
				opts.TalosEndpoint, installed)
		}
	}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./cluster -v`
Expected: PASS, including every pre-existing `TestUp*`. If `TestUpAnnouncesTheReasonForEveryNonObviousDecision` or `TestUpPrintsTheTenAnnouncedStepsInOrder` fails, you changed transcript lines that a DHCP machine prints — the new detail block must be inside the `opts.Network != nil` gate.

- [ ] **Step 8: Update the design doc**

In `docs/superpowers/specs/2026-08-02-static-network-design.md`, replace the `InstalledEndpoint` field block and the `installed := opts.InstalledEndpoint` snippet in the `cluster/up.go` section with `UpOptions.Network *Network` and `installedEndpoint(endpoint, n)`, and state that the post-install endpoint is derived rather than configured. Leave the routing table as it is — it is still correct.

- [ ] **Step 9: Full suite, vet, commit**

Run: `go build ./... && go test ./... && go vet ./...`

```bash
git add cluster/up.go cluster/up_test.go docs/superpowers/specs/2026-08-02-static-network-design.md
git commit -m "cluster: the address a node answers on after the install is the one it is dialled at"
```

---

### Task 5: Ask the node what NICs it has

`ListDisks` with a different type parameter. The disk trio is the model and this mirrors it deliberately: same maintenance client, same `safe.StateListAll`, same "print the table and refuse" remedy, because a NIC without carrier strands a machine exactly the way a wrong disk serial overwrites one.

**THE ONE UNPROVEN ASSUMPTION IN THIS BRANCH:** whether maintenance mode AUTHORIZES reading `LinkStatuses` is a fact about the Talos server, not about machinery, and it cannot be read out of the module. Step 1 writes the live assertion. **Run it against a real node before trusting the pre-flight in Task 7.** If it is denied, `RequireLink` still works — it is a pure function over a slice — but the table cannot be fetched, and the operator copies the MAC off the node's console dashboard instead.

**Files:**
- Modify: `cluster/nodeinfo.go` (append after `RequireDisk`)
- Modify: `cluster/nodeinfo_test.go`
- Modify: `cluster/client_test.go:848` (`TestAgainstARealNode`)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Link struct { ID, HardwareAddr, PermanentAddr, Driver, OperState string; Carrier, Physical bool }`
  - `func ListLinks(ctx context.Context, endpoint string) ([]Link, error)`
  - `func FormatLinks(links []Link) string`
  - `func RequireLink(links []Link, hardwareAddr string) error`
  - `func toLinks(ls []*netres.LinkStatus) []Link` — unexported, the testable half

- [ ] **Step 1: Extend the live gate first**

In `cluster/client_test.go`, inside `TestAgainstARealNode`, after the disks block, add:

```go
	// THE ONE ASSUMPTION THIS BRANCH COULD NOT PROVE FROM SOURCE. Whether
	// maintenance mode serves LinkStatuses is a fact about the Talos server,
	// and machinery holds no answer. If this fails, adopt cannot print a links
	// table and spec.baremetal.network.hardwareAddr has to be copied off the
	// node's own console instead.
	//
	// Not fatal, for the same reason the version question above is not: it has
	// a documented fallback, and aborting here would cost the rest of the run.
	links, err := safe.StateListAll[*netres.LinkStatus](ctx, c.COSI)

	switch {
	case err != nil:
		t.Errorf("listing LinkStatuses over COSI: %s\n"+
			"  reason: adopt's links table and its carrier check both depend on this call",
			redactErr(err))
	case links.Len() == 0:
		t.Error("the node reports no links at all")
	default:
		// Nothing here is generated material, so it is logged unredacted: this
		// is the evidence.
		t.Logf("links:\n%s", FormatLinks(toLinks(slices.Collect(links.All()))))
	}
```

Add the import: `netres "github.com/siderolabs/talos/pkg/machinery/resources/network"`.

- [ ] **Step 2: Write the failing unit tests**

Append to `cluster/nodeinfo_test.go`:

```go
// testLinks is the target machine's real shape: two physical NICs, one up and
// one down, plus the loopback every node has. The DOWN one is the point — a
// name or a MAC typo that lands on it strands the machine just as thoroughly as
// a wrong disk serial overwrites one.
func testLinks() []Link {
	return []Link{
		{ID: "enp1s0", HardwareAddr: "84:47:09:47:35:f9", Driver: "igc", OperState: "up", Carrier: true, Physical: true},
		{ID: "enp2s0", HardwareAddr: "84:47:09:47:35:f8", Driver: "igc", OperState: "down", Carrier: false, Physical: true},
	}
}

func TestRequireLinkAcceptsALinkWithCarrier(t *testing.T) {
	if err := RequireLink(testLinks(), "84:47:09:47:35:f9"); err != nil {
		t.Errorf("the node's only NIC with carrier was refused: %s", err)
	}
}

func TestRequireLinkIsCaseInsensitive(t *testing.T) {
	// A MAC copied out of a datasheet or a switch's web UI is upper case, and
	// the node reports lower. Refusing that is a refusal over presentation.
	if err := RequireLink(testLinks(), "84:47:09:47:35:F9"); err != nil {
		t.Errorf("an upper-case MAC was refused: %s", err)
	}
}

func TestRequireLinkRefusesALinkWithNoCarrier(t *testing.T) {
	err := RequireLink(testLinks(), "84:47:09:47:35:f8")
	if err == nil {
		t.Fatal("a NIC with no carrier was accepted\n" +
			"  reason: the node installs, reboots, brings up a cable that is not there,\n" +
			"  and is never heard from again")
	}

	if !strings.Contains(err.Error(), "enp2s0") {
		t.Errorf("the refusal does not name the link it found: %s", err)
	}
}

func TestRequireLinkRefusesAMACThisNodeDoesNotHave(t *testing.T) {
	err := RequireLink(testLinks(), "00:00:00:00:00:01")
	if err == nil {
		t.Fatal("a MAC matching none of the node's links was accepted")
	}

	// The table IS the remedy: without talosctl there is no other way to learn
	// this node's MACs.
	for _, want := range []string{"enp1s0", "84:47:09:47:35:f9", "enp2s0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not print %s, so it cannot be acted on:\n%s", want, err)
		}
	}
}

func TestRequireLinkRefusesAnEmptyMAC(t *testing.T) {
	// DEFENSIVE, and deliberately kept. CheckNetwork refuses an empty
	// hardwareAddr from the manifest before adopt ever reaches the node, so
	// this arm is reachable only by a direct caller of ListLinks. It stays
	// because the table is the right answer to "which one" no matter who asks,
	// and because a future caller that skips CheckNetwork must not get an
	// interface selected by an empty MAC.
	err := RequireLink(testLinks(), "")
	if err == nil {
		t.Fatal("no hardwareAddr was accepted")
	}

	if !strings.Contains(err.Error(), "84:47:09:47:35:f9") {
		t.Errorf("the first-run refusal does not print the table:\n%s", err)
	}
}

func TestRequireLinkRefusesANodeWithNoLinks(t *testing.T) {
	err := RequireLink(nil, "84:47:09:47:35:f9")
	if err == nil {
		t.Fatal("a node reporting no links at all was accepted")
	}

	// The remedy is NOT "choose one" — there is nothing to choose from.
	if strings.Contains(err.Error(), "DEVICE") {
		t.Error("the refusal prints an empty table as if it were a menu")
	}
}
```

- [ ] **Step 3: Run to verify they fail**

Run: `go test ./cluster -run TestRequireLink -v`
Expected: FAIL to compile — `undefined: Link`, `undefined: RequireLink`.

- [ ] **Step 4: Implement, appended to `cluster/nodeinfo.go`**

Add the import `netres "github.com/siderolabs/talos/pkg/machinery/resources/network"` and `"net"`.

```go
// Link is one of a node's network interfaces, reduced to what CHOOSING one
// needs. A struct of our own rather than machinery's LinkStatusSpec, for the
// reason Disk is one: the table below cannot drift with a field we never render.
type Link struct {
	ID            string
	HardwareAddr  string
	PermanentAddr string
	Driver        string
	OperState     string
	Carrier       bool
	Physical      bool
}

// ListLinks asks a maintenance-mode node what network interfaces it has.
//
// The same call shape as ListDisks, against the same maintenance client. Whether
// maintenance mode authorizes this resource is asserted in TestAgainstARealNode
// rather than assumed here — see that test for the fallback.
func ListLinks(ctx context.Context, endpoint string) ([]Link, error) {
	c, err := MaintenanceClient(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	defer c.Close() //nolint:errcheck

	list, err := safe.StateListAll[*netres.LinkStatus](ctx, c.COSI)
	if err != nil {
		return nil, fmt.Errorf("listing the node's network links: %w", err)
	}

	return toLinks(slices.Collect(list.All())), nil
}

// toLinks reduces machinery's link resources to the fields the table renders,
// in a deterministic order.
//
// Split out of ListLinks for the reason toDisks is split out of ListDisks:
// reaching it through ListLinks takes a node, so the only half of that function
// with any decisions in it would be asserted by nothing. A swapped pair here
// still compiles and still prints a table — one whose MAC column shows drivers.
func toLinks(ls []*netres.LinkStatus) []Link {
	out := make([]Link, 0, len(ls))

	for _, l := range ls {
		s := l.TypedSpec()

		// PHYSICAL ONLY. Talos reports loopback, bonds, bridges and vlans
		// through the same resource, and none of them is a NIC an operator can
		// plug a cable into. Physical() is machinery's own predicate for it.
		if !s.Physical() {
			continue
		}

		out = append(out, Link{
			ID:            l.Metadata().ID(),
			HardwareAddr:  net.HardwareAddr(s.HardwareAddr).String(),
			PermanentAddr: net.HardwareAddr(s.PermanentAddr).String(),
			Driver:        s.Driver,
			OperState:     s.OperationalState.String(),
			// CARRIER, not "up". A link can be administratively up with no
			// cable in it, and that is exactly the NIC an operator picks by
			// mistake on a two-port box.
			Carrier:  s.LinkState,
			Physical: true,
		})
	}

	// COSI's list order is not a promise, and this table is read by a human
	// copying a MAC out of it.
	slices.SortFunc(out, func(a, b Link) int { return strings.Compare(a.ID, b.ID) })

	return out
}

// linkRow is shared by the header and the rows beneath it because they are one
// table: widen a column in only one and the header slides off its values with
// nothing to fail.
const linkRow = "  %-10s %-19s %-10s %-8s %s\n"

// FormatLinks renders the table that is the REMEDY for the refusals below.
// Without talosctl there is no other way to learn this node's MACs, so it is
// not diagnostic decoration — it is the only path forward.
func FormatLinks(links []Link) string {
	var b strings.Builder

	fmt.Fprintf(&b, linkRow, "DEVICE", "MAC", "DRIVER", "STATE", "NOTES")

	for _, l := range links {
		var notes []string

		if l.Carrier {
			notes = append(notes, "carrier — a cable is in it")
		} else {
			notes = append(notes, "NO CARRIER — nothing plugged in, or the far end is down")
		}

		// Printed only when it DIFFERS. Equal is the normal case and a second
		// identical MAC in the row is noise a reader has to rule out.
		if l.PermanentAddr != "" && l.PermanentAddr != l.HardwareAddr {
			notes = append(notes, "permanent "+l.PermanentAddr)
		}

		fmt.Fprintf(&b, linkRow, l.ID, l.HardwareAddr, l.Driver, l.OperState, strings.Join(notes, ", "))
	}

	return b.String()
}

// RequireLink refuses unless hardwareAddr names a link this node has AND that
// link has carrier.
//
// THREE refusals, and the first two share one table because they are the same
// remedy. Empty is a first run. Unmatched is a typo — the realistic failure and
// the expensive one, because Talos with a selector matching nothing configures
// nothing and the node comes back with no address at all. No carrier is the
// third and it is the one this repo's target machine invites: two ports, one
// cabled, and the wrong choice is invisible until the install reboot.
func RequireLink(links []Link, hardwareAddr string) error {
	// Before any of them, because every refusal below ends by telling the
	// reader to copy a MAC out of the table, and with no links that table is a
	// header over nothing.
	if len(links) == 0 {
		return errors.New("the node reports no physical network links at all, so no NIC can be chosen\n\n" +
			"  a MAC cannot be picked from an empty list. Check that this machine has an\n" +
			"  ethernet interface its kernel can see, then run adopt again")
	}

	if hardwareAddr == "" {
		return fmt.Errorf("no hardwareAddr given for the static network, and one cannot be guessed\n\n"+
			"this node's links:\n\n%s\n"+
			"  put the MAC of the cabled one in spec.baremetal.network.hardwareAddr, then run\n"+
			"  adopt again", FormatLinks(links))
	}

	for _, l := range links {
		// EqualFold, because a MAC copied from a switch UI or a datasheet is
		// upper case and the node reports lower. Refusing that is a refusal
		// over presentation, and the remedy would read as a typo hunt.
		if !strings.EqualFold(l.HardwareAddr, hardwareAddr) {
			continue
		}

		if !l.Carrier {
			return fmt.Errorf("hardwareAddr %s is this node's %s, which has NO CARRIER\n\n"+
				"this node's links:\n\n%s\n"+
				"  nothing is plugged into it, or the far end is down. Configured anyway, the\n"+
				"  node installs, reboots, brings up a link that cannot pass traffic, and is\n"+
				"  never heard from again.", hardwareAddr, l.ID, FormatLinks(links))
		}

		return nil
	}

	return fmt.Errorf("hardwareAddr %s matches none of this node's links\n\n"+
		"this node's links:\n\n%s\n"+
		"  a MAC that matches nothing is almost always a typo. Talos does not report it as\n"+
		"  one: it configures no interface, and the node comes back with no address.",
		hardwareAddr, FormatLinks(links))
}
```

Add `"errors"` to the import block if it is not already there.

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./cluster -run TestRequireLink -v`
Expected: PASS.

- [ ] **Step 6: Full suite, vet, commit**

Run: `go build ./... && go test ./... && go vet ./...`

```bash
git add cluster/nodeinfo.go cluster/nodeinfo_test.go cluster/client_test.go
git commit -m "cluster: a node's links, and the one with a cable in it"
```

- [ ] **Step 7: Record the live gate as owed**

This task cannot prove its own central assumption without hardware. Note in the final report that `TINQ_NODE=<addr>:50000 go test ./cluster -run TestAgainstARealNode -v` is OWED and unrun.

---

### Task 6: The `network` sub-schema

Schema only: `required`, `minLength`, `minItems` and `pattern`. The relational checks stay in Go, because Kubernetes CEL's `cidr()` is API-server-version dependent and a schema must not need a particular cluster version to validate what it claims to.

The existing CEL rules need no edit — neither expression names `endpoint`, they name the VM build fields.

**Files:**
- Modify: `crd/talosmachine.yaml` (inside `spec.baremetal.properties`, after `talosVersion`)
- Modify: `cmd/tinq/crd_test.go`

**Interfaces:**
- Consumes: the renamed key from Task 1.
- Produces: `spec.baremetal.network` with `address`, `gateway`, `nameservers`, `hardwareAddr`. Task 7 reads exactly these names.

- [ ] **Step 1: Write the failing test**

In `cmd/tinq/crd_test.go`, add `"network"` to the field list adopt.go reads:

```go
	for _, f := range []string{"maintenanceEndpoint", "systemDiskSerial", "dataDiskSerial", "consoleArg", "talosVersion", "network"} {
```

And append, before the closing brace of `TestCRDGuardsWhatTheGoCodeAssumes`:

```go
	// The network block is ALL-OR-NOTHING. A schema that lets three of the four
	// through produces a node with a static address and no resolver, or one
	// whose NIC was never named — both of which install and then go silent.
	network := crdMap(t, crdDig(t, props, "network"), "spec.baremetal.network")

	if got, want := crdStrings(t, network["required"], "spec.baremetal.network.required"),
		[]string{"address", "gateway", "nameservers", "hardwareAddr"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spec.baremetal.network.required is %v, want %v — a half-configured static "+
			"network is a node that installs and never answers", got, want)
	}

	netProps := crdMap(t, network["properties"], "spec.baremetal.network.properties")

	for _, f := range []string{"address", "gateway", "nameservers", "hardwareAddr"} {
		if _, ok := netProps[f]; !ok {
			t.Errorf("spec.baremetal.network.%s is missing from the schema, but adopt.go reads it", f)
		}
	}

	// nameservers is a LIST, and `required` on a list is satisfied by an empty
	// one. minItems is what makes that line mean what it reads as — the same
	// gap minLength closes on maintenanceEndpoint.
	if got := fmt.Sprint(crdMap(t, netProps["nameservers"], "…network.nameservers")["minItems"]); got != "1" {
		t.Errorf("spec.baremetal.network.nameservers minItems is %s, want 1 — `required` alone "+
			"accepts an empty list, and a node with no resolver cannot pull an image", got)
	}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/tinq -run TestCRDGuardsWhatTheGoCodeAssumes -v`
Expected: FAIL — `crd/talosmachine.yaml has no spec.baremetal.properties.network`.

- [ ] **Step 3: Add the schema**

In `crd/talosmachine.yaml`, inside `spec.baremetal.properties` after the `talosVersion` block (line 163-167), at the same indentation:

```yaml
                    network:
                      type: object
                      required: [address, gateway, nameservers, hardwareAddr]
                      description: >-
                        Static addressing for a segment that serves no DHCP. ABSENT
                        MEANS DHCP, which is what every node had before this block
                        existed and what every QEMU machine still uses. All four fields
                        are required together: there is no useful half-configured
                        state, and a static block with no nameservers is a node that
                        cannot resolve a registry and so cannot pull the image it was
                        just told to install.
                      properties:
                        address:
                          type: string
                          minLength: 1
                          # A LOOSE PATTERN ON PURPOSE. It rejects the shapes that are
                          # obviously not CIDR; whether the prefix actually CONTAINS
                          # maintenanceEndpoint is the check that matters, it needs
                          # arithmetic, and it lives in cluster.CheckNetwork — which is
                          # also the only one that runs on the standalone path, where
                          # no apiserver ever sees this schema.
                          pattern: '^[0-9]+(\.[0-9]+){3}/[0-9]{1,2}$'
                          description: >-
                            Where the node answers AFTER the install, in CIDR —
                            192.168.2.10/24. Its prefix MUST contain
                            maintenanceEndpoint: inside it the node re-pins itself on
                            the same wire and comes back reachable, outside it the node
                            boots onto an address that does not exist on the wire it is
                            plugged into, and adopt cannot resume because an installed
                            node never serves the maintenance API again. This address
                            becomes apid's certificate SAN, the talosconfig endpoint
                            and the kubeconfig server.
                        gateway:
                          type: string
                          minLength: 1
                          pattern: '^[0-9]+(\.[0-9]+){3}$'
                          description: >-
                            The default route's next hop. Must be inside address's
                            prefix — a node has no route to a gateway off its own
                            segment.
                        nameservers:
                          type: array
                          minItems: 1
                          items: { type: string }
                          description: >-
                            At least one resolver. A segment with no DHCP offers no
                            resolver either, and there is no other source of one.
                        hardwareAddr:
                          type: string
                          pattern: '^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$'
                          description: >-
                            MAC of the NIC to configure. A MAC and not an interface
                            name, for the reason the install disk is selected by serial
                            and not by size: a stable identity rather than an
                            enumeration artifact. Omit it and the first adopt prints
                            this node's links and refuses, exactly as it does for a
                            missing disk serial.
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/tinq -run TestCRDGuardsWhatTheGoCodeAssumes -v`
Expected: PASS.

- [ ] **Step 5: Verify the CRD still parses as a whole**

Run: `go test ./cmd/tinq -v 2>&1 | tail -20`
Expected: PASS. A YAML indentation slip shows up here as "the CRD does not parse as YAML" or as a missing key elsewhere.

- [ ] **Step 6: Commit**

```bash
git add crd/talosmachine.yaml cmd/tinq/crd_test.go
git commit -m "crd: the four fields a static network needs, required together"
```

---

### Task 7: Read the block and wire the pre-flight

**Files:**
- Modify: `cmd/tinq/adopt.go` — `baremetalKubeEndpoint` (:82), `observeBaremetal` (:204), `adoptMachine` (:219)
- Modify: `cmd/tinq/adopt_test.go`

**Interfaces:**
- Consumes: `cluster.Network`, `cluster.CheckNetwork` (Task 2); `cluster.ListLinks`, `cluster.RequireLink` (Task 5); `UpOptions.Network` (Task 4); the `network` schema (Task 6).
- Produces:
  - `func baremetalNetwork(m *unstructured.Unstructured) (*cluster.Network, error)`
  - `func baremetalInstalledAddr(m *unstructured.Unstructured) string`
  - `func baremetalInstalledEndpoint(m *unstructured.Unstructured) string`
  - Task 8 uses `baremetalNetwork`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/tinq/adopt_test.go`:

```go
// staticMachine is baremetalMachine with the block the target hardware needs.
// The maintenance address and the static address are DELIBERATELY EQUAL, which
// is the no-DHCP case: the operator gave the node its final address at the GRUB
// prompt, so nothing moves.
func staticMachine() *unstructured.Unstructured {
	m := baremetalMachine()
	m.Object["spec"].(map[string]interface{})["baremetal"] = map[string]interface{}{
		"maintenanceEndpoint": "192.168.2.10",
		"systemDiskSerial":    "S1",
		"network": map[string]interface{}{
			"address":      "192.168.2.10/24",
			"gateway":      "192.168.2.1",
			"nameservers":  []interface{}{"1.1.1.1"},
			"hardwareAddr": "84:47:09:47:35:f9",
		},
	}

	return m
}

func TestBaremetalNetworkReadsEveryFieldOutOfTheBlock(t *testing.T) {
	n, err := baremetalNetwork(staticMachine())
	if err != nil {
		t.Fatalf("baremetalNetwork: %s", err)
	}

	if n == nil {
		t.Fatal("the network block was not read at all")
	}

	// EVERY field, because a reader that drops one produces a config that
	// installs and then cannot resolve, or route, or come up at all — and each
	// of those looks like a different bug.
	if n.Address != "192.168.2.10/24" || n.Gateway != "192.168.2.1" ||
		n.HardwareAddr != "84:47:09:47:35:f9" ||
		len(n.Nameservers) != 1 || n.Nameservers[0] != "1.1.1.1" {
		t.Errorf("baremetalNetwork = %+v, want every field of the manifest block", n)
	}
}

func TestBaremetalNetworkIsNilForADHCPMachine(t *testing.T) {
	n, err := baremetalNetwork(baremetalMachine())
	if err != nil {
		t.Fatalf("baremetalNetwork: %s", err)
	}

	if n != nil {
		t.Errorf("a machine with no network block produced %+v, want nil — absent is the "+
			"answer every node gave before this field existed", n)
	}
}

func TestBaremetalNetworkRefusesAMalformedBlock(t *testing.T) {
	m := baremetalMachine()
	m.Object["spec"].(map[string]interface{})["baremetal"].(map[string]interface{})["network"] = "192.168.2.10/24"

	if _, err := baremetalNetwork(m); err == nil {
		t.Error("a scalar `network:` was accepted, and every field read off it would be empty")
	}
}

// THE ENDPOINTS A CLIENT KEEPS. Both are baked into artifacts on disk — the
// talosconfig and the kubeconfig — so pointing them at the maintenance address
// leaves the operator with two files that dial an address the node dropped.
func TestBaremetalEndpointsFollowTheStaticAddress(t *testing.T) {
	m := staticMachine()
	m.Object["spec"].(map[string]interface{})["baremetal"].(map[string]interface{})["maintenanceEndpoint"] = "192.168.2.99"

	if got := baremetalTalosEndpoint(m); got != "192.168.2.99:50000" {
		t.Errorf("talos endpoint = %q, want 192.168.2.99:50000 — before the install the node "+
			"holds only the maintenance address", got)
	}

	if got := baremetalInstalledEndpoint(m); got != "192.168.2.10:50000" {
		t.Errorf("installed endpoint = %q, want 192.168.2.10:50000", got)
	}

	if got := baremetalKubeEndpoint(m); got != "https://192.168.2.10:6443" {
		t.Errorf("kube endpoint = %q, want https://192.168.2.10:6443 — a kubeconfig pointing "+
			"at the maintenance address cannot be used after the install reboot", got)
	}
}

func TestBaremetalEndpointsStayPutWithoutANetworkBlock(t *testing.T) {
	m := baremetalMachine()

	if got := baremetalInstalledEndpoint(m); got != "192.168.1.50:50000" {
		t.Errorf("installed endpoint = %q, want 192.168.1.50:50000 — a DHCP node does not move", got)
	}

	if got := baremetalKubeEndpoint(m); got != "https://192.168.1.50:6443" {
		t.Errorf("kube endpoint = %q, want https://192.168.1.50:6443", got)
	}
}

// Observe reports what a client dials, and after the install that is the static
// address. Reporting the maintenance one puts an address in kubectl output that
// stopped answering at the first reboot.
func TestObserveReportsTheAddressTheNodeKeeps(t *testing.T) {
	_, status, err := observeBaremetal(staticMachine(), t.TempDir())
	if err != nil {
		t.Fatalf("observeBaremetal: %s", err)
	}

	if got := status["apiEndpoint"]; got != "192.168.2.10:50000" {
		t.Errorf("status.apiEndpoint = %v, want 192.168.2.10:50000", got)
	}
}

// The refusal must land BEFORE the ten-minute maintenance wait. Reaching it
// afterwards is ten minutes spent on a verdict that was provable from the file.
func TestAdoptRefusesAnUnreachableStaticAddressWithoutDialling(t *testing.T) {
	// The same on-disk shape TestAdoptRefusesAnEndpointCarryingAPort uses
	// (adopt_test.go:285): a machine file in a temp dir and an hvf rooted in
	// another. There is no helper for this in the package; do not invent one.
	path := filepath.Join(t.TempDir(), "machine.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata: {name: bm0, namespace: default}
spec:
  site: lab
  baremetal:
    maintenanceEndpoint: 192.168.1.186
    systemDiskSerial: S1
    network:
      address: 192.168.2.10/24
      gateway: 192.168.2.1
      nameservers: [1.1.1.1]
      hardwareAddr: 84:47:09:47:35:f9
`), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &hvf{stateRoot: t.TempDir(), imageRoot: t.TempDir()}

	started := time.Now()
	err := adoptMachine(context.Background(), d, path)

	if err == nil {
		t.Fatal("adopt accepted a static address on a segment the node is not on")
	}

	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("adopt took %s to refuse, so it dialled first\n"+
			"  reason: this verdict comes from the manifest alone", elapsed)
	}

	if !strings.Contains(err.Error(), "192.168.2.10/24") {
		t.Errorf("the refusal does not name the address that caused it: %s", err)
	}
}
```

This file has NO `testHVF` or `writeMachineFile` helper and does not need one — `TestAdoptRefusesAnEndpointCarryingAPort` (`adopt_test.go:285`) writes the manifest inline and builds `&hvf{stateRoot: t.TempDir(), imageRoot: t.TempDir()}` on the spot. Follow that. Add `"os"`, `"path/filepath"` and `"time"` to the test imports if they are not there.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/tinq -run 'TestBaremetalNetwork|TestBaremetalEndpoints|TestObserveReports|TestAdoptRefusesAnUnreachable' -v`
Expected: FAIL to compile — `undefined: baremetalNetwork`.

- [ ] **Step 3: Add the readers to `cmd/tinq/adopt.go`**

Beside `baremetalTalosEndpoint`:

```go
// baremetalNetwork reads spec.baremetal.network, or nil when there is none.
//
// nil is a REAL ANSWER — DHCP, which is what every node had before this block
// existed. A malformed block is not: a scalar `network: 192.168.2.10/24` reads
// as every field empty, and a config generated from that would name no
// interface at all.
func baremetalNetwork(m *unstructured.Unstructured) (*cluster.Network, error) {
	raw, present := baremetalFields(m)["network"]
	if !present || raw == nil {
		return nil, nil
	}

	block, ok := raw.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("spec.baremetal.network is not a block of fields\n\n"+
			"  it must be a mapping:\n\n    network:\n      address: 192.168.2.10/24\n"+
			"      gateway: 192.168.2.1\n      nameservers: [1.1.1.1]\n"+
			"      hardwareAddr: 84:47:09:47:35:f9\n\n  (%s)", m.GetName())
	}

	n := &cluster.Network{
		Address:      str(block["address"], ""),
		Gateway:      str(block["gateway"], ""),
		HardwareAddr: str(block["hardwareAddr"], ""),
	}

	// A non-string entry becomes "", which cluster.CheckNetwork refuses as "not
	// an address" — the same complaint it would make about a typo, which is the
	// honest one for a value that is not a resolver either way.
	if list, ok := block["nameservers"].([]interface{}); ok {
		for _, v := range list {
			n.Nameservers = append(n.Nameservers, str(v, ""))
		}
	}

	return n, nil
}

// baremetalInstalledAddr is the BARE address a client dials once the node has
// installed: the static address when there is one, the maintenance address
// otherwise.
//
// The parse error is DISCARDED and the maintenance address stands in, which is
// safe for exactly one reason: adopt refuses an unparseable block up front, so
// the only caller that can reach the fallback is Observe — on a manifest adopt
// would never have accepted. Returning an error here instead would put a
// refusal in a status reporter, which has no way to state one.
func baremetalInstalledAddr(m *unstructured.Unstructured) string {
	maintenance := str(baremetalFields(m)["maintenanceEndpoint"], "")

	n, err := baremetalNetwork(m)
	if err != nil || n == nil {
		return maintenance
	}

	ip, err := n.IP()
	if err != nil {
		return maintenance
	}

	return ip
}

// baremetalInstalledEndpoint is where the node answers AFTER the install.
func baremetalInstalledEndpoint(m *unstructured.Unstructured) string {
	if a := baremetalInstalledAddr(m); a != "" {
		return fmt.Sprintf("%s:%d", a, talosAPIGuestPort)
	}

	return ""
}
```

Change `baremetalKubeEndpoint` to follow the installed address, and say why:

```go
// baremetalKubeEndpoint is the kubeconfig's server, and it follows the address
// the node KEEPS. A kubeconfig written against the maintenance address is a
// file that stops working at the first reboot, on a node that is otherwise fine.
func baremetalKubeEndpoint(m *unstructured.Unstructured) string {
	if a := baremetalInstalledAddr(m); a != "" {
		return fmt.Sprintf("https://%s:%d", a, kubeAPIGuestPort)
	}

	return ""
}
```

In `observeBaremetal`, report the installed endpoint:

```go
		"stateDir": dir, "apiEndpoint": baremetalInstalledEndpoint(m),
```

- [ ] **Step 4: Wire the pre-flight in `adoptMachine`**

After the "must be a bare address with no port" refusal and BEFORE `dir := d.dir(m)`:

```go
	// PARSED AND REFUSED BEFORE ANYTHING IS DIALLED. Every check in
	// CheckNetwork is provable from the file, and the expensive one to discover
	// late is the containment refusal: reaching it after the maintenance wait
	// costs ten minutes for a verdict the manifest already contained.
	network, err := baremetalNetwork(m)
	if err != nil {
		return err
	}

	if err := cluster.CheckNetwork(network, str(spec["maintenanceEndpoint"], "")); err != nil {
		return err
	}
```

After the `dataSerial` disk check and before the version resolution:

```go
	// ASKED ONLY WHEN THERE IS A STATIC BLOCK. A DHCP node's NIC is Talos's
	// business and naming one for it would be a choice nobody asked for.
	//
	// The refusal that matters here is CARRIER: this repo's target machine has
	// two ports with one cable, and a config pointing at the empty one installs,
	// reboots, brings up a link that cannot pass traffic, and goes silent.
	if network != nil {
		links, err := cluster.ListLinks(ctx, endpoint)
		if err != nil {
			return err
		}

		if err := cluster.RequireLink(links, network.HardwareAddr); err != nil {
			return err
		}
	}
```

In the `cluster.Up` call, add the block. `KubeEndpoint` already follows the installed address through `baremetalKubeEndpoint`:

```go
		// The address the node answers on AFTERWARDS is derived from this by
		// cluster.Up, never configured beside it — see installedEndpoint.
		Network: network,
```

- [ ] **Step 5: Run to verify they pass**

Run: `go test ./cmd/tinq -v`
Expected: PASS, including every pre-existing test.

- [ ] **Step 6: Full suite, vet, commit**

Run: `go build ./... && go test ./... && go vet ./...`

```bash
git add cmd/tinq/adopt.go cmd/tinq/adopt_test.go
git commit -m "adopt: the static block, refused from the file before the node is dialled"
```

---

### Task 8: The `ip=` line, on the timeout only

**Files:**
- Modify: `cmd/tinq/adopt.go`
- Modify: `cmd/tinq/adopt_test.go`

**Interfaces:**
- Consumes: `baremetalNetwork` (Task 7).
- Produces: `func kernelCmdlineHint(err error, n *cluster.Network) error`.

- [ ] **Step 1: Write the failing test**

```go
// The NETMASK is the whole reason this is generated rather than documented.
// /24 is the one everybody types correctly; /26 is the one that strands a
// machine, and it is exactly the arithmetic a human does in their head at a
// GRUB prompt with a laptop balanced on a rack rail.
func TestKernelCmdlineHintDerivesTheNetmask(t *testing.T) {
	cases := []struct {
		address string
		want    string
	}{
		{"192.168.2.10/24", "ip=192.168.2.10::192.168.2.1:255.255.255.0::<your-nic>:off"},
		{"192.168.2.10/26", "ip=192.168.2.10::192.168.2.1:255.255.255.192::<your-nic>:off"},
		{"192.168.2.10/16", "ip=192.168.2.10::192.168.2.1:255.255.0.0::<your-nic>:off"},
	}

	for _, c := range cases {
		t.Run(c.address, func(t *testing.T) {
			n := &cluster.Network{Address: c.address, Gateway: "192.168.2.1"}

			got := kernelCmdlineHint(errors.New("gave up waiting"), n)
			if !strings.Contains(got.Error(), c.want) {
				t.Errorf("the hint does not carry %q:\n%s", c.want, got)
			}

			// The original failure must survive. A hint that replaces the error
			// hides which wait actually timed out.
			if !strings.Contains(got.Error(), "gave up waiting") {
				t.Errorf("the hint swallowed the failure it decorates:\n%s", got)
			}
		})
	}
}

func TestKernelCmdlineHintLeavesADHCPFailureAlone(t *testing.T) {
	// A node with no static block was reachable by DHCP or it was not, and an
	// ip= recipe is advice for a problem it does not have.
	want := errors.New("gave up waiting")

	if got := kernelCmdlineHint(want, nil); got != want {
		t.Errorf("the failure was decorated for a machine with no network block:\n%s", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/tinq -run TestKernelCmdlineHint -v`
Expected: FAIL to compile — `undefined: kernelCmdlineHint`.

- [ ] **Step 3: Implement**

In `cmd/tinq/adopt.go`, with `"net"` and `"net/netip"` added to the imports:

```go
// kernelCmdlineHint explains a maintenance-wait timeout for a machine that
// declares a static address, and prints the kernel command line that fixes it.
//
// ONLY on the timeout, because that is the only moment it helps. adopt can run
// at all only once the node is already reachable, which means this line was
// already typed correctly — printing it on a successful run is noise on every
// run where nothing is wrong.
//
// The device field is left blank on purpose. The kernel wants an interface
// NAME and the manifest holds a MAC, which is the one cost of selecting the NIC
// by a stable identity. Three of the four values still come from the file,
// including the netmask, whose arithmetic is what gets typed wrong on a /26.
func kernelCmdlineHint(err error, n *cluster.Network) error {
	if n == nil {
		return err
	}

	// A block this malformed was already refused by CheckNetwork before
	// anything dialled, so this arm is unreachable through adopt. Returning the
	// original failure is still the right answer: a hint is decoration, and
	// decoration must never replace the error it decorates.
	prefix, perr := netip.ParsePrefix(n.Address)
	if perr != nil {
		return err
	}

	mask := net.IP(net.CIDRMask(prefix.Bits(), 32)).String()

	return fmt.Errorf("%w\n\n"+
		"This machine declares a STATIC address, so the segment it sits on probably serves\n"+
		"no DHCP — and a node booted from the ISO then has no address at all. There is\n"+
		"nothing here to reach until you give it one.\n\n"+
		"The ISO's kernel takes one on its command line. At the GRUB menu press `e`,\n"+
		"append this to the linux line, then Ctrl-X:\n\n"+
		"  ip=%s::%s:%s::<your-nic>:off\n\n"+
		"  fields: client::gateway:netmask:hostname:device:autoconf\n"+
		"  <your-nic> is the interface NAME, e.g. enp1s0 — the kernel wants a name where\n"+
		"  this machine file holds a MAC, and the node's console lists both.\n\n"+
		"That configures the MAINTENANCE boot ONLY. The installed system writes its own\n"+
		"command line and inherits nothing from the ISO, which is what the network block\n"+
		"in this machine file exists to carry.",
		err, prefix.Addr(), n.Gateway, mask)
}
```

Wire it at the maintenance wait in `adoptMachine`:

```go
	if err := cluster.WaitMaintenance(ctx, endpoint, adoptMaintenanceTimeout); err != nil {
		return kernelCmdlineHint(err, network)
	}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/tinq -run TestKernelCmdlineHint -v`
Expected: PASS, all three subtests.

- [ ] **Step 5: Full suite, vet, commit**

Run: `go build ./... && go test ./... && go vet ./...`

```bash
git add cmd/tinq/adopt.go cmd/tinq/adopt_test.go
git commit -m "adopt: the ip= line, printed at the one moment it is worth anything"
```

---

### Task 9: The example and the README

Documentation is not decoration here: `examples/adopt-node.yaml` is what an operator copies, and it is the only place the whole flow appears in order.

**Files:**
- Modify: `examples/adopt-node.yaml`
- Modify: `README.md` (the baremetal manifest at :547)

- [ ] **Step 1: Add the commented static block to the example**

Append to `examples/adopt-node.yaml`, after `dataDiskSerial`:

```yaml
    # STATIC ADDRESSING, for a segment that serves no DHCP. Omit the whole block
    # and the node uses DHCP, which is what this example does and what every
    # QEMU machine does.
    #
    # Its address MUST be on the same segment as maintenanceEndpoint above.
    # Inside that prefix the node re-pins itself on the same wire and comes back
    # reachable; outside it the node boots onto an address that does not exist
    # on the wire it is plugged into, and adopt refuses before applying anything
    # because a re-run cannot repair it — an installed node never serves the
    # maintenance API again.
    #
    # On a segment with no DHCP the node has NO ADDRESS from the ISO, so there
    # is nothing for adopt to dial. Give the maintenance boot one at the GRUB
    # menu: press `e`, append this to the linux line, Ctrl-X.
    #
    #   ip=192.168.2.10::192.168.2.1:255.255.255.0::enp1s0:off
    #
    # That covers the maintenance boot ONLY. The installed system writes its own
    # command line and inherits nothing from the ISO — which is what the block
    # below is for.
    #
    # network:
    #   address: 192.168.2.10/24
    #   gateway: 192.168.2.1
    #   nameservers: [1.1.1.1]
    #   hardwareAddr: 84:47:09:47:35:f9   # adopt prints this node's links on a mismatch
```

- [ ] **Step 2: Add it to the README**

In the baremetal manifest block at `README.md:547`, beneath the existing fields, add the same block in commented form with a one-line lead: `# static addressing — omit for DHCP; the address must be on maintenanceEndpoint's segment`.

- [ ] **Step 3: Verify the example still parses and the QEMU one is untouched**

Run: `go test ./... && git diff --stat examples/bootstrap-machine.yaml`
Expected: tests `ok`, and `bootstrap-machine.yaml` unchanged — it is the regression target and nothing in this branch may edit it.

- [ ] **Step 4: Commit**

```bash
git add examples/adopt-node.yaml README.md
git commit -m "docs: the static block, and the kernel line that gets you to it"
```

---

### Task 10: Make `adopt` resumable — the Tier 2 defect

**This is a separate defect with its own commit. Do not fold it into any task above.**

`adoptMachine` waits for the maintenance API unconditionally (`cmd/tinq/adopt.go`, the `cluster.WaitMaintenance` call) before it ever reaches `cluster.Up`. A node that has already been configured never re-enters maintenance mode, so re-running `adopt` after a failure at step 8, 9 or 10 spends the full ten minutes and then fails.

`Up` is idempotent and prints, on exactly that failure path, "`tinq up` is idempotent: re-running it is the first thing to try" (`cluster/up.go:298`). `adopt`'s pre-flight makes that promise unreachable. This branch also makes it worse: the address it hangs on is the maintenance address, which on a DHCP segment may by then belong to a different host.

`Up` already decides this correctly, from a talosconfig in the state dir (`up.go:321-331`), and calls it a CREDENTIAL rather than a status — nothing is believed about the node, because an authenticated call is impossible without that file. `adopt` uses the same signal for the same reason.

**Files:**
- Modify: `cluster/nodeinfo.go` (a sibling of `NodeVersion`)
- Modify: `cluster/nodeinfo_test.go`
- Modify: `cmd/tinq/adopt.go` (`adoptMachine`)
- Modify: `cmd/tinq/adopt_test.go`

**Interfaces:**
- Consumes: `AuthenticatedClient` (`cluster/client.go:145`), `versionTag` (`cluster/nodeinfo.go:57`).
- Produces: `func InstalledNodeVersion(ctx context.Context, talosconfig []byte, endpoint string) (string, error)`.

- [ ] **Step 1: Write the failing test for the fast path**

Append to `cmd/tinq/adopt_test.go`:

```go
// A RE-RUN MUST NOT WAIT FOR MAINTENANCE MODE. Up is idempotent and its own
// failure message says to re-run; a pre-flight that spends ten minutes proving
// the node left maintenance mode forever makes that advice a trap.
//
// The talosconfig here is deliberately garbage: what is asserted is WHICH path
// adopt took, and a fast failure on a credential it cannot parse proves it took
// the authenticated one. A maintenance wait would still be running.
func TestAdoptDoesNotWaitForMaintenanceOnAConfiguredMachine(t *testing.T) {
	d := &hvf{stateRoot: t.TempDir(), imageRoot: t.TempDir()}
	m := baremetalMachine()

	// d.dir(m) (main.go:507) is the same state directory adoptMachine derives,
	// which is what makes the seeded talosconfig the one it reads.
	dir := d.dir(m)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "talosconfig"), []byte("not a talosconfig"), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "machine.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata: {name: bm0, namespace: default}
spec:
  site: lab
  role: talos-cp
  baremetal:
    maintenanceEndpoint: 192.168.1.50
    systemDiskSerial: S1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	err := adoptMachine(context.Background(), d, path)

	if err == nil {
		t.Fatal("adopt succeeded against a node that does not exist")
	}

	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("adopt spent %s before failing, so it waited for maintenance mode\n"+
			"  reason: an installed node never serves that API again, and Up's own failure\n"+
			"  message tells the operator to re-run", elapsed)
	}

	if strings.Contains(err.Error(), "maintenance") {
		t.Errorf("adopt failed on the maintenance API for a machine that already has a "+
			"talosconfig:\n%s", err)
	}
}
```

Note that `baremetalMachine()` must be updated by Task 1 to use `maintenanceEndpoint`; the inline manifest above uses the same key.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/tinq -run TestAdoptDoesNotWaitForMaintenance -v`
Expected: FAIL — it takes ten minutes, or the error names the maintenance API. Give it `-timeout 15m` so you see the real behaviour rather than a test-framework timeout.

- [ ] **Step 3: Add the authenticated version reader**

In `cluster/nodeinfo.go`, beside `NodeVersion`:

```go
// InstalledNodeVersion asks a node that has ALREADY TAKEN ITS CONFIG for its
// Talos version, over the authenticated API.
//
// The same question NodeVersion asks and the same "" contract, differing only
// in which client can reach the node. It exists because the version guard runs
// at step 3, BEFORE Up's already-configured branch at step 5 — so a resumed
// bring-up still needs a version, and the maintenance API that NodeVersion uses
// is gone for good once a node has installed.
//
// talosconfig is secret and is neither logged nor placed in an error.
func InstalledNodeVersion(ctx context.Context, talosconfig []byte, endpoint string) (string, error) {
	c, err := AuthenticatedClient(ctx, talosconfig, endpoint)
	if err != nil {
		return "", err
	}

	defer c.Close() //nolint:errcheck

	resp, err := c.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("asking the installed node its Talos version: %w", err)
	}

	return versionTag(resp), nil
}
```

- [ ] **Step 4: Gate the pre-flight in `adoptMachine`**

Replace everything from the `log.Printf("waiting for the Talos maintenance API…")` line through the version resolution with:

```go
	// A CREDENTIAL, NOT A STATUS — the same read Up makes at up.go:321 and for
	// the same reason. Nothing about the node is believed on the strength of
	// this file; an authenticated call is simply impossible without it, so
	// having it is a precondition of asking rather than an answer.
	//
	// What it decides here is which API this node can still serve. Everything
	// in the maintenance pre-flight below — the wait, the disks, the links —
	// needs an API that an installed node stopped serving at its first reboot,
	// and running it anyway spends the whole ten-minute budget to discover
	// that. Up is idempotent and its own failure message says to re-run; this
	// is what makes that advice true.
	talosconfig, err := os.ReadFile(filepath.Join(dir, "talosconfig"))

	configured := err == nil

	if !configured && !os.IsNotExist(err) {
		// os.ReadFile's error quotes the PATH and never the contents, so it is
		// safe to wrap. Nothing below may relax that.
		return fmt.Errorf("reading this machine's talosconfig: %w", err)
	}

	// The node's own answer, with the spec as an override for the case Risk 1
	// of the design spec describes: a maintenance-mode node that reports no tag.
	version := str(spec["talosVersion"], "")
	source := "spec.baremetal.talosVersion"

	if configured {
		log.Printf("this machine already has a talosconfig, so the maintenance pre-flight is skipped")

		if version == "" {
			if version, err = cluster.InstalledNodeVersion(ctx, talosconfig, installed); err != nil {
				return err
			}

			source = "the node's authenticated API"
		}
	} else {
		log.Printf("waiting for the Talos maintenance API at %s", endpoint)

		if err := cluster.WaitMaintenance(ctx, endpoint, adoptMaintenanceTimeout); err != nil {
			return kernelCmdlineHint(err, network)
		}

		disks, err := cluster.ListDisks(ctx, endpoint)
		if err != nil {
			return err
		}

		if err := cluster.RequireDisk(disks, systemSerial, "install target"); err != nil {
			return err
		}

		// Checked ONLY when asked for. An absent data disk is a legitimate
		// choice and step 10 announces what it costs; an absent one that was
		// MEANT to be present is a typo, which the same check catches.
		if dataSerial != "" {
			if err := cluster.RequireDisk(disks, dataSerial, "data disk"); err != nil {
				return err
			}
		}

		if network != nil {
			links, err := cluster.ListLinks(ctx, endpoint)
			if err != nil {
				return err
			}

			if err := cluster.RequireLink(links, network.HardwareAddr); err != nil {
				return err
			}
		}

		if version == "" {
			if version, err = cluster.NodeVersion(ctx, endpoint); err != nil {
				return err
			}

			source = "the node's maintenance API"
		}
	}
```

`systemSerial` and `dataSerial` must be read from `spec` ABOVE this block, since both branches close over them. `installed := baremetalInstalledEndpoint(m)` likewise. Add `"path/filepath"` to the imports if it is not already there.

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./cmd/tinq -run TestAdopt -v`
Expected: PASS, and the new test returns in seconds.

- [ ] **Step 6: Full suite, vet, commit**

Run: `go build ./... && go test ./... && go vet ./...`

```bash
git add cluster/nodeinfo.go cluster/nodeinfo_test.go cmd/tinq/adopt.go cmd/tinq/adopt_test.go
git commit -m "adopt: a node that already took its config is asked, not waited for"
```

---

## What this plan does NOT do

Recorded so a reviewer does not read the absence as an oversight:

- **Bonds, VLANs, bridges, MTU, VIP, wireguard, multi-interface, IPv6.** Out of scope per D7 of the design; IPv6 is refused by name in `CheckNetwork`.
- **Reconfiguring an installed node.** `cluster/` is bootstrap-only and that boundary is deliberate.
- **The different-wire move-the-box flow.** Refused, not half-supported. It needs a physical gate mid-run, which does not belong in a non-interactive tool.
- **A QEMU static network.** Structurally impossible: the block lives inside `spec.baremetal`, which the CEL rule at `crd/talosmachine.yaml:79` already forbids on a VM.

## Live gates OWED after the plan is implemented

None of these can run in a development session. Report them as owed and unrun.

1. **`TINQ_NODE=<addr>:50000 go test ./cluster -run TestAgainstARealNode -v`** — settles whether maintenance-mode COSI authorizes `LinkStatuses`. Task 5's central assumption. If it fails, the links table and the carrier check are dropped and `hardwareAddr` comes off the node's console.
2. **`tinq up examples/bootstrap-machine.yaml`** — ten steps to Ready, with the generated config parsed to confirm `machine.network` is absent. The rename touches this path.
3. **On the real box** — boot the ISO on `lan5` with `ip=192.168.2.10::192.168.2.1:255.255.255.0::enp1s0:off`, adopt with the static block, and confirm the node answers at `192.168.2.10` after the install reboot **and that `kubectl` reaches it there**.
4. **The refusal on hardware** — `network.address: 192.168.9.10/24` against a node on `192.168.2.x` must be refused in under a second with nothing applied.

