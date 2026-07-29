// Package driverkit is the half that every XSite driver repeats.
//
// Factored AFTER two drivers existed (provider-hvf, provider-gcpmin), not
// before. Extracting a skeleton from one instance is guessing about which parts
// are shared; extracting it from two is reading.
//
// What is here is the GC CONTRACT, which is identical everywhere:
//
//	list -> hold a finalizer -> observe the external system -> create if absent
//	     -> destroy BEFORE dropping the finalizer
//
// What is deliberately NOT here is anything a substrate decides for itself: its
// SCC shape, how it tags artifacts with the site, how it resolves a neutral
// profile name. Pulling those up would be exactly the lowest-common-denominator
// flattening that hides each provider's orphan classes (ARCHITECTURE.md D6).
package driverkit

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// Driver is the substrate-specific half: three verbs against one external
// system. Everything else in this package is the same for all of them.
type Driver interface {
	// Observe asks the EXTERNAL SYSTEM whether the resource exists, and returns
	// status fields to publish. It must never consult a local state file —
	// talosctl's `cluster show` deserialises state.yaml and reports a long-dead
	// cluster as present, which is the failure this signature exists to prevent.
	Observe(ctx context.Context, m *unstructured.Unstructured) (exists bool, status map[string]interface{}, err error)

	// Create provisions it. Called only when Observe reported absent, so it may
	// assume nothing exists; it must still be safe to retry after a partial
	// failure, since the next tick will call it again.
	Create(ctx context.Context, m *unstructured.Unstructured) error

	// Destroy removes it, INCLUDING every artifact in its SCC. Must be
	// idempotent: already-gone is success, or a repeated delete tick wedges the
	// finalizer forever. Returning an error BLOCKS deletion — that is the point.
	// A teardown that reports success while stranding a resource is the leak the
	// whole design exists to prevent.
	Destroy(ctx context.Context, m *unstructured.Unstructured) error
}

type Config struct {
	GVR       schema.GroupVersionResource
	Finalizer string
	Interval  time.Duration
}

// Run is the reconcile loop. It owns the finalizer dance and status publishing
// so no driver can get that half subtly wrong.
func Run(ctx context.Context, cfg Config, d Driver) error {
	kubeconfig := flag.Lookup("kubeconfig").Value.String()
	rc, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("kubeconfig: %w", err)
	}
	dc, err := dynamic.NewForConfig(rc)
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}
	if cfg.Interval == 0 {
		cfg.Interval = 10 * time.Second
	}

	log.Printf("%s driver up — reconciling every %s", cfg.GVR.Resource, cfg.Interval)
	for {
		list, err := dc.Resource(cfg.GVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Printf("list: %v", err)
		} else {
			for i := range list.Items {
				m := &list.Items[i]
				if err := reconcile(ctx, dc, cfg, d, m); err != nil {
					log.Printf("%s: %v", m.GetName(), err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.Interval):
		}
	}
}

func reconcile(ctx context.Context, dc dynamic.Interface, cfg Config, d Driver, m *unstructured.Unstructured) error {
	ri := dc.Resource(cfg.GVR)

	// DELETE FIRST. Destroy must succeed before the finalizer goes, so a failed
	// teardown blocks deletion instead of leaking silently.
	if m.GetDeletionTimestamp() != nil {
		if err := d.Destroy(ctx, m); err != nil {
			return fmt.Errorf("destroy (deletion BLOCKED, which is correct): %w", err)
		}
		log.Printf("%s: destroyed", m.GetName())
		_, err := ri.Patch(ctx, m.GetName(), "application/merge-patch+json",
			[]byte(`{"metadata":{"finalizers":[]}}`), metav1.PatchOptions{})
		return err
	}

	if !hasFinalizer(m, cfg.Finalizer) {
		body := fmt.Sprintf(`{"metadata":{"finalizers":["%s"]}}`, cfg.Finalizer)
		_, err := ri.Patch(ctx, m.GetName(), "application/merge-patch+json", []byte(body), metav1.PatchOptions{})
		return err // re-read next tick
	}

	exists, st, err := d.Observe(ctx, m)
	if err != nil {
		return fmt.Errorf("observe: %w", err)
	}
	if exists {
		return publish(ctx, ri, m, st, true, "Running", "observed in the external system")
	}

	if err := d.Create(ctx, m); err != nil {
		_ = publish(ctx, ri, m, nil, false, "CreateFailed", err.Error())
		return fmt.Errorf("create: %w", err)
	}
	log.Printf("%s: created", m.GetName())
	return nil // next tick observes it
}

func publish(ctx context.Context, ri dynamic.ResourceInterface, m *unstructured.Unstructured,
	st map[string]interface{}, ready bool, reason, msg string) error {
	status := map[string]interface{}{}
	for k, v := range st {
		status[k] = v
	}
	s := "False"
	if ready {
		s = "True"
	}
	status["observedGeneration"] = m.GetGeneration()
	status["conditions"] = []interface{}{map[string]interface{}{
		"type": "Ready", "status": s, "reason": reason, "message": msg,
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}}
	b, _ := json.Marshal(map[string]interface{}{"status": status})
	_, err := ri.Patch(ctx, m.GetName(), "application/merge-patch+json", b, metav1.PatchOptions{}, "status")
	return err
}

func hasFinalizer(m *unstructured.Unstructured, f string) bool {
	for _, x := range m.GetFinalizers() {
		if x == f {
			return true
		}
	}
	return false
}

// Kubeconfig registers the flag every driver needs. Call before flag.Parse.
func Kubeconfig() {
	flag.String("kubeconfig", os.Getenv("KUBECONFIG"), "control plane kubeconfig")
}

// Str reads a string out of a resource, empty if absent.
func Str(m *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedString(m.Object, path...)
	return v
}
