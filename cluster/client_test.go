package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	blockpb "github.com/siderolabs/talos/pkg/machinery/api/resource/definitions/block"
	"github.com/siderolabs/talos/pkg/machinery/cel"
	"github.com/siderolabs/talos/pkg/machinery/cel/celenv"
	"github.com/siderolabs/talos/pkg/machinery/client"
	"github.com/siderolabs/talos/pkg/machinery/config/types/block/blockhelpers"
	"github.com/siderolabs/talos/pkg/machinery/proto"
	"github.com/siderolabs/talos/pkg/machinery/resources/block"
	netres "github.com/siderolabs/talos/pkg/machinery/resources/network"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Everything in this file obeys config_test.go's rule: nothing derived from
// generated material reaches t.Errorf/t.Fatalf/t.Logf except through redact()
// or redactErr(). A talosconfig is a CA and a client key; a kubeconfig is a CA
// and a client key. The probes take both.

// runBounded runs f in a goroutine and fails the test if it has not returned
// within limit.
//
// Every wait in client.go takes a timeout, and the whole point of a timeout is
// that it is HONOURED. A test that simply calls the wait and asserts on its
// error cannot tell "returned at the deadline" from "ignored the deadline and
// happened to return" — and a mutant that drops the deadline entirely would
// hang the package until `go test`'s own 10-minute panic, which reads as
// infrastructure trouble rather than as a failing assertion. This turns that
// mutant into a one-line failure at a bound of our choosing.
func runBounded(t *testing.T, limit time.Duration, f func() error) error {
	t.Helper()

	done := make(chan error, 1)

	go func() { done <- f() }()

	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("did not return within %s: the deadline is not being honoured", limit)

		return nil
	}
}

// acceptOnlyListener is qemu's hostfwd in miniature: it completes the TCP
// handshake and then says nothing at all, forever.
func acceptOnlyListener(t *testing.T) (addr string, accepted *atomic.Int64) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { ln.Close() })

	accepted = new(atomic.Int64)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			accepted.Add(1)

			// Held open, never read from, never written to. Closing here
			// would hand the client a fast RST, which is the EASY case; the
			// case that fools a dial probe is the one where the socket stays
			// up and silent.
			t.Cleanup(func() { conn.Close() })
		}
	}()

	return ln.Addr().String(), accepted
}

// THE trap-1 regression test.
//
// qemu's hostfwd accepts on the HOST. The guest may have no listener at all —
// may not have booted a kernel — and a TCP dial to the forwarded port still
// succeeds. Any probe built on net.Dial reports a dead VM as ready, and the
// caller then applies a config into a void.
func TestWaitMaintenanceRejectsAnAcceptOnlyListener(t *testing.T) {
	t.Parallel()

	addr, _ := acceptOnlyListener(t)

	const timeout = 2 * time.Second

	start := time.Now()

	err := runBounded(t, 15*time.Second, func() error {
		return WaitMaintenance(context.Background(), addr, timeout)
	})
	if err == nil {
		t.Fatal("a socket that accepts but never speaks Talos was reported READY\n" +
			"  reason: qemu hostfwd accepts on the host even when nothing listens in the guest,\n" +
			"  so readiness has to be a real Talos API call, never a dial")
	}

	if elapsed := time.Since(start); elapsed < timeout {
		t.Errorf("gave up after %s, before the %s deadline\n"+
			"  reason: a node that is still booting refuses connections for a while; a probe that\n"+
			"  stops at the first refusal never sees the node come up: %s",
			elapsed, timeout, redactErr(err))
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout is not reported as a deadline: %s", redactErr(err))
	}
}

// The authenticated probe has the same host to fool it, and reaches the same
// forwarded port. It gets the same test rather than trusting that it shares an
// implementation, because sharing one is a decision a later edit can undo.
func TestWaitAPIRejectsAnAcceptOnlyListener(t *testing.T) {
	t.Parallel()

	addr, _ := acceptOnlyListener(t)

	const timeout = 2 * time.Second

	start := time.Now()

	err := runBounded(t, 15*time.Second, func() error {
		return WaitAPI(context.Background(), mustGenerateDefault(t).Talosconfig, addr, timeout)
	})
	if err == nil {
		t.Fatal("a socket that accepts but never speaks Talos was reported READY")
	}

	if elapsed := time.Since(start); elapsed < timeout {
		t.Errorf("gave up after %s, before the %s deadline: %s", elapsed, timeout, redactErr(err))
	}
}

