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

func TestDestroyOwnerWithJoinersTakesThemFirst(t *testing.T) {
	h := &hvf{stateRoot: t.TempDir()}
	rec := &memberCalls{}
	h.members = rec.ops()

	owner, joiner := ownerAndJoiner(t)
	ownerDir := member(t, h, owner, true)
	joinerDir := member(t, h, joiner, true)

	if err := destroyMachine(context.Background(), h, machineFile(t, owner), destroyOptions{withJoiners: true}); err != nil {
		t.Fatalf("destroy --with-joiners: %v", err)
	}

	if len(rec.calls) == 0 || !strings.HasPrefix(rec.calls[0], "leave 127.0.0.1:50001") {
		t.Errorf("cluster calls = %q, want the joiner to leave etcd first", rec.calls)
	}

	if exists(joinerDir) || exists(ownerDir) {
		t.Error("destroy --with-joiners left a state directory behind")
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
