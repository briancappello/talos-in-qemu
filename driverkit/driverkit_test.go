package driverkit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
)

func machine(powerState string) *unstructured.Unstructured {
	spec := map[string]interface{}{}
	if powerState != "" {
		spec["powerState"] = powerState
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "t"},
		"spec":     spec,
	}}
}

// The transition table from the design (D2). This is the whole point of the
// change, so it is asserted exhaustively.
func TestPlanTransitions(t *testing.T) {
	cases := []struct {
		name       string
		observed   State
		desired    string
		wantCreate bool
		wantStop   bool
	}{
		{"absent wants running -> create", Absent, "Running", true, false},
		{"stopped wants running -> create (start)", Stopped, "Running", true, false},
		{"running wants running -> noop", Running, "Running", false, false},
		{"running wants stopped -> stop", Running, "Stopped", false, true},
		{"stopped wants stopped -> noop", Stopped, "Stopped", false, false},
		{"absent wants stopped -> create, converge next tick", Absent, "Stopped", true, false},
		{"empty powerState defaults to Running", Stopped, "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCreate, gotStop := plan(tc.observed, desiredPowerState(machine(tc.desired)))
			if gotCreate != tc.wantCreate || gotStop != tc.wantStop {
				t.Fatalf("plan(%v, %q) = create:%v stop:%v, want create:%v stop:%v",
					tc.observed, tc.desired, gotCreate, gotStop, tc.wantCreate, tc.wantStop)
			}
		})
	}
}

func TestDesiredPowerStateDefaultsToRunning(t *testing.T) {
	if got := desiredPowerState(machine("")); got != "Running" {
		t.Fatalf("desiredPowerState(no field) = %q, want Running", got)
	}
}

// plan must never ask for both verbs at once: Create and Stop against the same
// machine in one tick is a boot immediately power-cut, which is how you corrupt
// an install rather than converge on one.
func TestPlanNeverAsksForBothVerbs(t *testing.T) {
	for _, observed := range []State{Absent, Stopped, Running} {
		for _, desired := range []string{"Running", "Stopped", ""} {
			if create, stop := plan(observed, desiredPowerState(machine(desired))); create && stop {
				t.Fatalf("plan(%v, %q) asked to create AND stop", observed, desired)
			}
		}
	}
}

