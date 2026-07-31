package cluster

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/generate/secrets"
)

// testInput is the shape main.go passes: both serials come from the CALLER,
// because the constants live in package main and this package must not
// duplicate them — a duplicated literal drifts, and a drifted install selector
// matches no disk at all.
func testInput() ConfigInput {
	return ConfigInput{
		ClusterName:      "probe",
		Endpoint:         "https://127.0.0.1:6443",
		TalosVersion:     "v1.13.7",
		ConsoleArg:       "console=ttyS0",
		SystemDiskSerial: "talos-system",
		DataDiskSerial:   "talos-data",
	}
}

func mustGenerate(t *testing.T, in ConfigInput) *Generated {
	t.Helper()

	g, err := GenerateConfig(in)
	if err != nil {
		t.Fatalf("GenerateConfig(%+v): %v", in, err)
	}

	return g
}

// Generating a config means generating five certificate authorities, which
// dominates the runtime of this package. The tests that use testInput()
// unchanged share one, read-only.
var defaultGenerated = sync.OnceValues(func() (*Generated, error) { return GenerateConfig(testInput()) })

func mustGenerateDefault(t *testing.T) *Generated {
	t.Helper()

	g, err := defaultGenerated()
	if err != nil {
		t.Fatalf("GenerateConfig(%+v): %v", testInput(), err)
	}

	return g
}

// A Talos machine config is a MULTI-DOCUMENT YAML: v1alpha1 first, then any
// number of separate documents. Asserting on the whole blob cannot tell which
// document a string landed in, which is exactly how a swapped serial survives.
func splitDocs(cp []byte) []string {
	return strings.Split(string(cp), "\n---\n")
}

// The encoder documents every field it sets AND emits a commented-out example
// of most fields it did not. Asserting against that text is how a test for
// `allowSchedulingOnControlPlanes: true` passes on a config that sets it to
// false — the string really is in the file, in a comment. Four mutants survived
// this suite until every assertion was made to read the CODE, not the manual.
var comments = regexp.MustCompile(`(?m)^[ \t]*#.*$|[ \t]+#.*$`)

func code(doc string) string { return comments.ReplaceAllString(doc, "") }

func v1alpha1Doc(t *testing.T, cp []byte) string {
	t.Helper()

	doc := code(splitDocs(cp)[0])
	if !strings.Contains(doc, "version: v1alpha1") {
		t.Fatalf("first document is not the v1alpha1 config:\n%s", doc)
	}

	return doc
}

func docOfKind(t *testing.T, cp []byte, kind string) (string, bool) {
	t.Helper()

	for _, doc := range splitDocs(cp) {
		if doc = code(doc); strings.Contains(doc, "kind: "+kind) {
			return doc, true
		}
	}

	return "", false
}

// installSelector matches an install block that selects BY SERIAL. The
// indentation is the encoder's, and the pairing is the point: `serial:`
// anywhere in the file proves nothing about which disk Talos will install to.
var installSelector = regexp.MustCompile(`(?m)^ {8}diskSelector:\n {12}serial: (\S+)`)

func TestGenerateConfigInstallsToTheSystemDiskBySerial(t *testing.T) {
	doc := v1alpha1Doc(t, mustGenerateDefault(t).ControlPlane)

	m := installSelector.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("install has no diskSelector.serial\n"+
			"  reason: with two large disks a size matcher is a coin flip between the OS target and the data disk\n%s", doc)
	}

	if m[1] != "talos-system" {
		t.Errorf("install selects serial %q, want %q\n"+
			"  reason: installing onto the DATA disk destroys the user volume and leaves the system disk empty", m[1], "talos-system")
	}

	if regexp.MustCompile(`(?m)^ {8}disk: `).MatchString(doc) {
		t.Error("install sets a device path\n" +
			"  reason: /dev/vdX ordering is not stable across boots; the serial is the identity")
	}
}

// The serials belong to package main. If this package ever hardcodes them, the
// two halves drift the moment main.go renames one — and the failure is silent.
func TestGenerateConfigUsesTheCallersSerials(t *testing.T) {
	in := testInput()
	in.SystemDiskSerial = "sys-9000"
	in.DataDiskSerial = "data-9000"

	cp := mustGenerate(t, in).ControlPlane

	if m := installSelector.FindStringSubmatch(v1alpha1Doc(t, cp)); m == nil || m[1] != "sys-9000" {
		t.Errorf("install selector = %v, want serial sys-9000\n"+
			"  reason: the serials are main.go's constants; a literal copied into this package drifts silently", m)
	}

	vol, ok := docOfKind(t, cp, "UserVolumeConfig")
	if !ok || !strings.Contains(vol, `disk.serial == "data-9000"`) {
		t.Errorf("user volume does not select data-9000\n"+
			"  reason: same drift, other half — the volume would match no disk and never appear\n%s", vol)
	}
}