// WaitBootstrapReady is the one whose TIMING is the trap: it must be reachable
// while the node is `booting`, because `running` is unreachable until etcd
// exists and etcd is what the bootstrap it gates creates. Nothing in this
// package may make it wait for a stage.
func TestWaitBootstrapReadyRejectsAnAcceptOnlyListener(t *testing.T) {
	t.Parallel()

	addr, _ := acceptOnlyListener(t)

	const timeout = 2 * time.Second

	err := runBounded(t, 15*time.Second, func() error {
		return WaitBootstrapReady(context.Background(), mustGenerateDefault(t).Talosconfig, addr, timeout)
	})
	if err == nil {
		t.Fatal("a socket that accepts but never speaks Talos was reported READY")
	}
}

// WaitBootstrapReady must be the AUTHENTICATED wait, and the talosconfig is
// what proves it is.
//
// A maintenance-mode node answers its own API happily while holding no config
// at all, so a bootstrap gated on the maintenance API fires into a machine that
// has not installed anything — and the failure surfaces much later, as a node
// that never joins. The credentials are the discriminator: only a client
// carrying the cluster PKI can tell the two states apart, so a wait that does
// not need the talosconfig is not waiting for what it claims.
//
// Rubbish credentials must therefore stop it DEAD, and quickly: the timeout
// here is six times the bound, so a wait that shrugged the talosconfig off and
// went polling would blow it.
func TestWaitBootstrapReadyNeedsTheClusterPKI(t *testing.T) {
	t.Parallel()

	addr, _ := acceptOnlyListener(t)

	err := runBounded(t, 5*time.Second, func() error {
		return WaitBootstrapReady(context.Background(), []byte("\tnot a talosconfig\n"), addr, 30*time.Second)
	})
	if err == nil {
		t.Fatal("bootstrap-readiness was reported without usable credentials")
	}
}

func TestWaitsRefuseAnEmptyEndpoint(t *testing.T) {
	t.Parallel()

	// Long enough that a wait which does NOT refuse up front is unmistakable:
	// it would sit here retrying a nonsense address for half a minute.
	const timeout = 30 * time.Second

	for name, wait := range map[string]func(string) error{
		"WaitMaintenance": func(endpoint string) error {
			return WaitMaintenance(context.Background(), endpoint, timeout)
		},
		"WaitAPI": func(endpoint string) error {
			return WaitAPI(context.Background(), mustGenerateDefault(t).Talosconfig, endpoint, timeout)
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := runBounded(t, 5*time.Second, func() error { return wait("") })
			if err == nil {
				t.Fatal("an empty endpoint was accepted")
			}
		})
	}
}

// Neither constructor may fall back to an AMBIENT talosconfig.
//
// Machinery's default when it is given neither a TLS config nor a config is
// WithDefaultConfig(), which reads ~/.talos/config — some other cluster's
// credentials, belonging to whoever ran talosctl on this machine last. Linking
// machinery instead of shelling out to talosctl exists precisely to keep
// ambient state out of a bring-up, and dropping either option here would let it
// back in through the side door.
//
// TALOSCONFIG is pointed at a path that does not exist, which is what makes the
// check deterministic: on a developer's laptop the fallback would silently
// SUCCEED and pin nothing.
func TestClientsUseTheGivenEndpointAndNoAmbientConfig(t *testing.T) {
	// No t.Parallel: t.Setenv.
	t.Setenv("TALOSCONFIG", filepath.Join(t.TempDir(), "there-is-no-talosconfig-here"))

	// Never dialled — client.New builds a lazy gRPC channel and connects on the
	// first RPC, so no listener is needed to inspect what it was built with.
	const endpoint = "127.0.0.1:65000"

	ctx := t.Context()

	for name, build := range map[string]func() (*client.Client, error){
		"MaintenanceClient": func() (*client.Client, error) {
			return MaintenanceClient(ctx, endpoint)
		},
		"AuthenticatedClient": func() (*client.Client, error) {
			return AuthenticatedClient(ctx, mustGenerateDefault(t).Talosconfig, endpoint)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := build()
			if err != nil {
				t.Fatalf("could not build a client with no ambient talosconfig on this machine: %s",
					redactErr(err))
			}

			defer c.Close() //nolint:errcheck

			if got := c.GetEndpoints(); !slices.Equal(got, []string{endpoint}) {
				t.Errorf("dials %v, want [%s]\n"+
					"  reason: the port is the HOST side of a qemu forward, which the caller chose and\n"+
					"  the talosconfig does not know; machinery would default it to apid's own 50000",
					got, endpoint)
			}
		})
	}
}

