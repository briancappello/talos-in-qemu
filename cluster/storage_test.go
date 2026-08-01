package cluster

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/siderolabs/talos/pkg/machinery/config/configloader"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block"
	"github.com/siderolabs/talos/pkg/machinery/constants"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// The manifest carries no secrets, so nothing in this file needs redact().
// InstallStorage's INPUT does — a kubeconfig is a CA and a client key — and
// TestInstallStorageWithholdsTheKubeconfigFromItsError holds that line.

func manifestObjectsOrFail(t *testing.T) []*unstructured.Unstructured {
	t.Helper()

	objs, err := manifestObjects()
	if err != nil {
		t.Fatalf("manifestObjects: %v", err)
	}

	return objs
}

// object finds the one object of this kind and name. It FAILS on a second
// match rather than taking the first: a duplicated document is how a
// hand-edited manifest silently ships two StorageClasses, and an assertion
// that stops at the first would pass while the cluster got the other one.
func object(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()

	var found *unstructured.Unstructured

	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			if found != nil {
				t.Fatalf("two %s objects named %q in the manifest", kind, name)
			}

			found = o
		}
	}

	if found == nil {
		t.Fatalf("no %s named %q in the manifest (have %s)", kind, name, inventory(objs))
	}

	return found
}

func inventory(objs []*unstructured.Unstructured) string {
	var b strings.Builder

	for i, o := range objs {
		if i > 0 {
			b.WriteString(", ")
		}

		b.WriteString(o.GetKind() + "/" + o.GetName())
	}

	return b.String()
}

// PATCH 1. Talos's root filesystem is READ-ONLY: upstream's
// /opt/local-path-provisioner is not writable, and the provisioner's helper pod
// fails to create the directory with an error that surfaces only as a PVC stuck
// Pending. /var is the writable EPHEMERAL partition.
//
// The assertion parses config.json rather than grepping the manifest, because a
// substring check is satisfied by the path appearing in a comment.
func TestManifestProvisionsUnderTheTalosMountPath(t *testing.T) {
	cm := object(t, manifestObjectsOrFail(t), "ConfigMap", "local-path-config")

	raw, ok, err := unstructured.NestedString(cm.Object, "data", "config.json")
	if err != nil || !ok {
		t.Fatalf("ConfigMap has no data.config.json (ok=%v): %v", ok, err)
	}

	var cfg struct {
		NodePathMap []struct {
			Node  string   `json:"node"`
			Paths []string `json:"paths"`
		} `json:"nodePathMap"`
	}

	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("config.json does not parse as JSON: %v\n%s", err, raw)
	}

	if len(cfg.NodePathMap) != 1 {
		t.Fatalf("config.json has %d nodePathMap entries, want 1\n%s", len(cfg.NodePathMap), raw)
	}

	if got := cfg.NodePathMap[0].Paths; len(got) != 1 || got[0] != mountPath {
		t.Errorf("provisioner root path = %v, want [%s]\n"+
			"  reason: Talos mounts / read-only; a PVC under a non-writable root stays Pending with nothing naming the cause",
			got, mountPath)
	}
}

// The other half of patch 1, and the half a positive assertion cannot make:
// upstream's path must be gone from EVERY object, not merely absent from the
// one field checked above. Serialising the parsed objects rather than reading
// the file means a comment cannot satisfy this either.
func TestManifestMentionsUpstreamsRootPathNowhere(t *testing.T) {
	for _, o := range manifestObjectsOrFail(t) {
		b, err := json.Marshal(o.Object)
		if err != nil {
			t.Fatalf("marshalling %s/%s: %v", o.GetKind(), o.GetName(), err)
		}

		if strings.Contains(string(b), upstreamRootPath) {
			t.Errorf("%s/%s still names %s\n"+
				"  reason: that path is on Talos's read-only root and nothing can be written there",
				o.GetKind(), o.GetName(), upstreamRootPath)
		}
	}
}