// The condition split is the reason this change exists, so the four cases that
// reach publish are pinned. The failure each row prevents is in its name.
func TestStatusPatchSplitsSyncedFromReady(t *testing.T) {
	cases := []struct {
		name       string
		observed   State
		synced     bool
		ready      bool
		reason     string
		wantSynced string
		wantReady  string
	}{
		// Without this row, a machine stopped ON PURPOSE is indistinguishable
		// in kubectl from one that failed to start — and one of those is an
		// incident someone gets paged for.
		{"deliberately stopped is synced but not ready", Stopped, true, false, "Stopped", "True", "False"},
		{"running is synced and ready", Running, true, true, "Running", "True", "True"},
		// A failed verb reporting Synced=True is a lie: it claims spec was
		// applied when nothing was.
		{"create failure is not synced", Absent, false, false, "CreateFailed", "False", "False"},
		{"stop failure is not synced", Running, false, false, "StopFailed", "False", "False"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Status struct {
					PowerState         string `json:"powerState"`
					ObservedGeneration int64  `json:"observedGeneration"`
					Conditions         []struct {
						Type    string `json:"type"`
						Status  string `json:"status"`
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"conditions"`
				} `json:"status"`
			}
			body := statusPatch(7, map[string]interface{}{"pid": int64(42)},
				tc.observed, tc.synced, tc.ready, tc.reason, "msg")
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("statusPatch produced invalid JSON: %v\n%s", err, body)
			}

			conds := map[string]string{}
			for _, c := range got.Status.Conditions {
				conds[c.Type] = c.Status
				if c.Reason != tc.reason || c.Message != "msg" {
					t.Errorf("%s condition: reason %q msg %q, want %q msg", c.Type, c.Reason, c.Message, tc.reason)
				}
			}
			if len(conds) != 2 {
				t.Fatalf("conditions = %v, want exactly Synced and Ready", conds)
			}
			if conds["Synced"] != tc.wantSynced {
				t.Errorf("Synced = %q, want %q", conds["Synced"], tc.wantSynced)
			}
			if conds["Ready"] != tc.wantReady {
				t.Errorf("Ready = %q, want %q", conds["Ready"], tc.wantReady)
			}
			// status.powerState is what the new printer column reads, and it
			// reports what was OBSERVED, never what was asked for.
			if got.Status.PowerState != tc.observed.String() {
				t.Errorf("status.powerState = %q, want %q", got.Status.PowerState, tc.observed.String())
			}
			if got.Status.ObservedGeneration != 7 {
				t.Errorf("observedGeneration = %d, want 7", got.Status.ObservedGeneration)
			}
		})
	}
}

// Driver-supplied status must survive into the patch; dropping it would blank
// pid and apiEndpoint on every publish.
func TestStatusPatchKeepsDriverFields(t *testing.T) {
	body := statusPatch(1, map[string]interface{}{"stateDir": "/s"}, Running, true, true, "Running", "msg")
	var got map[string]map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["status"]["stateDir"] != "/s" {
		t.Fatalf("stateDir = %v, want /s", got["status"]["stateDir"])
	}
}

// ---------------------------------------------------------------------------
// reconcile: the dispatch itself, not just the pure functions beneath it.
//
// plan and statusPatch being pure and tested proves the TABLE is right and the
// PATCH BODY is right. It proves nothing about the wiring between them, and
// that wiring is where the guarantee lives: reconcile is the only thing that
// decides which verb runs and which of the two booleans reaches which
// condition. Left untested, "CreateFailed reports Synced=True" — the exact lie
// this change exists to prevent — is a one-word edit no test notices.
//
// It is tested with hand-written fakes rather than client-go's dynamic/fake
// because dynamic.Interface is ONE method and NamespaceableResourceInterface
// is twelve, of which reconcile touches exactly one. Everything below imports
// only what driverkit.go already imports, so go.mod does not move.

// fakeDriver records which verbs ran and can fail any of them on demand. The
// counters are the point: a create branch that dispatched to Stop would still
// return nil, and only the counters can tell.
type fakeDriver struct {
	state  State
	status map[string]interface{}

	observeErr error
	createErr  error
	stopErr    error
	destroyErr error

	created   int
	stopped   int
	destroyed int
}

func (f *fakeDriver) Observe(context.Context, *unstructured.Unstructured) (State, map[string]interface{}, error) {
	return f.state, f.status, f.observeErr
}

func (f *fakeDriver) Create(context.Context, *unstructured.Unstructured) error {
	f.created++
	return f.createErr
}

func (f *fakeDriver) Stop(context.Context, *unstructured.Unstructured) error {
	f.stopped++
	return f.stopErr
}

func (f *fakeDriver) Destroy(context.Context, *unstructured.Unstructured) error {
	f.destroyed++
	return f.destroyErr
}

// patchCall is one Patch reconcile made. subresources distinguishes the status
// write from the finalizer write, which go to the same method.
type patchCall struct {
	name         string
	body         []byte
	subresources []string
}

// fakeResource is dynamic.NamespaceableResourceInterface with one method
// implemented. The other eleven panic rather than returning zero values: if
// reconcile ever starts calling Get or Update, a silent nil would turn into a
// confusing nil-deref far from the cause, while a panic names the method.
type fakeResource struct {
	patches []patchCall
	err     error
}

func (f *fakeResource) Patch(_ context.Context, name string, _ types.PatchType, data []byte,
	_ metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	// Copy: callers own the slice they passed and may reuse it.
	f.patches = append(f.patches, patchCall{
		name: name, body: append([]byte(nil), data...), subresources: subresources,
	})
	return nil, f.err
}

func (f *fakeResource) Namespace(string) dynamic.ResourceInterface { panic("unused") }
func (f *fakeResource) Create(context.Context, *unstructured.Unstructured, metav1.CreateOptions, ...string) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (f *fakeResource) Update(context.Context, *unstructured.Unstructured, metav1.UpdateOptions, ...string) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (f *fakeResource) UpdateStatus(context.Context, *unstructured.Unstructured, metav1.UpdateOptions) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (f *fakeResource) Delete(context.Context, string, metav1.DeleteOptions, ...string) error {
	panic("unused")
}
func (f *fakeResource) DeleteCollection(context.Context, metav1.DeleteOptions, metav1.ListOptions) error {
	panic("unused")
}
func (f *fakeResource) Get(context.Context, string, metav1.GetOptions, ...string) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (f *fakeResource) List(context.Context, metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	panic("unused")
}
func (f *fakeResource) Watch(context.Context, metav1.ListOptions) (watch.Interface, error) {
	panic("unused")
}
func (f *fakeResource) Apply(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions, ...string) (*unstructured.Unstructured, error) {
	panic("unused")
}
func (f *fakeResource) ApplyStatus(context.Context, string, *unstructured.Unstructured, metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	panic("unused")
}

// fakeDynamic is dynamic.Interface, which is one method.
type fakeDynamic struct{ ri *fakeResource }

func (f *fakeDynamic) Resource(schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return f.ri
}

const testFinalizer = "test.fleet.io/vm"

// managed is a machine reconcile will actually act on. Without the finalizer,
// reconcile patches it on and returns before ever reaching Observe, so every
// assertion below would pass vacuously.
func managed(powerState string) *unstructured.Unstructured {
	m := machine(powerState)
	m.SetFinalizers([]string{testFinalizer})
	m.SetGeneration(7)
	return m
}

func runReconcile(d *fakeDriver, m *unstructured.Unstructured) (*fakeResource, error) {
	ri := &fakeResource{}
	err := reconcile(context.Background(), &fakeDynamic{ri: ri}, Config{Finalizer: testFinalizer}, d, m)
	return ri, err
}

type gotCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// statusConditions decodes the single status patch reconcile published, and
// fails if it published none, several, or wrote to the main resource instead of
// the status subresource.
func (f *fakeResource) statusConditions(t *testing.T) (map[string]gotCondition, string) {
	t.Helper()
	var body []byte
	for _, p := range f.patches {
		if len(p.subresources) == 1 && p.subresources[0] == "status" {
			if body != nil {
				t.Fatalf("reconcile published status twice: %v", f.patches)
			}
			body = p.body
		}
	}
	if body == nil {
		t.Fatalf("reconcile published no status patch; patches = %v", f.patches)
	}
	var got struct {
		Status struct {
			PowerState string         `json:"powerState"`
			Conditions []gotCondition `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("status patch is not JSON: %v\n%s", err, body)
	}
	conds := map[string]gotCondition{}
	for _, c := range got.Status.Conditions {
		conds[c.Type] = c
	}
	return conds, got.Status.PowerState
}

// A failed Create must NOT claim Synced=True. This is the headline guarantee of
// the whole change: Synced answers "did the reconciler apply spec", and nothing
// was applied. Flipping this one argument is invisible to every pure-function
// test in this file.
func TestReconcileCreateFailureIsNotSynced(t *testing.T) {
	d := &fakeDriver{state: Absent, createErr: errors.New("qemu exploded")}
	ri, err := runReconcile(d, managed("Running"))
	if err == nil {
		t.Fatal("reconcile returned nil after Create failed; the tick must report it")
	}
	if d.created != 1 || d.stopped != 0 {
		t.Fatalf("create=%d stop=%d, want create=1 stop=0", d.created, d.stopped)
	}
	conds, _ := ri.statusConditions(t)
	if conds["Synced"].Status != "False" {
		t.Errorf("Synced = %q after CreateFailed, want False — a failed verb reporting synced is a lie",
			conds["Synced"].Status)
	}
	if conds["Ready"].Status != "False" {
		t.Errorf("Ready = %q after CreateFailed, want False", conds["Ready"].Status)
	}
	if conds["Synced"].Reason != "CreateFailed" {
		t.Errorf("reason = %q, want CreateFailed", conds["Synced"].Reason)
	}
	if conds["Synced"].Message != "qemu exploded" {
		t.Errorf("message = %q, want the driver's error", conds["Synced"].Message)
	}
}

// Same guarantee on the other failing verb. Both branches publish separately,
// so testing one proves nothing about the other.
func TestReconcileStopFailureIsNotSynced(t *testing.T) {
	d := &fakeDriver{state: Running, stopErr: errors.New("qmp refused")}
	ri, err := runReconcile(d, managed("Stopped"))
	if err == nil {
		t.Fatal("reconcile returned nil after Stop failed; the tick must report it")
	}
	if d.stopped != 1 || d.created != 0 {
		t.Fatalf("create=%d stop=%d, want create=0 stop=1", d.created, d.stopped)
	}
	conds, _ := ri.statusConditions(t)
	if conds["Synced"].Status != "False" {
		t.Errorf("Synced = %q after StopFailed, want False", conds["Synced"].Status)
	}
	if conds["Ready"].Status != "False" {
		t.Errorf("Ready = %q after StopFailed, want False", conds["Ready"].Status)
	}
	if conds["Synced"].Reason != "StopFailed" {
		t.Errorf("reason = %q, want StopFailed", conds["Synced"].Reason)
	}
}

// The converged-stopped path is where the two booleans DIFFER, so it is the only
// path that catches them being transposed at the call site. Running/Running is
// True/True and Stopped-on-purpose is True/False; swap the arguments and only
// this case changes.
func TestReconcileConvergedStoppedIsSyncedNotReady(t *testing.T) {
	d := &fakeDriver{state: Stopped, status: map[string]interface{}{"stateDir": "/s"}}
	ri, err := runReconcile(d, managed("Stopped"))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if d.created != 0 || d.stopped != 0 {
		t.Fatalf("converged tick ran a verb: create=%d stop=%d", d.created, d.stopped)
	}
	conds, power := ri.statusConditions(t)
	if conds["Synced"].Status != "True" {
		t.Errorf("Synced = %q, want True — stopped BECAUSE THAT IS WHAT WAS ASKED FOR is synced",
			conds["Synced"].Status)
	}
	if conds["Ready"].Status != "False" {
		t.Errorf("Ready = %q, want False — a stopped machine is not usable", conds["Ready"].Status)
	}
	if conds["Ready"].Reason != "Stopped" {
		t.Errorf("reason = %q, want Stopped", conds["Ready"].Reason)
	}
	if power != "Stopped" {
		t.Errorf("status.powerState = %q, want Stopped", power)
	}
}

func TestReconcileConvergedRunningIsSyncedAndReady(t *testing.T) {
	d := &fakeDriver{state: Running}
	ri, err := runReconcile(d, managed("Running"))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if d.created != 0 || d.stopped != 0 {
		t.Fatalf("converged tick ran a verb: create=%d stop=%d", d.created, d.stopped)
	}
	conds, power := ri.statusConditions(t)
	if conds["Synced"].Status != "True" || conds["Ready"].Status != "True" {
		t.Errorf("Synced=%q Ready=%q, want True/True",
			conds["Synced"].Status, conds["Ready"].Status)
	}
	if power != "Running" {
		t.Errorf("status.powerState = %q, want Running", power)
	}
}

// plan says create; reconcile must dispatch to Create. A branch wired to the
// wrong verb still returns nil and still logs "created", so only the counters
// see it — and the live consequence is a machine that never boots.
func TestReconcileCreateBranchCallsCreate(t *testing.T) {
	d := &fakeDriver{state: Absent}
	ri, err := runReconcile(d, managed("Running"))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if d.created != 1 || d.stopped != 0 {
		t.Fatalf("create=%d stop=%d, want create=1 stop=0", d.created, d.stopped)
	}
	// The acting tick publishes nothing: the NEXT tick observes the result.
	// Publishing here would report the pre-create state as converged.
	for _, p := range ri.patches {
		if len(p.subresources) == 1 && p.subresources[0] == "status" {
			t.Fatalf("create tick published status before re-observing: %s", p.body)
		}
	}
}

// plan says stop; reconcile must actually call the driver. A stop branch that
// only logs leaves the VM running while kubectl reports it converged.
func TestReconcileStopBranchCallsStop(t *testing.T) {
	d := &fakeDriver{state: Running}
	ri, err := runReconcile(d, managed("Stopped"))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if d.stopped != 1 || d.created != 0 {
		t.Fatalf("create=%d stop=%d, want create=0 stop=1", d.created, d.stopped)
	}
	for _, p := range ri.patches {
		if len(p.subresources) == 1 && p.subresources[0] == "status" {
			t.Fatalf("stop tick published status before re-observing: %s", p.body)
		}
	}
}

// A failed Destroy must BLOCK deletion: the finalizer stays, because dropping
// it is how a resource disappears from kubectl while its disks stay on disk.
func TestReconcileFailedDestroyKeepsFinalizer(t *testing.T) {
	m := managed("Running")
	now := metav1.Now()
	m.SetDeletionTimestamp(&now)
	d := &fakeDriver{state: Running, destroyErr: errors.New("rm failed")}
	ri, err := runReconcile(d, m)
	if err == nil {
		t.Fatal("reconcile returned nil after Destroy failed; deletion would proceed and leak")
	}
	if d.destroyed != 1 {
		t.Fatalf("destroy called %d times, want 1", d.destroyed)
	}
	if len(ri.patches) != 0 {
		t.Fatalf("patched after a failed destroy: %v", ri.patches)
	}
}

// Observe failing must not be papered over with a status write: reporting
// anything here would publish a state nobody actually observed.
func TestReconcileObserveFailurePublishesNothing(t *testing.T) {
	d := &fakeDriver{observeErr: errors.New("stat: i/o error")}
	ri, err := runReconcile(d, managed("Running"))
	if err == nil {
		t.Fatal("reconcile returned nil after Observe failed")
	}
	if d.created != 0 || d.stopped != 0 {
		t.Fatalf("acted on an unknown state: create=%d stop=%d", d.created, d.stopped)
	}
	if len(ri.patches) != 0 {
		t.Fatalf("published status from a failed observe: %v", ri.patches)
	}
}