// marker is deliberately SEVEN characters.
//
// Machinery's YAML decoder quotes the scalar it choked on, and it TRUNCATES
// that quote: a 200-character secret is reported as "SEKRITx...". Measured, not
// assumed — the first draft of these tests used a 19-character marker, and the
// mutant that wraps the parse error with %w sailed through both of them because
// the full marker never appeared in any error. A marker no longer than the
// truncation is what makes the assertion able to fail at all.
const marker = "SEKRITx"

// parserFingerprints are how a decoder's own message announces itself. They are
// asserted on IN ADDITION to the marker, because how much of a document a
// decoder quotes is a version's behaviour and not a promise: today machinery
// leaks seven characters and clientcmd leaks none, and a library that starts
// printing the whole scalar tomorrow would turn a marker-only test green while
// publishing a private key. What this package actually undertakes is that the
// parser's message does not travel at all, and that is what these pin.
var parserFingerprints = []string{"yaml:", "json:"}

func assertNoSecretParserOutput(t *testing.T, what string, err error) {
	t.Helper()

	if err == nil {
		t.Fatalf("a %s that is not a %s was accepted", what, what)
	}

	if strings.Contains(err.Error(), marker) {
		t.Errorf("the error quotes the %s it failed to parse\n"+
			"  reason: that document is a client key and its CA; an error goes to a log, a CI\n"+
			"  transcript and an issue report: %s", what, redactErr(err))
	}

	for _, fingerprint := range parserFingerprints {
		if strings.Contains(err.Error(), fingerprint) {
			t.Errorf("the error carries the decoder's own message (%q)\n"+
				"  reason: that message is the thing that quotes the document, and how much of it\n"+
				"  gets quoted is not a guarantee any YAML library makes: %s",
				fingerprint, redactErr(err))
		}
	}
}

func TestAuthenticatedClientDoesNotLeakTheTalosconfig(t *testing.T) {
	t.Parallel()

	// Valid YAML as far as the scanner is concerned, and a type error for the
	// decoder: `contexts` must be a mapping. The decoder reports the scalar it
	// could not convert, which is where the secret comes out.
	broken := []byte("context: default\ncontexts: " + marker + strings.Repeat("A", 200) + "\n")

	_, err := AuthenticatedClient(context.Background(), broken, "127.0.0.1:50000")

	assertNoSecretParserOutput(t, "talosconfig", err)
}

func TestWaitNodeReadyDoesNotLeakTheKubeconfig(t *testing.T) {
	t.Parallel()

	broken := []byte("apiVersion: v1\nkind: Config\nclusters: " + marker + strings.Repeat("A", 200) + "\n")

	err := runBounded(t, 5*time.Second, func() error {
		return WaitNodeReady(context.Background(), broken, 30*time.Second)
	})

	assertNoSecretParserOutput(t, "kubeconfig", err)
}