func TestGenerateConfigPinsInstallerToTheImageVersion(t *testing.T) {
	in := testInput()
	// Deliberately NOT the generator's version: an installer pinned to ours
	// turns a fresh install into a silent cross-version upgrade.
	in.TalosVersion = "v1.12.0"

	doc := v1alpha1Doc(t, mustGenerate(t, in).ControlPlane)

	if want := "image: ghcr.io/siderolabs/installer:v1.12.0"; !strings.Contains(doc, want) {
		t.Errorf("install image is not %q\n"+
			"  reason: unset, Talos defaults the installer to the GENERATOR's version and upgrades the node mid-install\n%s",
			want, doc)
	}

	if strings.Contains(doc, "installer:"+GeneratorVersion()) {
		t.Errorf("install image is pinned to the generator version %s\n"+
			"  reason: the installer must follow the ISO, not this binary", GeneratorVersion())
	}
}

func TestGenerateConfigCarriesConsoleArgToTheInstalledSystem(t *testing.T) {
	doc := v1alpha1Doc(t, mustGenerateDefault(t).ControlPlane)

	if !regexp.MustCompile(`(?m)^ {8}extraKernelArgs:\n {12}- console=ttyS0$`).MatchString(doc) {
		t.Errorf("install has no extraKernelArgs console=ttyS0\n"+
			"  reason: the installed system writes its OWN cmdline and does not inherit the ISO's console; "+
			"serial goes dead at exactly the boot you need to watch\n%s", doc)
	}

	if regexp.MustCompile(`(?m)^ {8}grubUseUKICmdline: true`).MatchString(doc) {
		t.Errorf("extraKernelArgs is set while GRUB takes its cmdline from the installer's UKI\n" +
			"  reason: the two are mutually exclusive — the console arg is ignored, and the node rejects the config outright")
	}
}

// NewInput takes clusterName and endpoint adjacently and both are strings, so
// a swap compiles and produces a self-consistent — and useless — cluster.
func TestGenerateConfigNamesTheClusterAndTheEndpointTheRightWayRound(t *testing.T) {
	doc := v1alpha1Doc(t, mustGenerateDefault(t).ControlPlane)

	// A worker config generates, encodes, validates and carries every install
	// option asserted elsewhere. It just never becomes a cluster: nothing runs
	// etcd, and `talosctl bootstrap` has nothing to bootstrap.
	if !strings.Contains(doc, "type: controlplane") {
		t.Errorf("machine type is not controlplane\n"+
			"  reason: a single-node cluster has exactly one node; if it is a worker there is no control plane at all\n%s", doc)
	}

	if !strings.Contains(doc, "clusterName: probe") {
		t.Errorf("cluster is not named probe\n"+
			"  reason: cluster name and endpoint are adjacent string arguments; swapped, everything still generates\n%s", doc)
	}

	if !strings.Contains(doc, "endpoint: https://127.0.0.1:6443") {
		t.Errorf("control plane endpoint is not the Kubernetes API URL\n"+
			"  reason: the node would join a cluster whose API address is its own name\n%s", doc)
	}

	// emptyIf() silently drops the kubelet image when the Kubernetes version is
	// empty, so a missing version reads as a config that merely omits a field.
	if !strings.Contains(doc, "image: ghcr.io/siderolabs/kubelet:v") {
		t.Errorf("kubelet image is not pinned\n"+
			"  reason: an empty Kubernetes version leaves the node without a kubelet to run\n%s", doc)
	}
}

func TestGenerateConfigSchedulesOnTheControlPlane(t *testing.T) {
	doc := v1alpha1Doc(t, mustGenerateDefault(t).ControlPlane)

	if !strings.Contains(doc, "allowSchedulingOnControlPlanes: true") {
		t.Errorf("control-plane taint is left in place\n"+
			"  reason: a single-node cluster schedules NOTHING while the taint stands; this is a topology correction\n%s", doc)
	}
}

