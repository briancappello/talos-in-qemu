package cluster

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/siderolabs/talos/pkg/machinery/constants"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// Kubernetes has no in-tree storage for a bare-metal single node: with no
// StorageClass a PVC is never bound and nothing schedules behind it. This file
// installs rancher/local-path-provisioner, which is what kind and k3s use for
// the same reason.
//
// It is BOOTSTRAP ONLY, like the rest of this package: it puts the manifest
// into a cluster that has just been created. Nothing here reconciles it
// afterwards, and nothing here uninstalls it.

// LocalPathVersion is the rancher/local-path-provisioner release installed.
// The manifest beside this file is that release's, and
// TestManifestRunsTheVersionTheConstantNames holds the two together.
const LocalPathVersion = "v0.0.31"

// The manifest is EMBEDDED rather than fetched. A bring-up that reaches out to
// raw.githubusercontent.com at install time is a network dependency in the
// middle of a local VM workflow, an unpinned input, and a diff nobody reviews.
// Provenance is recorded at the top of the file.
//
//go:embed local-path-storage.yaml
var localPathManifest []byte

// upstreamRootPath is the directory upstream provisions PVCs under. On Talos it
// cannot work: the root filesystem is READ-ONLY, so /opt is not writable, and
// the helper pod's `mkdir -p` fails with the PVC left Pending and the reason
// buried in a pod that has already been garbage collected.
const upstreamRootPath = "/opt/local-path-provisioner"

// mountPath is where PVCs actually go, and it is DERIVED rather than written
// out: constants.UserVolumeMountPoint is where Talos mounts user volumes, and
// userVolumeName is the volume cluster/config.go asks Talos to create on the
// data disk. Spelling "/var/mnt/local-path-provisioner" here instead would
// compile, read correctly, and stop being true the moment either side is
// renamed — after which PVCs land on the EPHEMERAL partition beside etcd and
// the dedicated data disk sits empty, with nothing failing to say so.
// TestMountPathIsWhereTalosMountsTheGeneratedUserVolume checks the agreement
// against a generated config rather than against this comment.
const mountPath = constants.UserVolumeMountPoint + "/" + userVolumeName

// fieldManager identifies this tool's server-side apply. Any value works; a
// stable one means a second run updates the fields the first run owned instead
// of accumulating managers.
const fieldManager = "tinq"

// render applies the one patch that is not in the vendored file: upstream's
// root path becomes the Talos user volume's mount point.
//
// It is a text substitution, so it can silently do NOTHING — an upstream bump
// that moves or renames that path would leave the manifest unpatched, install
// cleanly, and hand every PVC a directory on a read-only filesystem. Hence the
// count, and hence exactly one: two occurrences means upstream grew a second
// site whose meaning nobody has looked at, and patching both by reflex is a
// guess.
func render(manifest []byte) ([]byte, error) {
	if n := bytes.Count(manifest, []byte(upstreamRootPath)); n != 1 {
		return nil, fmt.Errorf("the local-path-provisioner manifest names %s %d times, want exactly 1\n\n"+
			"That path is upstream's, and it is on Talos's READ-ONLY root filesystem. It is rewritten to %s,\n"+
			"the Talos user volume on the data disk, and a substitution that matches nothing leaves every PVC\n"+
			"provisioning into a directory that cannot be written.\n\n"+
			"  re-vendor cluster/local-path-storage.yaml from local-path-provisioner %s and check what moved",
			upstreamRootPath, n, mountPath, LocalPathVersion)
	}

	return bytes.ReplaceAll(manifest, []byte(upstreamRootPath), []byte(mountPath)), nil
}

// manifestObjects is the embedded manifest, patched and parsed. It is a pure
// artifact: no cluster, no network, no filesystem, which is why almost all of
// this file's behaviour is testable before a VM exists.
func manifestObjects() ([]*unstructured.Unstructured, error) {
	return objectsIn(localPathManifest)
}

// objectsIn is manifestObjects with the bytes supplied, which is what makes the
// failure paths reachable from a test: the embedded manifest is correct by
// construction, so nothing below can be exercised through manifestObjects.
func objectsIn(manifest []byte) ([]*unstructured.Unstructured, error) {
	rendered, err := render(manifest)
	if err != nil {
		return nil, err
	}

	objs, err := decode(rendered)
	if err != nil {
		return nil, err
	}

	// Nothing below this point distinguishes "installed everything" from
	// "installed nothing": apply over an empty slice makes no request and
	// returns success, so a truncated or comment-only manifest would report a
	// storage install that never happened, and the first sign of it would be a
	// PVC in Task 8 with no provisioner to serve it.
	if len(objs) == 0 {
		return nil, errors.New("the local-path-provisioner manifest contains no objects")
	}

	return objs, nil
}