// nodeAPI serves exactly one endpoint, /api/v1/nodes, returning the given
// nodes. It is enough to drive WaitNodeReady end to end, success included,
// which no Talos-side test can do without a VM.
func nodeAPI(t *testing.T, nodes ...corev1.Node) []byte {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/nodes" {
			http.NotFound(w, r)

			return
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(corev1.NodeList{Items: nodes}); err != nil {
			t.Errorf("encoding the node list: %s", err)
		}
	}))

	t.Cleanup(srv.Close)

	// Plain HTTP, so there is no CA and no client key here — this kubeconfig
	// is not secret material, unlike the real one.
	return fmt.Appendf(nil, `apiVersion: v1
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
}

func node(name string, conditions ...corev1.NodeCondition) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: conditions},
	}
}

func ready(status corev1.ConditionStatus) corev1.NodeCondition {
	return corev1.NodeCondition{Type: corev1.NodeReady, Status: status}
}

func TestWaitNodeReadyAcceptsAReadyNode(t *testing.T) {
	t.Parallel()

	kubeconfig := nodeAPI(t, node("clvc-cp0", ready(corev1.ConditionTrue)))

	start := time.Now()

	err := runBounded(t, 10*time.Second, func() error {
		return WaitNodeReady(context.Background(), kubeconfig, 5*time.Second)
	})
	if err != nil {
		t.Fatalf("a Ready node was not accepted: %s", redactErr(err))
	}

	// Not a performance assertion: a wait that only ever returns at its
	// deadline would pass the assertion above too.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %s to notice a node that was Ready before the first poll", elapsed)
	}
}

func TestWaitNodeReadyRejects(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		nodes  []corev1.Node
		reason string
	}{
		// The vacuous-truth case. "every node is Ready" is TRUE of no nodes at
		// all, and an API server answers long before the kubelet registers.
		"no nodes at all": {
			nodes:  nil,
			reason: "an empty node list satisfies `all nodes are Ready` and the kubelet has not even registered",
		},
		"Ready=False": {
			nodes:  []corev1.Node{node("clvc-cp0", ready(corev1.ConditionFalse))},
			reason: "the node is registered and explicitly not ready",
		},
		// The one a status==True check gets wrong when it forgets that a
		// condition can be absent-but-reported: Unknown is what a node that
		// stopped heartbeating reports.
		"Ready=Unknown": {
			nodes:  []corev1.Node{node("clvc-cp0", ready(corev1.ConditionUnknown))},
			reason: "Unknown is a lost kubelet, not a ready one",
		},
		// The one a "does any condition say True" check gets wrong.
		"another condition is True, Ready absent": {
			nodes: []corev1.Node{node("clvc-cp0",
				corev1.NodeCondition{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue})},
			reason: "MemoryPressure=True is not readiness; only the Ready condition is",
		},
		"no conditions at all": {
			nodes:  []corev1.Node{node("clvc-cp0")},
			reason: "a registered node with no conditions yet has not reported readiness",
		},
		// Single-node today, but `all` and `any` are the same thing on one
		// node, so the distinction has to be pinned with two.
		"one of two is not Ready": {
			nodes: []corev1.Node{
				node("clvc-cp0", ready(corev1.ConditionTrue)),
				node("clvc-cp1", ready(corev1.ConditionFalse)),
			},
			reason: "one Ready node does not make the cluster ready",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			kubeconfig := nodeAPI(t, tc.nodes...)

			err := runBounded(t, 10*time.Second, func() error {
				return WaitNodeReady(context.Background(), kubeconfig, 2*time.Second)
			})
			if err == nil {
				t.Fatalf("reported ready\n  reason: %s", tc.reason)
			}
		})
	}
}

// A wait that gives up must say what the API SAID, not just that no node was
// ready. The two are wildly different problems — an expired certificate, a
// 503 from a control plane that is still starting, and a kubelet that has not
// registered all look identical once the API's answer is dropped on the floor.
func TestWaitNodeReadyReportsWhyTheAPIRefused(t *testing.T) {
	t.Parallel()

	// A real Status object rather than http.Error's plain text, because that is
	// what client-go surfaces to the caller — and it lets the assertion below
	// be about a string this test chose, not about client-go's wording for a
	// bare 503.
	const refusal = "kube-apiserver is still starting up"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)

		if err := json.NewEncoder(w).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure,
			Code:     http.StatusServiceUnavailable,
			Reason:   metav1.StatusReasonServiceUnavailable,
			Message:  refusal,
		}); err != nil {
			t.Errorf("encoding the refusal: %s", err)
		}
	}))

	t.Cleanup(srv.Close)

	kubeconfig := fmt.Appendf(nil, `apiVersion: v1
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

	err := runBounded(t, 10*time.Second, func() error {
		return WaitNodeReady(context.Background(), kubeconfig, 2*time.Second)
	})
	if err == nil {
		t.Fatal("an API server that answered 503 was reported as a Ready node")
	}

	if !strings.Contains(err.Error(), refusal) {
		t.Errorf("the error does not carry what the API server answered: %s\n"+
			"  reason: dropping it turns every failure into `no nodes are registered yet`, which is\n"+
			"  what a healthy API server says before the kubelet joins", redactErr(err))
	}
}

// A kubeconfig can parse and still be unusable. The Kubernetes client is built
// ONCE, before the loop, so this fails immediately instead of once per second
// for the whole timeout — and it must fail, not proceed with a nil client.
func TestWaitNodeReadyRefusesAnUnusableKubeconfig(t *testing.T) {
	t.Parallel()

	// Parses, validates, and is not a URL any client can be built for.
	kubeconfig := []byte(`apiVersion: v1
kind: Config
clusters:
- name: probe
  cluster:
    server: "://nope"
contexts:
- name: probe
  context:
    cluster: probe
current-context: probe
`)

	err := runBounded(t, 5*time.Second, func() error {
		return WaitNodeReady(context.Background(), kubeconfig, 30*time.Second)
	})
	if err == nil {
		t.Fatal("a kubeconfig no client can be built from was accepted")
	}
}