func TestGenerateConfigAddsLoopbackToMachineCertSANs(t *testing.T) {
	cfg, err := configloader.NewFromBytes(mustGenerateDefault(t).ControlPlane)
	if err != nil {
		t.Fatalf("generated config does not parse: %v", err)
	}

	// Asserted through the typed API on purpose: `- 127.0.0.1` also appears
	// under apiServer.certSANs, where the endpoint puts it for free, so a
	// substring match would pass with the machine SAN missing entirely.
	if sans := cfg.Machine().Security().CertSANs(); !slices.Contains(sans, "127.0.0.1") {
		t.Errorf("machine certSANs = %v, want 127.0.0.1\n"+
			"  reason: the Talos API is reached over a host port forward, so apid's cert must name the loopback or TLS fails", sans)
	}
}

func TestGenerateConfigCreatesUserVolumeOnTheDataDisk(t *testing.T) {
	cp := mustGenerateDefault(t).ControlPlane

	vol, ok := docOfKind(t, cp, "UserVolumeConfig")
	if !ok {
		t.Fatalf("no UserVolumeConfig document\n"+
			"  reason: PVCs would land on EPHEMERAL beside etcd, where a runaway PVC wedges the only control-plane node\n%s", cp)
	}

	if !strings.Contains(vol, "name: local-path-provisioner") {
		t.Errorf("user volume is not named local-path-provisioner\n"+
			"  reason: the name fixes the mount path /var/mnt/<name>, which the provisioner manifest hardcodes\n%s", vol)
	}

	if !strings.Contains(vol, `disk.serial == "talos-data"`) {
		t.Errorf("user volume does not select the data disk by serial\n"+
			"  reason: any other matcher can pick the system disk or the boot ISO, both of which are also virtio-blk\n%s", vol)
	}

	if !strings.Contains(vol, "volumeType: partition") {
		t.Errorf("user volume has no explicit volumeType\n"+
			"  reason: the shape of the volume is then a runtime default this config does not state\n%s", vol)
	}

	if !strings.Contains(vol, "grow: true") {
		t.Errorf("user volume does not grow to the disk\n"+
			"  reason: it would sit at its minimum size and a 40Gi data disk would silently provide 1Gi of PVCs\n%s", vol)
	}

	if !strings.Contains(vol, "type: xfs") {
		t.Errorf("user volume has no filesystem\n"+
			"  reason: an unformatted volume validates fine and then holds nothing\n%s", vol)
	}

	if strings.Contains(vol, "talos-system") {
		t.Errorf("user volume mentions the system disk serial\n"+
			"  reason: provisioning a user volume on the install target destroys it\n%s", vol)
	}
}

func TestGenerateConfigOmitsUserVolumeWithoutDataDisk(t *testing.T) {
	in := testInput()
	in.DataDiskSerial = ""

	cp := mustGenerate(t, in).ControlPlane

	if _, ok := docOfKind(t, cp, "UserVolumeConfig"); ok {
		t.Errorf("user volume emitted with no data disk\n"+
			"  reason: the volume would wait forever for a disk that was never attached, and the node never reaches ready\n%s", cp)
	}

	if strings.Contains(string(cp), "talos-data") {
		t.Error("no data disk means no reference to one; the two halves of storage must not disagree")
	}
}

func TestGenerateConfigRefusesAnImageNewerThanTheGenerator(t *testing.T) {
	in := testInput()
	in.TalosVersion = "v1.99.0"

	if _, err := GenerateConfig(in); err == nil {
		t.Fatal("generated a config for an image newer than the generator\n" +
			"  reason: exceeding the contract does not error, it silently emits a config for a Talos that does not exist")
	}
}

// An unknown image version disables the version GUARD by design (Task 2) — the
// guard only refuses images it can prove are too new. Generation is stricter,
// and has to be: the installer tag is written to disk, and defaulting it to
// ours is the cross-version install the pin above exists to prevent, rebuilt by
// hand for the one image we could not identify.
func TestGenerateConfigRefusesAnUnknownImageVersion(t *testing.T) {
	in := testInput()
	in.TalosVersion = ""

	// The config is deliberately NOT dumped on failure the way the passing-path
	// assertions dump theirs: this branch holds a fully generated config, which
	// is five CA private keys and the machine token, in the test log.
	_, err := GenerateConfig(in)
	if err == nil {
		t.Fatal("generated a config for an image of unknown version\n" +
			"  reason: the installer tag can only be this binary's version, which either the maintenance system " +
			"rejects or installs into a node that hangs at /sbin/init")
	}

	// A refusal with no way out is a dead end. The stock ISO's volume id is
	// where a usable version comes from, so the message has to name it.
	if !strings.Contains(err.Error(), "TALOS_V") {
		t.Errorf("refusal does not say how to obtain an identifiable image: %v", err)
	}
}

