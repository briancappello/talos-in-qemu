package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/coglative/talos-in-qemu/driverkit"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// HOST CAPACITY FOR A CLUSTER OF SEVERAL VMS. Each VM declares its own memory,
// and nothing stops three of them from adding up to more than the host has. A
// host that swaps turns an etcd heartbeat into a missed election, which then
// presents as a Talos bug, three layers away from the cause. So a joiner is
// refused, with the numbers, before it boots.

// memoryMarginPercent is the share of host memory the site's VMs may declare.
// The rest is the host's: its own processes, QEMU's overhead per VM, and the
// page cache that keeps the qcow2 images fast.
const memoryMarginPercent = 85

// hostMemTotalMB reads the host's physical memory in MiB.
func hostMemTotalMB() (int, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0, fmt.Errorf("reading hw.memsize: %w", err)
		}

		b, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parsing hw.memsize: %w", err)
		}

		return int(b >> 20), nil
	}

	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck

	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, fmt.Errorf("parsing MemTotal: %w", err)
			}

			return kb / 1024, nil
		}
	}

	return 0, errors.New("/proc/meminfo has no MemTotal")
}

// checkCapacity refuses m when the running VMs of its site, plus m, declare
// more memory than the margin allows.
func (h *hvf) checkCapacity(m *unstructured.Unstructured) error {
	memTotal := h.memTotal
	if memTotal == nil {
		memTotal = hostMemTotalMB
	}

	total, err := memTotal()
	if err != nil {
		return fmt.Errorf("checking host memory before a joiner boots: %w", err)
	}

	machines, err := siteMachines(h.stateRoot, driverkit.Str(m, "spec", "site"), h.dir(m))
	if err != nil {
		return err
	}

	mine := toMB(str(driverkit.Str(m, "spec", "memory"), "2Gi"))
	sum := mine

	var lines []string

	for _, o := range machines {
		if o.GetName() == m.GetName() {
			continue
		}

		if state, _, err := h.Observe(context.Background(), o.Unstructured); err != nil || state != driverkit.Running {
			continue
		}

		mb := toMB(str(driverkit.Str(o.Unstructured, "spec", "memory"), "2Gi"))
		sum += mb
		lines = append(lines, fmt.Sprintf("    %-16s %6d MiB  (running)", o.GetName(), mb))
	}

	limit := total * memoryMarginPercent / 100
	if sum <= limit {
		return nil
	}

	lines = append(lines, fmt.Sprintf("    %-16s %6d MiB  (this machine)", m.GetName(), mine))

	return fmt.Errorf("%s would bring site %s to %d MiB of VM memory, over %d MiB "+
		"(%d%% of this host's %d MiB)\n\n%s\n\n"+
		"  a host that swaps turns etcd heartbeats into lost elections. Lower spec.memory,\n"+
		"  or stop a VM of the site first",
		m.GetName(), driverkit.Str(m, "spec", "site"), sum, limit, memoryMarginPercent, total,
		strings.Join(lines, "\n"))
}