// waitFor is the retry loop under all four waits, and these are its contract.

func TestWaitForReturnsAsSoonAsTheProbeSucceeds(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	start := time.Now()

	err := runBounded(t, 10*time.Second, func() error {
		return waitFor(context.Background(), time.Minute, "a probe that succeeds", func(context.Context) error {
			calls.Add(1)

			return nil
		})
	})
	if err != nil {
		t.Fatalf("a probe that succeeded was reported as a failure: %s", redactErr(err))
	}

	if got := calls.Load(); got != 1 {
		t.Errorf("probed %d times, want 1", got)
	}

	// Bounded by the INTERVAL, not by some round number: the first attempt
	// happens straight away, before any tick. A loop that sleeps first costs
	// every wait in a bring-up a second it never had to spend, and this is the
	// only assertion that would notice.
	if elapsed := time.Since(start); elapsed >= probeInterval {
		t.Errorf("took %s to report a probe that succeeded immediately, which is a whole %s tick",
			elapsed, probeInterval)
	}
}

func TestWaitForRetriesUntilTheProbeSucceeds(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64

	const attempts = 3

	start := time.Now()

	err := runBounded(t, 30*time.Second, func() error {
		return waitFor(context.Background(), time.Minute, "a probe that comes good", func(context.Context) error {
			if calls.Add(1) < attempts {
				return errors.New("not yet")
			}

			return nil
		})
	})
	if err != nil {
		t.Fatalf("a probe that eventually succeeded was reported as a failure: %s", redactErr(err))
	}

	if got := calls.Load(); got != attempts {
		t.Errorf("probed %d times, want %d", got, attempts)
	}

	// Paced by probeInterval, so three attempts cost two ticks. The bound is
	// what distinguishes the two durations in this file from each other: they
	// are both time.Duration constants and swapping them at the call sites
	// leaves a loop that still retries and still caps its attempts, just at
	// each other's scale.
	if elapsed, limit := time.Since(start), (attempts+1)*probeInterval; elapsed > limit {
		t.Errorf("%d attempts took %s, beyond %s\n"+
			"  reason: the retry pace is probeInterval (%s); probeAttempt (%s) is the per-attempt cap",
			attempts, elapsed, limit, probeInterval, probeAttempt)
	}
}

func TestWaitForReportsTheLastFailure(t *testing.T) {
	t.Parallel()

	// These strings share no substring, so no assertion below can be satisfied
	// by another's text.
	const (
		what      = "the maintenance API"
		probeFail = "connection refused by qemu"
		firstFail = "still installing"
	)

	var calls atomic.Int64

	err := runBounded(t, 15*time.Second, func() error {
		return waitFor(context.Background(), 3*time.Second, what, func(context.Context) error {
			// The LAST failure, not the first: a node that answers differently
			// as it progresses is the normal case, and the first answer is the
			// stalest thing available.
			if calls.Add(1) == 1 {
				return errors.New(firstFail)
			}

			return errors.New(probeFail)
		})
	})
	if err == nil {
		t.Fatal("a probe that never succeeded was reported as success")
	}

	if strings.Contains(err.Error(), firstFail) {
		t.Errorf("the error carries the FIRST probe failure, not the last: %s", redactErr(err))
	}

	// What is being waited FOR, and why it never arrived. Without the first the
	// operator cannot tell which of four waits gave up; without the second the
	// message is "timed out" and nothing else, which is the single most useless
	// error a bring-up tool can print.
	if !strings.Contains(err.Error(), what) {
		t.Errorf("the error does not name what was being waited for: %s", redactErr(err))
	}

	if !strings.Contains(err.Error(), probeFail) {
		t.Errorf("the error does not carry the last probe failure: %s", redactErr(err))
	}
}