// The agreement that patch 1 depends on, checked against the OTHER file rather
// than against a literal repeated here: mountPath must be where Talos actually
// mounts the user volume cluster/config.go asks for. Whichever side is renamed,
// this fails.
func TestMountPathIsWhereTalosMountsTheGeneratedUserVolume(t *testing.T) {
	cfg, err := configloader.NewFromBytes(mustGenerateDefault(t).ControlPlane)
	if err != nil {
		t.Fatalf("parsing the generated config: %s", redactErr(err))
	}

	var volumes []string

	for _, doc := range cfg.Documents() {
		if v, ok := doc.(*block.UserVolumeConfigV1Alpha1); ok {
			volumes = append(volumes, v.Name())
		}
	}

	if len(volumes) != 1 {
		t.Fatalf("the generated config has %d user volumes %v, want 1", len(volumes), volumes)
	}

	if want := constants.UserVolumeMountPoint + "/" + volumes[0]; mountPath != want {
		t.Errorf("mountPath = %q, but Talos mounts the generated user volume at %q\n"+
			"  reason: the provisioner would write to a directory on EPHEMERAL, not on the data disk, "+
			"and the disk would sit empty while PVCs filled the partition holding etcd",
			mountPath, want)
	}
}

// PATCH 2. Talos ships no default StorageClass (kind bundles one; Talos does
// not). Without this a PVC that names no storageClassName stays Pending
// forever, and nothing in the cluster says why.
func TestStorageClassIsTheClusterDefault(t *testing.T) {
	sc := object(t, manifestObjectsOrFail(t), "StorageClass", "local-path")

	got, ok, err := unstructured.NestedString(sc.Object,
		"metadata", "annotations", "storageclass.kubernetes.io/is-default-class")
	if err != nil {
		t.Fatalf("reading the default-class annotation: %v", err)
	}

	if !ok || got != "true" {
		t.Errorf("is-default-class = %q (present=%v), want \"true\"\n"+
			"  reason: a PVC with no storageClassName binds to nothing and stays Pending with no event naming the cause",
			got, ok)
	}
}

// PATCH 3. Talos enforces PodSecurity at `baseline` cluster-wide, and the
// provisioner's helper pod mounts a hostPath — rejected at admission. Exactly
// one namespace is exempted, and it is the provisioner's own.
func TestProvisionerNamespaceIsExemptFromPodSecurity(t *testing.T) {
	objs := manifestObjectsOrFail(t)
	ns := object(t, objs, "Namespace", "local-path-storage")

	got, ok, err := unstructured.NestedString(ns.Object,
		"metadata", "labels", "pod-security.kubernetes.io/enforce")
	if err != nil {
		t.Fatalf("reading the pod-security label: %v", err)
	}

	if !ok || got != "privileged" {
		t.Errorf("pod-security.kubernetes.io/enforce = %q (present=%v), want \"privileged\"\n"+
			"  reason: Talos enforces `baseline`, and the helper pod's hostPath is refused at admission",
			got, ok)
	}

	// The exemption is only worth anything if the workload lands in the
	// namespace carrying it, and the helper pod is created in the
	// provisioner's own namespace (POD_NAMESPACE).
	deploy := object(t, objs, "Deployment", "local-path-provisioner")
	if deploy.GetNamespace() != ns.GetName() {
		t.Errorf("the provisioner runs in namespace %q but %q carries the exemption\n"+
			"  reason: the helper pod inherits the provisioner's namespace, so the label must be on that one",
			deploy.GetNamespace(), ns.GetName())
	}

	// Only that one namespace. A blanket exemption would let every user
	// workload run privileged.
	for _, o := range objs {
		if o.GetKind() != "Namespace" || o.GetName() == ns.GetName() {
			continue
		}

		if v, _, _ := unstructured.NestedString(o.Object,
			"metadata", "labels", "pod-security.kubernetes.io/enforce"); v == "privileged" {
			t.Errorf("namespace %q is also exempt from PodSecurity\n"+
				"  reason: only the provisioner's infra namespace may be; user workloads stay at baseline", o.GetName())
		}
	}
}