// decode splits a multi-document manifest into typeless objects.
//
// It is typeless deliberately: a scheme-based decoder needs every kind
// registered, which couples this file to whichever API groups client-go's
// scheme happens to carry, for no gain — the objects are forwarded verbatim and
// never inspected.
func decode(manifest []byte) ([]*unstructured.Unstructured, error) {
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(manifest)))

	var objs []*unstructured.Unstructured

	for i := 0; ; i++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return objs, nil
		}

		if err != nil {
			return nil, fmt.Errorf("reading manifest document %d: %w", i, err)
		}

		var content map[string]interface{}

		if err := yaml.Unmarshal(doc, &content); err != nil {
			return nil, fmt.Errorf("parsing manifest document %d: %w", i, err)
		}

		if len(content) == 0 {
			continue
		}

		obj := &unstructured.Unstructured{Object: content}

		if obj.GetKind() == "" || obj.GetAPIVersion() == "" {
			return nil, fmt.Errorf("manifest document %d has apiVersion %q and kind %q; both are required",
				i, obj.GetAPIVersion(), obj.GetKind())
		}

		objs = append(objs, obj)
	}
}

// apply sends every object to the cluster, in order, with SERVER-SIDE APPLY.
//
// The verb is what makes this idempotent, and idempotent is a requirement, not
// a nicety: Task 7 may run bring-up again over a cluster that already has the
// provisioner, and a Create would fail AlreadyExists on every object. An apply
// converges instead — the same document a second time is a no-op. Force is set
// because the second apply would otherwise conflict with the fields the first
// one claimed, which is the same failure wearing a different name.
//
// Order is the manifest's, and it matters: the Namespace is the first document
// and everything inside it would otherwise be refused on a fresh cluster.
//
// The RESTMapper is a parameter rather than built here so this function can be
// driven against a stub API server without stubbing discovery too.
func apply(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper,
	objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		gvk := obj.GroupVersionKind()

		// The CLUSTER decides whether a kind is namespaced, not the document:
		// a copy-pasted `namespace:` on a cluster-scoped object would otherwise
		// address it through a namespace and get a 404 the manifest cannot
		// explain.
		mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("mapping %s %q to a cluster resource: %w", gvk.Kind, obj.GetName(), err)
		}

		resource := dyn.Resource(mapping.Resource)

		var target dynamic.ResourceInterface = resource

		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			target = resource.Namespace(obj.GetNamespace())
		}

		// Unreachable for anything decode produced — the content came from JSON
		// in the first place — and checked because the alternative is a silent
		// nil body.
		data, err := json.Marshal(obj.Object)
		if err != nil {
			return fmt.Errorf("encoding %s %q: %w", gvk.Kind, obj.GetName(), err)
		}

		if _, err := target.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{
			FieldManager: fieldManager,
			Force:        new(true),
		}); err != nil {
			return fmt.Errorf("applying %s %q: %w", gvk.Kind, obj.GetName(), err)
		}
	}

	return nil
}

// InstallStorage installs local-path-provisioner into a bootstrapped cluster
// and makes it the default StorageClass.
//
// It is IDEMPOTENT — see apply — so a caller that retries after a partial
// failure, or runs bring-up over a cluster that already has it, gets
// convergence rather than AlreadyExists.
//
// It needs no kubectl, kustomize or helm on the host: the manifest is embedded
// and applied through client-go.
//
// kubeconfig is SECRET — a CA and a client key — and is neither logged nor
// placed in an error; see errSecretParse.
func InstallStorage(ctx context.Context, kubeconfig []byte) error {
	// First, because it is the only step that costs nothing and the only one
	// whose failure is entirely this repository's fault.
	objs, err := manifestObjects()
	if err != nil {
		return err
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return errSecretParse("kubeconfig")
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building a Kubernetes client: %w", err)
	}

	// Discovery is what tells apply whether each kind is namespaced. It is
	// asked of the CLUSTER rather than hardcoded here because a wrong answer is
	// a 404 with nothing pointing at the cause, and the memory cache keeps it
	// to one round trip per API group.
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("building a discovery client: %w", err)
	}

	return apply(ctx, dyn, restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient)), objs)
}