func TestGenerateConfigTargetsTheRequestedContract(t *testing.T) {
	oldIn, newIn := testInput(), testInput()
	oldIn.TalosVersion, newIn.TalosVersion = "v1.5.0", "v1.13.7"

	old := code(string(mustGenerate(t, oldIn).ControlPlane))
	current := code(string(mustGenerate(t, newIn).ControlPlane))

	// v1.5 predates KubePrism; v1.13 has it. Identical output for both means
	// the contract is not reaching the generator at all.
	if strings.Contains(old, "kubePrism") {
		t.Error("a v1.5 contract emitted kubePrism\n" +
			"  reason: the node rejects fields its version does not know")
	}

	if !strings.Contains(current, "kubePrism") {
		t.Error("a v1.13 contract did not emit kubePrism\n" +
			"  reason: the contract is not being threaded through; every version-gated default is then wrong")
	}

	// The install-time override below is version-gated too: grubUseUKICmdline
	// did not exist before 1.12, and Talos rejects fields it does not know.
	if strings.Contains(old, "grubUseUKICmdline") {
		t.Error("a v1.5 contract emitted grubUseUKICmdline\n" +
			"  reason: an override must not reintroduce a field the contract deliberately withheld")
	}
}

func TestGenerateConfigProducesAConfigMachineryAcceptsBack(t *testing.T) {
	cfg, err := configloader.NewFromBytes(mustGenerateDefault(t).ControlPlane)
	if err != nil {
		t.Fatalf("generated config does not parse: %v\n"+
			"  reason: an unparseable config is discovered by the NODE, minutes into a boot", err)
	}

	warnings, err := cfg.Validate(metalMode{})
	if err != nil {
		t.Fatalf("generated config does not validate: %v", err)
	}

	t.Logf("validation warnings: %v", warnings)
}

func TestGenerateConfigProducesATalosconfigPointingAtTheHostForward(t *testing.T) {
	g := mustGenerateDefault(t)

	c, err := clientconfig.FromBytes(g.Talosconfig)
	if err != nil {
		t.Fatalf("talosconfig does not parse: %v", err)
	}

	ctx, ok := c.Contexts[c.Context]
	if !ok {
		t.Fatalf("talosconfig has no context %q", c.Context)
	}

	if c.Context != "probe" {
		t.Errorf("talosconfig context = %q, want the cluster name\n"+
			"  reason: the context is named after the cluster; anything else means the name and the endpoint were swapped", c.Context)
	}

	if !slices.Contains(ctx.Endpoints, "127.0.0.1") {
		t.Errorf("talosconfig endpoints = %v, want 127.0.0.1\n"+
			"  reason: an endpointless talosconfig makes every talosctl call require -e, including the bootstrap", ctx.Endpoints)
	}
}

// secrets.yaml exists so the cluster can be regenerated with the same identity
// (`talosctl gen config --with-secrets`). That is worth nothing if machinery
// cannot read back what we wrote.
func TestGenerateConfigProducesReloadableSecrets(t *testing.T) {
	g := mustGenerateDefault(t)

	path := filepath.Join(t.TempDir(), "secrets.yaml")
	if err := os.WriteFile(path, g.Secrets, 0o600); err != nil {
		t.Fatal(err)
	}

	bundle, err := secrets.LoadBundle(path)
	if err != nil {
		t.Fatalf("machinery cannot load the secrets we wrote: %v", err)
	}

	if err := bundle.Validate(); err != nil {
		t.Errorf("reloaded secrets bundle is incomplete: %v\n"+
			"  reason: a bundle that loads but is missing a CA regenerates a cluster the old certs cannot talk to", err)
	}
}

// metalMode is the validation mode of a machine that installs to a disk, which
// is the only mode tinq ever produces configs for.
type metalMode struct{}

func (metalMode) String() string        { return "metal" }
func (metalMode) RequiresInstall() bool { return true }
func (metalMode) InContainer() bool     { return false }