// LocalPathVersion is what the report and the upgrade path are written against.
// If it and the vendored manifest disagree, the constant is a lie: the cluster
// runs whatever the manifest says.
func TestManifestRunsTheVersionTheConstantNames(t *testing.T) {
	deploy := object(t, manifestObjectsOrFail(t), "Deployment", "local-path-provisioner")

	containers, ok, err := unstructured.NestedSlice(deploy.Object, "spec", "template", "spec", "containers")
	if err != nil || !ok || len(containers) != 1 {
		t.Fatalf("Deployment has %d containers (ok=%v): %v", len(containers), ok, err)
	}

	image, _, _ := unstructured.NestedString(containers[0].(map[string]interface{}), "image")
	if want := "rancher/local-path-provisioner:" + LocalPathVersion; image != want {
		t.Errorf("provisioner image = %q, want %q\n"+
			"  reason: LocalPathVersion is what provenance and upgrades are tracked by; the manifest is what runs",
			image, want)
	}
}

// imageLine matches every `image:` value in the MANIFEST TEXT, which is what
// makes the helper pod visible: it lives as a YAML string inside a ConfigMap,
// so walking the decoded object graph never reaches it as a container spec.
var imageLine = regexp.MustCompile(`(?m)^\s*image:\s*(\S+)\s*$`)

// No image may run whatever `latest` resolves to on the day the node pulls: a
// floating tag makes the version constant unfalsifiable.
//
// The absence of a tag is the SAME failure, and it is the one that hid here.
// Kubernetes resolves a bare `busybox` to `busybox:latest`, so a test that
// greps for the literal ":latest" reports clean on precisely the image nobody
// pinned — and that image is the helper pod, which `mkdir`s and `rm -rf`s
// every PVC directory. So the tag is PARSED and required to exist, not
// searched for.
func TestManifestPinsEveryImageByTag(t *testing.T) {
	rendered, err := render(localPathManifest)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	found := imageLine.FindAllStringSubmatch(string(rendered), -1)

	// The provisioner Deployment and the helper pod. Without this the whole
	// test passes by matching nothing the day the pattern or the manifest
	// shape changes.
	if len(found) < 2 {
		t.Fatalf("matched %d `image:` lines, want at least 2 (the provisioner and its helper pod)\n"+
			"  reason: an assertion that matches nothing passes forever", len(found))
	}

	for _, m := range found {
		image := m[1]

		// The last colon is only a tag separator if it comes AFTER the last
		// slash: a registry host may carry a port (registry:5000/busybox),
		// and reading that as a tag would call an untagged image pinned.
		tag := ""
		if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
			tag = image[i+1:]
		}

		switch tag {
		case "":
			t.Errorf("%q carries no tag\n"+
				"  reason: Kubernetes resolves an untagged image to :latest, so this floats — and it "+
				"floats invisibly, because the string \":latest\" never appears", image)
		case "latest":
			t.Errorf("%q uses a floating :latest tag", image)
		}
	}
}

func TestRenderRefusesAManifestThatDoesNotCarryUpstreamsPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"no occurrence", "kind: ConfigMap\n"},
		{"two occurrences", "a: " + upstreamRootPath + "\nb: " + upstreamRootPath + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := render([]byte(tc.in)); err == nil {
				t.Errorf("render accepted a manifest with the wrong number of occurrences\n" +
					"  reason: the patch is a text substitution; if it silently no-ops the cluster gets upstream's read-only path")
			}
		})
	}
}

