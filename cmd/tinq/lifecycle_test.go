package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// memberCalls records what destroy asked of the cluster, in order.
type memberCalls struct {
	calls    []string
	leaveErr error
}

func (r *memberCalls) ops() *memberOps {
	return &memberOps{
		leave: func(_ context.Context, talosconfig []byte, endpoint string) error {
			r.calls = append(r.calls, "leave "+endpoint+" "+string(talosconfig))
			return r.leaveErr
		},
		remove: func(_ context.Context, talosconfig []byte, via, peer string) error {
			r.calls = append(r.calls, "remove via "+via+" peer "+peer+" "+string(talosconfig))
			return r.leaveErr
		},
		deleteNode: func(_ context.Context, _ []byte, addr string) error {
			r.calls = append(r.calls, "deleteNode "+addr)
			return nil
		},
	}
}

// member records m as a configured member of the site: a machine record, the
// artifacts destroy reads, and a disk, so Observe reports it Stopped — or
// Running, with a decoy process standing in for qemu.
func member(t *testing.T, h *hvf, m *unstructured.Unstructured, running bool) string {
	t.Helper()

	seed(t, h, m)
	dir := h.dir(m)

	for name, body := range map[string]string{
		"talosconfig":  "talosconfig-of-" + m.GetName(),
		"kubeconfig":   "kubeconfig",
		"system.qcow2": "",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if running {
		pid := startDecoy(t, dir)
		if err := os.WriteFile(filepath.Join(dir, "qemu.pid"), []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// machineFile writes m where destroy reads it from.
func machineFile(t *testing.T, m *unstructured.Unstructured) string {
	t.Helper()

	b, err := yaml.Marshal(m.Object)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), m.GetName()+".yaml")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func ownerAndJoiner(t *testing.T) (owner, joiner *unstructured.Unstructured) {
	t.Helper()

	owner = netMachine(t, "cp0", 50000, ownerNet)
	joiner = netMachine(t, "cp1", 50001, joinerNet)
	joiner.Object["spec"].(map[string]interface{})["joins"] = "cp0"

	return owner, joiner
}

func exists(dir string) bool {
	_, err := os.Stat(dir)
	return err == nil
}

func TestDestroyOwnerWithJoinersIsRefused(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir()}
	rec := &memberCalls{}
	h.members = rec.ops()

	owner, joiner := ownerAndJoiner(t)
	ownerDir := member(t, h, owner, false)
	joinerDir := member(t, h, joiner, false)

	err := destroyMachine(context.Background(), h, machineFile(t, owner), destroyOptions{})
	if err == nil {
		t.Fatal("an owner with a joiner was destroyed")
	}

	for _, want := range []string{"cp0", "cp1", "--with-joiners", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}

	if !exists(ownerDir) || !exists(joinerDir) {
		t.Error("a refused destroy removed a state directory")
	}

	if len(rec.calls) != 0 {
		t.Errorf("a refused destroy touched the cluster: %v", rec.calls)
	}
}

// A RUNNING joiner leaves etcd by itself, with its own credential, through its
// own forward — and only then is its VM destroyed.
func TestDestroyRunningJoinerLeavesEtcdFirst(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir()}
	rec := &memberCalls{}
	h.members = rec.ops()

	owner, joiner := ownerAndJoiner(t)
	member(t, h, owner, true)
	joinerDir := member(t, h, joiner, true)

	h.members.leave = func(_ context.Context, talosconfig []byte, endpoint string) error {
		if !exists(joinerDir) {
			t.Error("the joiner was destroyed before it left etcd")
		}

		rec.calls = append(rec.calls, "leave "+endpoint+" "+string(talosconfig))

		return nil
	}

	if err := destroyMachine(context.Background(), h, machineFile(t, joiner), destroyOptions{}); err != nil {
		t.Fatalf("destroy of a running joiner: %v", err)
	}

	want := []string{"leave 127.0.0.1:50001 talosconfig-of-cp1", "deleteNode 10.254.0.12"}
	if strings.Join(rec.calls, "|") != strings.Join(want, "|") {
		t.Errorf("cluster calls = %q, want %q", rec.calls, want)
	}

	if exists(joinerDir) {
		t.Error("the joiner's state directory survived its destroy")
	}
}

// A STOPPED joiner cannot leave by itself. It is removed, by its cluster
// address, through a member that is running.
func TestDestroyStoppedJoinerIsRemovedThroughARunningMember(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir()}
	rec := &memberCalls{}
	h.members = rec.ops()

	owner, joiner := ownerAndJoiner(t)
	member(t, h, owner, true)
	member(t, h, joiner, false)

	if err := destroyMachine(context.Background(), h, machineFile(t, joiner), destroyOptions{}); err != nil {
		t.Fatalf("destroy of a stopped joiner: %v", err)
	}

	if len(rec.calls) == 0 || rec.calls[0] != "remove via 127.0.0.1:50000 peer 10.254.0.12 talosconfig-of-cp1" {
		t.Errorf("cluster calls = %q, want the member removed through cp0's forward", rec.calls)
	}
}

