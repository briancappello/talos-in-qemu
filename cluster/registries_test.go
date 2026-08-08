package cluster

import (
	"strings"
	"testing"

	v1alpha1 "github.com/siderolabs/talos/pkg/machinery/config/types/v1alpha1"
)

func TestRegistriesConfigEmpty(t *testing.T) {
	got := registriesConfig(nil)

	if got.RegistryMirrors != nil || got.RegistryConfig != nil {
		t.Fatalf("no mirrors must yield no maps, got %+v", got)
	}
}

func TestRegistriesConfigPlainHTTP(t *testing.T) {
	got := registriesConfig([]RegistryMirror{{
		Host:     "10.0.2.2:5000",
		Endpoint: "http://10.0.2.2:5000",
	}})

	mirror := got.RegistryMirrors["10.0.2.2:5000"]
	if mirror == nil {
		t.Fatalf("mirror not keyed by host:port, got %+v", got.RegistryMirrors)
	}

	if len(mirror.MirrorEndpoints) != 1 || mirror.MirrorEndpoints[0] != "http://10.0.2.2:5000" {
		t.Fatalf("endpoint not carried through verbatim: %+v", mirror.MirrorEndpoints)
	}

	// The whole point of the http:// scheme: a cleartext mirror needs no TLS
	// stanza, and emitting one would claim a certificate that does not exist.
	if got.RegistryConfig != nil {
		t.Fatalf("plain HTTP mirror must emit no TLS config, got %+v", got.RegistryConfig)
	}
}

func TestRegistriesConfigInsecureTLS(t *testing.T) {
	got := registriesConfig([]RegistryMirror{{
		Host:               "registry.lan:5000",
		Endpoint:           "https://registry.lan:5000",
		InsecureSkipVerify: true,
	}})

	cfg := got.RegistryConfig["registry.lan:5000"]
	if cfg == nil || cfg.RegistryTLS == nil || cfg.RegistryTLS.TLSInsecureSkipVerify == nil {
		t.Fatalf("insecureSkipVerify did not reach the TLS config: %+v", got.RegistryConfig)
	}

	if !*cfg.RegistryTLS.TLSInsecureSkipVerify {
		t.Fatal("insecureSkipVerify set to false")
	}
}

func TestRegistriesConfigCA(t *testing.T) {
	pem := "-----BEGIN CERTIFICATE-----\nMIIseed\n-----END CERTIFICATE-----\n"
	got := registriesConfig([]RegistryMirror{{
		Host: "registry.lab", Endpoint: "https://registry.lab", CA: pem,
	}})

	cfg := got.RegistryConfig["registry.lab"]
	if cfg == nil || cfg.RegistryTLS == nil || cfg.RegistryTLS.TLSCA == nil {
		t.Fatalf("CA did not reach the TLS config: %+v", got.RegistryConfig)
	}
	if string(cfg.RegistryTLS.TLSCA) != pem {
		t.Fatal("CA bytes not carried verbatim")
	}
	// A CA to TRUST is not a request to SKIP verification.
	if cfg.RegistryTLS.TLSInsecureSkipVerify != nil {
		t.Fatal("CA must not imply insecureSkipVerify")
	}
}

func TestRegistriesConfigWildcardHasNoTLSConfig(t *testing.T) {
	// Talos rejects "*" as a TLS config key. Emitting one fails validation at
	// apply time, i.e. after a VM has already booted.
	got := registriesConfig([]RegistryMirror{{
		Host:               "*",
		Endpoint:           "https://registry.lan:5000",
		InsecureSkipVerify: true,
	}})

	if got.RegistryConfig != nil {
		t.Fatalf("wildcard host must not produce a TLS config entry: %+v", got.RegistryConfig)
	}

	if got.RegistryMirrors["*"] == nil {
		t.Fatal("wildcard mirror itself must still be emitted")
	}
}

func TestRegistriesConfigTwoEndpointsOneHost(t *testing.T) {
	got := registriesConfig([]RegistryMirror{
		{Host: "docker.io", Endpoint: "http://10.0.2.2:5000"},
		{Host: "docker.io", Endpoint: "https://mirror.example.com"},
	})

	if n := len(got.RegistryMirrors["docker.io"].MirrorEndpoints); n != 2 {
		t.Fatalf("list->map fold lost an endpoint: want 2, got %d", n)
	}
}

func TestRegistriesConfigOverridePath(t *testing.T) {
	// A stock registry serves the API at /v2/. overridePath suppresses that
	// suffix, so setting it where it is not wanted turns every pull into a 404
	// that reads like a missing image — which is why it must be carried
	// EXACTLY as asked and never defaulted on.
	got := registriesConfig([]RegistryMirror{
		{Host: "a:5000", Endpoint: "http://a:5000/mirror/a", OverridePath: true},
		{Host: "b:5000", Endpoint: "http://b:5000"},
	})

	on := got.RegistryMirrors["a:5000"].MirrorOverridePath
	if on == nil || !*on {
		t.Fatalf("overridePath did not reach the mirror config: %+v", on)
	}

	if off := got.RegistryMirrors["b:5000"].MirrorOverridePath; off != nil {
		t.Fatalf("overridePath emitted for a mirror that did not ask: %+v", *off)
	}
}

// machineRegistries is the rendered stanza these two tests look for.
//
// A BARE "registries:" WOULD MATCH THE WRONG ONE. A generated config carries a
// SECOND registries key — cluster.discovery.registries, which lists Kubernetes
// and the discovery service and is emitted for every machine ever generated.
// It sits at eight spaces, machine.registries at four, so the indent is what
// tells them apart; the `mirrors:` line under it is what makes the match
// unambiguous. The first draft of this test asserted on the bare word and
// passed against the discovery block with the patch not wired at all.
const machineRegistries = "\n    registries:\n        mirrors:\n"

// The mapper being right proves nothing on its own: it is reached through a
// PatchV1Alpha1 callback, and a patch that is never wired up leaves every test
// above green while no node ever sees a mirror. This is the only check that
// fails if that wiring is dropped.
func TestGeneratedConfigCarriesTheMirror(t *testing.T) {
	in := testInput()
	in.Registries = []RegistryMirror{{
		Host:     "10.0.2.2:5000",
		Endpoint: "http://10.0.2.2:5000",
	}}

	// v1alpha1 is the FIRST document; the user volume is another one. A mirror
	// that landed anywhere else is a mirror the node ignores.
	v1alpha1Doc := splitDocs(mustGenerate(t, in).ControlPlane)[0]

	if !strings.Contains(v1alpha1Doc, machineRegistries) {
		t.Fatalf("no machine.registries.mirrors stanza — the registries patch never reached "+
			"the config:\n%s", v1alpha1Doc)
	}

	for _, want := range []string{"10.0.2.2:5000:", "- http://10.0.2.2:5000"} {
		if !strings.Contains(v1alpha1Doc, want) {
			t.Errorf("the generated v1alpha1 document does not contain %q", want)
		}
	}
}

// An empty list must leave the section OUT, not emit `registries: {}`. The
// distinction is not cosmetic here: every generated config in this repo is
// diffed by eye during a bring-up, and a stanza that appears on every machine
// whether or not it configures anything is noise that hides the one that does.
func TestGeneratedConfigOmitsRegistriesWhenNoneAreAsked(t *testing.T) {
	if doc := splitDocs(mustGenerateDefault(t).ControlPlane)[0]; strings.Contains(doc, machineRegistries) {
		t.Error("a machine with no mirrors emitted a machine.registries section")
	}
}

var _ = v1alpha1.RegistriesConfig{}