func TestRenderRewritesUpstreamsPath(t *testing.T) {
	out, err := render([]byte("paths: [\"" + upstreamRootPath + "\"]\n"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if got, want := string(out), "paths: [\""+mountPath+"\"]\n"; got != want {
		t.Errorf("render = %q, want %q", got, want)
	}
}

func TestDecodeSkipsEmptyDocuments(t *testing.T) {
	// A leading separator, a comment-only document and a trailing separator —
	// all three are legal YAML and all three decode to nothing. Passed to the
	// API server they are an object with no kind, and the apply fails on a
	// manifest that is perfectly valid.
	objs, err := decode([]byte("---\n# just a comment\n---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: keeper\n---\n"))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(objs) != 1 || objs[0].GetName() != "keeper" {
		t.Errorf("decode returned %d objects %s, want 1 (Namespace/keeper)", len(objs), inventory(objs))
	}
}

func TestDecodeRejectsAnObjectWithNoKind(t *testing.T) {
	if _, err := decode([]byte("apiVersion: v1\nmetadata:\n  name: keeper\n")); err == nil {
		t.Error("decode accepted a document with no kind\n" +
			"  reason: it cannot be mapped to a resource, and the failure would surface at apply time as a bare 404")
	}
}

func TestDecodeRejectsMalformedYAML(t *testing.T) {
	if _, err := decode([]byte("kind: [unterminated\n")); err == nil {
		t.Error("decode accepted malformed YAML")
	}
}

// A kubeconfig is a CA and a client key. errSecretParse exists so a parser's
// message — which quotes the scalar it choked on — never reaches a terminal.
//
// The input, the seven-character marker and the fingerprint assertion are
// client_test.go's, deliberately: the first draft of this test used a long
// obvious secret and a marker-only check, and the mutant that wraps the
// parser's error with %w SURVIVED both, because clientcmd truncates what it
// quotes and the whole marker never appeared. assertNoSecretParserOutput also
// pins the decoder's own fingerprint, which is what makes the verdict
// deterministic rather than a bet on how much a library chooses to print.
func TestInstallStorageWithholdsTheKubeconfigFromItsError(t *testing.T) {
	broken := []byte("apiVersion: v1\nkind: Config\nclusters: " + marker + strings.Repeat("A", 200) + "\n")

	assertNoSecretParserOutput(t, "kubeconfig", InstallStorage(t.Context(), broken))
}

func TestInstallStorageRejectsAnEmptyKubeconfig(t *testing.T) {
	if err := InstallStorage(t.Context(), nil); err == nil {
		t.Error("InstallStorage accepted an empty kubeconfig")
	}
}

// The manifest is embedded, so it cannot be corrupted at runtime — but it CAN
// be broken by a bad re-vendor, and then InstallStorage must say so rather than
// report a storage install that put nothing in the cluster. The kubeconfig here
// is valid and points at a live server, so the only thing that can fail is the
// manifest, and the assertion that no request was made proves the check happens
// before anything is sent.
//
// Swapping the package-level manifest is the only way to reach that branch, and
// it is safe here because this test is not parallel and no other test in the
// package reads the variable. `go test -race` covers the claim.
func TestInstallStorageRefusesAManifestItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
	}{
		{"unpatchable", "kind: ConfigMap\napiVersion: v1\nmetadata:\n  name: cm\n"},
		{"no objects", "# " + upstreamRootPath + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &recordingAPI{}
			api.client(t) // only for the server; InstallStorage builds its own

			original := localPathManifest
			t.Cleanup(func() { localPathManifest = original })

			localPathManifest = []byte(tc.manifest)

			if err := InstallStorage(t.Context(), api.kubeconfig); err == nil {
				t.Error("InstallStorage reported success on a manifest it could not use\n" +
					"  reason: it would have installed nothing and said so nowhere")
			}

			if len(api.requests) != 0 {
				t.Errorf("InstallStorage sent %d requests before checking the manifest, want 0", len(api.requests))
			}
		})
	}
}

// Every way a manifest can be unusable ends in an error — that much is cheap.
// What is asserted here is WHICH error, because all three failures cascade into
// "no objects" if the one before is swallowed, and "the manifest contains no
// objects" tells whoever re-vendored it nothing about what they broke.
func TestObjectsInReportsWhyAManifestIsUnusable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
		want     string
	}{
		{
			// render fails: nothing to substitute.
			"unpatchable", "kind: ConfigMap\napiVersion: v1\nmetadata:\n  name: cm\n",
			upstreamRootPath,
		},
		{
			// decode fails: the second document is not YAML.
			"malformed", "# " + upstreamRootPath + "\n---\nb: [unterminated\n",
			"document 1",
		},
		{
			// both succeed and produce nothing at all.
			"no objects", "# " + upstreamRootPath + "\n",
			"no objects",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := objectsIn([]byte(tc.manifest))
			if err == nil {
				t.Fatal("objectsIn accepted an unusable manifest\n" +
					"  reason: applying an empty slice makes no request and returns success")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not mention %q: %v\n"+
					"  reason: swallowed, this failure resurfaces as the generic \"no objects\", "+
					"which names neither the cause nor the fix", tc.want, err)
			}
		})
	}
}

// --- apply -----------------------------------------------------------------