func TestDestroyJoinerThatCannotLeaveIsRefusedUnlessForced(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir()}
	rec := &memberCalls{leaveErr: errors.New("etcd: unavailable")}
	h.members = rec.ops()

	owner, joiner := ownerAndJoiner(t)
	member(t, h, owner, false)
	joinerDir := member(t, h, joiner, true)

	err := destroyMachine(context.Background(), h, machineFile(t, joiner), destroyOptions{})
	if err == nil || !strings.Contains(err.Error(), "not removed from etcd") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("destroy = %v, want a refusal naming etcd and --force", err)
	}

	if !exists(joinerDir) {
		t.Fatal("a refused destroy removed the joiner")
	}

	if err := destroyMachine(context.Background(), h, machineFile(t, joiner), destroyOptions{force: true}); err != nil {
		t.Fatalf("destroy --force: %v", err)
	}

	if exists(joinerDir) {
		t.Error("destroy --force left the joiner in place")
	}
}

// chain is a three-member cluster where cp2 joined THROUGH cp1, not cp0. A
// joiner's state directory holds the cluster's secrets too, so this is a
// cluster tinq builds.
func chain(t *testing.T) (cp0, cp1, cp2 *unstructured.Unstructured) {
	t.Helper()

	cp0, cp1 = ownerAndJoiner(t)
	cp2 = netMachine(t, "cp2", 50002, "cidr: 10.254.0.0/24\naddress: 10.254.0.13\n")
	cp2.Object["spec"].(map[string]interface{})["joins"] = "cp1"

	return cp0, cp1, cp2
}

// --with-joiners on the machine that CREATED the cluster takes all of it, so
// no member survives to need etcd membership cleaned up. It must not ask
// etcd, whatever the members' power states: a stopped cluster has no member to
// ask through, and one with a single member up has no quorum to answer.
func TestDestroyWholeClusterDoesNotTouchEtcd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running [3]bool
	}{
		{"every member stopped", [3]bool{false, false, false}},
		{"only the owner running, no quorum", [3]bool{true, false, false}},
		{"every member running", [3]bool{true, true, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &hvf{stateRoot: t.TempDir()}
			rec := &memberCalls{leaveErr: errors.New("etcd: no quorum")}
			h.members = rec.ops()

			cp0, cp1, cp2 := chain(t)
			dirs := []string{
				member(t, h, cp0, tc.running[0]),
				member(t, h, cp1, tc.running[1]),
				member(t, h, cp2, tc.running[2]),
			}

			if err := destroyMachine(context.Background(), h, machineFile(t, cp0), destroyOptions{withJoiners: true}); err != nil {
				t.Fatalf("destroy --with-joiners: %v", err)
			}

			for _, d := range dirs {
				if exists(d) {
					t.Errorf("destroy --with-joiners left %s behind", d)
				}
			}

			if len(rec.calls) != 0 {
				t.Errorf("a whole-cluster destroy touched the cluster: %q", rec.calls)
			}
		})
	}
}

// --with-joiners on a member that itself JOINED takes only part of the
// cluster: the owner survives, and every member taken leaves etcd first —
// cp2, which joined through cp1, as well as cp1.
func TestDestroyJoinerWithJoinersStillLeavesEtcd(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir()}
	rec := &memberCalls{}
	h.members = rec.ops()

	cp0, cp1, cp2 := chain(t)
	ownerDir := member(t, h, cp0, true)
	cp1Dir := member(t, h, cp1, true)
	cp2Dir := member(t, h, cp2, false)

	if err := destroyMachine(context.Background(), h, machineFile(t, cp1), destroyOptions{withJoiners: true}); err != nil {
		t.Fatalf("destroy --with-joiners: %v", err)
	}

	want := []string{
		"remove via 127.0.0.1:50000 peer 10.254.0.13 talosconfig-of-cp2", "deleteNode 10.254.0.13",
		"leave 127.0.0.1:50001 talosconfig-of-cp1", "deleteNode 10.254.0.12",
	}
	if strings.Join(rec.calls, "|") != strings.Join(want, "|") {
		t.Errorf("cluster calls = %q, want %q", rec.calls, want)
	}

	if exists(cp1Dir) || exists(cp2Dir) || !exists(ownerDir) {
		t.Errorf("cp1 gone=%v cp2 gone=%v owner kept=%v; want all true",
			!exists(cp1Dir), !exists(cp2Dir), exists(ownerDir))
	}
}

func TestDestroyOwnerForceOrphansTheJoiners(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir()}
	rec := &memberCalls{}
	h.members = rec.ops()

	owner, joiner := ownerAndJoiner(t)
	ownerDir := member(t, h, owner, false)
	joinerDir := member(t, h, joiner, false)

	if err := destroyMachine(context.Background(), h, machineFile(t, owner), destroyOptions{force: true}); err != nil {
		t.Fatalf("destroy --force: %v", err)
	}

	if exists(ownerDir) || !exists(joinerDir) {
		t.Errorf("destroy --force: owner gone=%v, joiner kept=%v; want both true", !exists(ownerDir), exists(joinerDir))
	}

	if len(rec.calls) != 0 {
		t.Errorf("destroy --force of an owner touched the cluster: %v", rec.calls)
	}
}

// A single VM with none of the multi-node fields is destroyed exactly as
// before: no refusal, and no cluster call.
func TestDestroyPlainVMIsUnchanged(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir()}
	rec := &memberCalls{}
	h.members = rec.ops()

	m := netMachine(t, "solo", 50000, "")
	dir := member(t, h, m, true)

	if err := destroyMachine(context.Background(), h, machineFile(t, m), destroyOptions{}); err != nil {
		t.Fatal(err)
	}

	if exists(dir) || len(rec.calls) != 0 {
		t.Errorf("plain destroy: dir kept=%v, cluster calls=%v", exists(dir), rec.calls)
	}
}