// The failure an operator needs is the one the NODE gave, and the last attempt
// before a deadline is the one least likely to have it: it runs with no budget
// left and fails on the clock — client-go's rate limiter refuses before it
// reaches the wire, a gRPC dial gives up mid-handshake. Reporting that as the
// last attempt replaces the 503 with a restatement of the timeout.
//
// Driven through waitFor directly because the timing has to be exact: against a
// real server the deadline lands between attempts and the artifact never
// appears, which is a test that agrees with anything.
func TestWaitForKeepsTheAnswerTheNodeGaveNotTheDeadlineArtifact(t *testing.T) {
	t.Parallel()

	const realAnswer = "kube-apiserver said 503"

	var calls atomic.Int64

	// Not a multiple of probeInterval, so the deadline lands INSIDE the second
	// attempt rather than racing a tick.
	const timeout = 2500 * time.Millisecond

	err := runBounded(t, 15*time.Second, func() error {
		return waitFor(context.Background(), timeout, "a node", func(ctx context.Context) error {
			if calls.Add(1) == 1 {
				return errors.New(realAnswer)
			}

			// Every later attempt is cut short by the caller's deadline, which
			// is what a real client does once the budget is gone.
			<-ctx.Done()

			return ctx.Err()
		})
	})
	if err == nil {
		t.Fatal("a probe that never succeeded was reported as success")
	}

	if !strings.Contains(err.Error(), realAnswer) {
		t.Errorf("the error reports the deadline instead of what the node said: %s\n"+
			"  reason: `context deadline exceeded (last attempt: context deadline exceeded)` tells an\n"+
			"  operator nothing they did not already know", redactErr(err))
	}
}

// An attempt started with a sliver of the budget left cannot reach the node: it
// fails on the clock and its answer replaces the node's. This asserts the loop
// does not start one, and it is deterministic by construction — the timeout is
// two and a half ticks, so the attempt that must not happen would be half a
// tick from the deadline, and the one that must happen is a tick and a half
// from it. Half a second of margin either side, on purpose: the whole reason
// this guard exists is that a knife-edge margin turns into a coin flip.
func TestWaitForDoesNotStartAnAttemptItCannotFinish(t *testing.T) {
	t.Parallel()

	const timeout = 5 * probeInterval / 2

	budgets := make(chan time.Duration, 16)

	err := runBounded(t, 15*time.Second, func() error {
		return waitFor(context.Background(), timeout, "a node", func(ctx context.Context) error {
			// probeAttempt is far longer than this timeout, so the attempt's
			// deadline IS the caller's: what is left here is the whole budget.
			if deadline, ok := ctx.Deadline(); ok {
				budgets <- time.Until(deadline)
			}

			return errors.New("not yet")
		})
	})
	if err == nil {
		t.Fatal("a probe that never succeeded was reported as success")
	}

	close(budgets)

	var attempts int

	for budget := range budgets {
		attempts++

		if budget < probeInterval {
			t.Errorf("attempt %d started with %s left, under one %s tick\n"+
				"  reason: it cannot reach the node in that; it reports the clock, and the clock is\n"+
				"  what the surrounding message already says", attempts, budget, probeInterval)
		}
	}

	if want := int(timeout / probeInterval); attempts != want {
		t.Errorf("made %d attempts in %s, want %d", attempts, timeout, want)
	}
}

func TestWaitForHonoursTheParentContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	defer cancel()

	start := time.Now()

	// A timeout far beyond the bound, so only the CANCEL can end this.
	err := runBounded(t, 10*time.Second, func() error {
		return waitFor(ctx, 10*time.Minute, "a probe that never succeeds", func(context.Context) error {
			return errors.New("not yet")
		})
	})
	if err == nil {
		t.Fatal("a cancelled wait was reported as success")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait does not report cancellation: %s", redactErr(err))
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to notice its context was cancelled", elapsed)
	}
}

// The per-attempt cap is what makes the retry loop a retry loop at all. Against
// qemu's hostfwd a probe does not FAIL, it HANGS: the host completes the
// handshake and the guest never answers. With no cap the first attempt swallows
// the entire budget and the second attempt — the one that would have found a
// node that finished booting in the meantime — never runs.
func TestWaitForCapsEachAttempt(t *testing.T) {
	t.Parallel()

	deadlines := make(chan time.Time, 8)

	err := runBounded(t, 30*time.Second, func() error {
		return waitFor(context.Background(), 10*time.Minute, "a probe that hangs", func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				return errors.New("the attempt has no deadline of its own")
			}

			deadlines <- deadline

			if len(deadlines) >= 2 {
				return nil
			}

			<-ctx.Done()

			return ctx.Err()
		})
	})
	if err != nil {
		t.Fatalf("waiting: %s", redactErr(err))
	}

	first := <-deadlines
	if until := time.Until(first); until > probeAttempt {
		t.Errorf("the first attempt's deadline is %s away, beyond the %s cap\n"+
			"  reason: an attempt that inherits the whole budget turns a hung connection into a\n"+
			"  wait that never retries", until, probeAttempt)
	}
}