// recordingAPI is a stand-in API server that records what apply sent it. It
// answers every request with the object it was given, which is what a real
// server does on a successful apply.
type recordingAPI struct {
	requests []*http.Request
	bodies   []string
	status   int
	// kubeconfig reaches this same server. Plain HTTP, so it holds no CA and
	// no client key and is not secret material, unlike the real one.
	kubeconfig []byte
}

func (r *recordingAPI) client(t *testing.T) dynamic.Interface {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)

		r.requests = append(r.requests, req)
		r.bodies = append(r.bodies, string(body))

		if r.status != 0 {
			w.WriteHeader(r.status)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"nope"}`))

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	r.kubeconfig = fmt.Appendf(nil, `apiVersion: v1
kind: Config
clusters:
- name: probe
  cluster:
    server: %s
contexts:
- name: probe
  context:
    cluster: probe
current-context: probe
`, srv.URL)

	dyn, err := dynamic.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("dynamic client: %v", err)
	}

	return dyn
}

// testMapper maps only what the tests below use. A DefaultRESTMapper is enough:
// the production mapper is discovery-backed, but what apply does with a mapping
// is the same either way.
func testMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, meta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, meta.RESTScopeNamespace)

	return m
}

func objectFromYAML(t *testing.T, y string) *unstructured.Unstructured {
	t.Helper()

	objs, err := decode([]byte(y))
	if err != nil || len(objs) != 1 {
		t.Fatalf("decode(%q) = %d objects, %v", y, len(objs), err)
	}

	return objs[0]
}

const (
	namespacedYAML = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: ns\n"
	// The stray `namespace:` is deliberate. A Namespace is cluster-scoped and
	// the field means nothing on it, but it is exactly what a copy-pasted
	// document carries — and apply must route by the SCOPE the cluster reports,
	// not by whether the object happens to have the field set. Without it here,
	// "always call .Namespace()" is indistinguishable from correct code,
	// because .Namespace("") builds the cluster-scoped URL anyway.
	clusterYAML = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns\n  namespace: stray\n"
)

// A namespaced object sent to the cluster-scoped URL is created in whatever
// namespace the server defaults to — `default` — while the manifest says
// otherwise, and nothing errors.
func TestApplySendsNamespacedObjectsToTheirOwnNamespace(t *testing.T) {
	api := &recordingAPI{}

	if err := apply(t.Context(), api.client(t), testMapper(),
		[]*unstructured.Unstructured{objectFromYAML(t, namespacedYAML)}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if len(api.requests) != 1 {
		t.Fatalf("apply made %d requests, want 1", len(api.requests))
	}

	if got, want := api.requests[0].URL.Path, "/api/v1/namespaces/ns/configmaps/cm"; got != want {
		t.Errorf("apply requested %q, want %q\n"+
			"  reason: dropped, the object lands in the server's default namespace and the manifest silently means something else",
			got, want)
	}
}

// The mirror image: a cluster-scoped object addressed through a namespace is a
// 404 the manifest cannot explain.
func TestApplySendsClusterScopedObjectsToNoNamespace(t *testing.T) {
	api := &recordingAPI{}

	if err := apply(t.Context(), api.client(t), testMapper(),
		[]*unstructured.Unstructured{objectFromYAML(t, clusterYAML)}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if got, want := api.requests[0].URL.Path, "/api/v1/namespaces/ns"; got != want {
		t.Errorf("apply requested %q, want %q\n"+
			"  reason: a Namespace addressed inside a namespace is a 404", got, want)
	}
}

// EVERY object, and in the manifest's order. A loop that stops early installs a
// Namespace and calls it storage; the order matters because the Namespace has
// to exist before the objects inside it.
func TestApplySendsEveryObjectInOrder(t *testing.T) {
	api := &recordingAPI{}

	objs := []*unstructured.Unstructured{objectFromYAML(t, clusterYAML), objectFromYAML(t, namespacedYAML)}
	if err := apply(t.Context(), api.client(t), testMapper(), objs); err != nil {
		t.Fatalf("apply: %v", err)
	}

	want := []string{"/api/v1/namespaces/ns", "/api/v1/namespaces/ns/configmaps/cm"}
	if len(api.requests) != len(want) {
		t.Fatalf("apply made %d requests for %d objects", len(api.requests), len(objs))
	}

	for i, w := range want {
		if got := api.requests[i].URL.Path; got != w {
			t.Errorf("request %d went to %q, want %q\n"+
				"  reason: out of order, the first run fails with \"namespace not found\"", i, got, w)
		}
	}
}

// Idempotency is not a retry loop, it is the VERB. Server-side apply is
// declarative: the same document sent twice converges rather than colliding,
// where a create would fail AlreadyExists the second time. Task 7 may retry.
func TestApplyUsesServerSideApply(t *testing.T) {
	api := &recordingAPI{}

	objs := []*unstructured.Unstructured{objectFromYAML(t, namespacedYAML)}
	for range 2 {
		if err := apply(t.Context(), api.client(t), testMapper(), objs); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}

	if len(api.requests) != 2 {
		t.Fatalf("two applies made %d requests, want 2", len(api.requests))
	}

	for i, req := range api.requests {
		if got, want := req.Method, http.MethodPatch; got != want {
			t.Errorf("request %d used %s, want %s\n"+
				"  reason: a create fails AlreadyExists on the second run, which Task 7's retry would hit", i, got, want)
		}

		if got, want := req.Header.Get("Content-Type"), "application/apply-patch+yaml"; got != want {
			t.Errorf("request %d content type = %q, want %q\n"+
				"  reason: any other patch type is not declarative and does not converge", i, got, want)
		}

		q := req.URL.Query()
		if q.Get("fieldManager") == "" {
			t.Errorf("request %d sends no fieldManager\n"+
				"  reason: server-side apply rejects the patch outright without one", i)
		}

		if q.Get("force") != "true" {
			t.Errorf("request %d does not force ownership (force=%q)\n"+
				"  reason: a second apply conflicts with the fields the first one claimed and errors", i, q.Get("force"))
		}
	}

	if api.bodies[0] != api.bodies[1] {
		t.Error("the two applies sent different bodies\n" +
			"  reason: the manifest is a pure artifact; a body that varies per call cannot converge")
	}
}

func TestApplyReportsTheObjectItFailedOn(t *testing.T) {
	api := &recordingAPI{status: http.StatusInternalServerError}

	err := apply(t.Context(), api.client(t), testMapper(),
		[]*unstructured.Unstructured{objectFromYAML(t, namespacedYAML)})
	if err == nil {
		t.Fatal("apply reported success on a server error")
	}

	if !strings.Contains(err.Error(), "cm") {
		t.Errorf("the error does not name the object: %v\n"+
			"  reason: eight objects go out; an unattributed failure says nothing about which", err)
	}
}

func TestApplyReportsAKindItCannotMap(t *testing.T) {
	api := &recordingAPI{}

	unmapped := objectFromYAML(t, "apiVersion: does.not/v1\nkind: Nope\nmetadata:\n  name: keeper\n")

	err := apply(t.Context(), api.client(t), testMapper(), []*unstructured.Unstructured{unmapped})
	if err == nil {
		t.Fatal("apply accepted a kind the cluster does not serve")
	}

	if !strings.Contains(err.Error(), "Nope") {
		t.Errorf("the error does not name the kind: %v", err)
	}

	if len(api.requests) != 0 {
		t.Errorf("apply made %d requests for an unmappable kind, want 0", len(api.requests))
	}
}

// Every object in the real manifest must map and be sent, in order. The
// Namespace has to precede the objects inside it: applied the other way round,
// the first run fails with "namespaces \"local-path-storage\" not found".
func TestApplySendsTheNamespaceFirst(t *testing.T) {
	objs := manifestObjectsOrFail(t)

	for i, o := range objs {
		if o.GetKind() != "Namespace" {
			continue
		}

		if i != 0 {
			t.Errorf("the Namespace is object %d of the manifest, want 0 (%s)\n"+
				"  reason: applied after the objects inside it, the first run fails with \"namespace not found\"",
				i, inventory(objs))
		}
	}
}

func TestDecodeRejectsAnObjectWithNoAPIVersion(t *testing.T) {
	if _, err := decode([]byte("kind: Namespace\nmetadata:\n  name: keeper\n")); err == nil {
		t.Error("decode accepted a document with no apiVersion\n" +
			"  reason: it maps to the empty group, and the apply goes to a URL no server serves")
	}
}