// TestAgainstARealNode is the half that no stub can stand in for, and it is the
// half that settled disk.serial for this branch. Skipped unless a real
// maintenance-mode node is pointed at it:
//
//	go run ./cmd/tinq -apply <machine.yaml>
//	TINQ_NODE=127.0.0.1:50000 go test ./cluster -run TestAgainstARealNode -v
//
// It has no build tag on purpose: a tagged test is a test that never compiles
// in CI, and the first thing to rot would be the COSI call below.
func TestAgainstARealNode(t *testing.T) {
	endpoint := os.Getenv("TINQ_NODE")
	if endpoint == "" {
		t.Skip("set TINQ_NODE=host:port to run against a maintenance-mode Talos VM")
	}

	ctx := t.Context()

	start := time.Now()

	if err := WaitMaintenance(ctx, endpoint, 5*time.Minute); err != nil {
		t.Fatalf("WaitMaintenance(%s): %s", endpoint, redactErr(err))
	}

	t.Logf("WaitMaintenance succeeded after %s", time.Since(start))

	// The discriminator the whole bootstrap sequence rests on, checked against
	// the one state that can disprove it. A maintenance-mode node answers the
	// maintenance API — proved a line above — and must NOT answer the
	// authenticated one: its certificate is not signed by this cluster's CA,
	// and it asks for no client certificate. If it ever did answer, WaitAPI
	// would return the moment the ISO booted and bootstrap would fire at a
	// machine with no config on its disk.
	if err := WaitAPI(ctx, mustGenerateDefault(t).Talosconfig, endpoint, 5*time.Second); err == nil {
		t.Error("the AUTHENTICATED Talos API answered on a node that is in maintenance mode\n" +
			"  reason: every wait after ApplyConfiguration uses that as the signal that the\n" +
			"  installed system is up; if maintenance mode satisfies it, they all return early")
	}

	// RISK 1 FROM THE DESIGN SPEC, resolved against a live node: whether a
	// maintenance-mode node reports a populated version tag before any config
	// is applied. If this ever fails, spec.baremetal.talosVersion is the
	// documented fallback.
	//
	// Not fatal: the disk.serial evidence below is the reason this test needs a
	// booted VM at all, and aborting here would cost the whole run to report a
	// question that has its own documented fallback.
	version, err := NodeVersion(ctx, endpoint)

	switch {
	case err != nil:
		t.Errorf("NodeVersion(%s): %s", endpoint, redactErr(err))
	case version == "":
		t.Error("a maintenance-mode node reported no Talos version tag\n" +
			"  reason: adopt pins the installer image to this value; with no tag it " +
			"must fall back to spec.baremetal.talosVersion")
	default:
		t.Logf("the node reports Talos %s", version)
	}

	c, err := MaintenanceClient(ctx, endpoint)
	if err != nil {
		t.Fatalf("MaintenanceClient(%s): %s", endpoint, redactErr(err))
	}

	defer c.Close() //nolint:errcheck

	// The branch's one openly-unverified assumption: config.go selects both the
	// install target and the user volume by disk.serial, and nothing had ever
	// read a serial off a real qemu virtio disk.
	//
	// ONE list, for every question below. The raw resources are what the CEL
	// fallback needs — it converts each one to proto — and toDisks is the same
	// reduction ListDisks performs, so the table logged below is the one adopt
	// refuses on. Calling ListDisks here instead would open a second
	// maintenance connection and gather a SECOND list, leaving the emptiness
	// check guarding something other than what CEL is then evaluated against.
	disks, err := safe.StateListAll[*block.Disk](ctx, c.COSI)
	if err != nil {
		t.Fatalf("listing disks over COSI: %s", redactErr(err))
	}

	if disks.Len() == 0 {
		t.Fatal("the node reports no disks at all")
	}

	// Nothing here is generated material, so it is logged unredacted: this is
	// the evidence.
	t.Logf("disks:\n%s", FormatDisks(toDisks(slices.Collect(disks.All()))))

	// THE ONE ASSUMPTION THIS BRANCH COULD NOT PROVE FROM SOURCE. Whether
	// maintenance mode serves LinkStatuses is a fact about the Talos server,
	// and machinery holds no answer. If this fails, adopt cannot print a links
	// table and spec.baremetal.network.hardwareAddr has to be copied off the
	// node's own console instead.
	//
	// Not fatal, for the same reason the version question above is not: it has
	// a documented fallback, and aborting here would cost the rest of the run.
	//
	// ASKED HERE, ahead of every t.Fatalf the disk questions below still make.
	// Hardware access is transient and this gate gets one run; downstream of
	// those fatals, an unrelated disk regression consumes the run and this
	// question — the only one on the branch nothing else can answer — comes
	// back unreported.
	links, err := safe.StateListAll[*netres.LinkStatus](ctx, c.COSI)

	switch {
	case err != nil:
		t.Errorf("listing LinkStatuses over COSI: %s\n"+
			"  reason: adopt's links table and its carrier check both depend on this call",
			redactErr(err))
	default:
		// toLinks' FILTERED output, not links.Len(). A node reporting nothing
		// but loopback passes a raw COSI count, and this gate then logs a table
		// header over no rows — green, with none of the evidence it exists to
		// produce.
		physical := toLinks(slices.Collect(links.All()))

		if len(physical) == 0 {
			t.Error("the node reports no physical links at all\n" +
				"  reason: adopt chooses a NIC by MAC out of this list, and loopback " +
				"is not one an operator can plug a cable into")

			break
		}

		// Nothing here is generated material, so it is logged unredacted: this
		// is the evidence.
		t.Logf("links:\n%s", FormatLinks(physical))
	}

	in := testInput()

	// config.go's actual selector, taken from the volume it builds rather than
	// retyped here, evaluated against the real disks in machinery's own CEL
	// environment. This is the whole disk.serial question, answered.
	volume, err := userVolume(in.DataDiskSerial)
	if err != nil {
		t.Fatalf("building the user volume: %s", redactErr(err))
	}

	bySerial := volume.ProvisioningSpec.DiskSelectorSpec.Match

	// MatchDisks is the node's own matcher, called here over the node's own
	// COSI state: same expression, same CEL environment, same proto conversion
	// the volume controller performs. Evaluating a hand-rolled equivalent would
	// answer a question nobody asked.
	matched, err := blockhelpers.MatchDisks(ctx, c.COSI, &bySerial)
	if err != nil {
		t.Fatalf("matching disks by serial: %s", redactErr(err))
	}

	matchedBySerial := make([]string, 0, len(matched))
	for _, d := range matched {
		matchedBySerial = append(matchedBySerial, d.Metadata().ID())
	}

	// The fallback the spec recorded when nobody had read a serial off a real
	// qemu disk. It is evaluated here because it is WRONG in a way only a real
	// node shows: a Talos ISO is a read-only virtio-blk device, not a cdrom,
	// and so is the squashfs loop device.
	//
	// MatchDisks cannot be reused for it: MatchDisks hardcodes
	// system_disk=false, which is right for a selector that never mentions the
	// variable and would stack the deck for one that does.
	byElimination, err := cel.ParseBooleanExpression("!system_disk && !disk.cdrom", celenv.DiskLocator())
	if err != nil {
		t.Fatalf("building the fallback selector: %s", redactErr(err))
	}

	var matchedByElimination []string

	for d := range disks.All() {
		spec := &blockpb.DiskSpec{}
		if err := proto.ResourceSpecToProto(d, spec); err != nil {
			t.Fatalf("converting %s to proto: %s", d.Metadata().ID(), redactErr(err))
		}

		matches, err := byElimination.EvalBool(celenv.DiskLocator(), map[string]any{
			"disk": spec,
			// What the node itself would set, emulated from the same serial the
			// install selector uses.
			"system_disk": d.TypedSpec().Serial == in.SystemDiskSerial,
		})
		if err != nil {
			t.Fatalf("evaluating the fallback against %s: %s", d.Metadata().ID(), redactErr(err))
		}

		if matches {
			matchedByElimination = append(matchedByElimination, d.Metadata().ID())
		}
	}

	if !slices.Equal(matchedBySerial, []string{"vdc"}) {
		t.Errorf("the serial selector matched %v, want exactly [vdc]\n"+
			"  reason: disk.serial is what both the install target and the user volume are chosen by",
			matchedBySerial)
	}

	t.Logf("%s matched %v; the %s fallback matched %v",
		bySerial, matchedBySerial, byElimination, matchedByElimination)

	if len(matchedByElimination) < 2 {
		t.Errorf("the fallback selector matched %v — only ambiguous selectors justify the serial\n"+
			"  reason: if elimination were unambiguous on a real node, this test would be arguing "+
			"for a selector nobody needs", matchedByElimination)
	}

}
