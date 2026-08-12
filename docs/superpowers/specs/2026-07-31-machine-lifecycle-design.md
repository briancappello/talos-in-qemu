# Machine lifecycle: separating power state from existence

Date: 2026-07-31
Branch: `feat/lifecycle`, twelve commits on `upstream/main`
Status: implemented. D1–D10 all shipped, including the two rungs this design
originally expected to defer — see "What shipped" below.

## Goal

Make shutdown/reboot a distinct operation from destroy, so a machine's disks —
the installed OS and the user's PVCs — survive an intentional stop.

Before this, `TalosMachine` had two states: running, or gone. The only way to
stop a machine was `destroy`, which deletes `system.qcow2` and `data.qcow2`
along with it. That makes the cluster disposable-by-construction, which is
defensible for a throwaway dev target and untenable for anything that
accumulates state.

## What shipped, against what this document first planned

This design was written when the work was going to be split across a branch
stack, with `cluster/` arriving in a later branch. That is not what happened:
`platform` and `cluster` were squash-merged upstream first, and the CLI was
converted from stdlib `flag` to cobra in the same window. This branch therefore
starts from a tree that already has `cluster.AuthenticatedClient`, `WaitAPI` and
a cobra command tree, and the two rungs the original text called deferred were
built here rather than left for someone else:

- **D6's graceful rung shipped.** `hvf.shutdownGuest` is a real implementation,
  not a stub that returns an error (`cmd/tinq/main.go:585`).
- **D8 and D9 shipped.** `cluster.Up` is idempotent and resumes a configured
  machine (`cluster/up.go:159`).
- **The verb is a cobra SUBCOMMAND**, `tinq stop <machine.yaml>`, not a `-stop`
  flag (`cmd/tinq/main.go:152`). Every `-verb` spelling in the original text was
  wrong by the time the code was written.

The decisions below are stated as they were decided, with a **Shipped** note
wherever the implementation went somewhere the design did not anticipate.

## Why this is a missing verb, not an architectural limit

Three findings from the existing source, each of which shrank the work:

1. **`destroy()` is already two operations welded together.** It signals the
   QEMU pid, then `os.RemoveAll(dir)`. Stop is destroy minus one line.

2. **`ensureQcow2` is already idempotent, deliberately.** Its own comment:
   *"Never recreated, never resized: system.qcow2 holds the installed OS and
   data.qcow2 holds the user's PVCs. create() runs on EVERY reconcile tick where
   Observe reported absent."* So calling `create()` against an existing state dir
   re-execs QEMU on the **installed** system disk with **intact** PVCs — no
   reinstall, no re-bootstrap. Start already worked; nothing named it.

3. **`Observe` returned `false` whenever the pid was dead**, regardless of
   whether the disks were still there. The model had two states where it needed
   three. That is the actual gap, and everything else follows.

## Scope

**In:** the tri-state model, the `Driver` interface change, `spec.powerState`,
a `Stop` verb, `up` idempotency, bootstrap-state detection, and the
`processAlive` PID-reuse fix.

**Out, deliberately:**

- **Auto-start after a host reboot.** Nothing in-cluster can start its own
  machine — the cluster is down, so the controller is down. Solving it needs a
  host-level unit (systemd/launchd) or a host-resident controller. Decided to
  defer; recorded here so it is visibly a decision.
- **Cluster-wide stop/start ordering.** Belongs to a resource that *composes*
  `TalosMachine`s. The CRD's "granularity is deliberate: one resource == one VM"
  note already reserves that boundary.
- **Multi-node networking.** VM-to-VM connectivity is a SLIRP limitation and a
  separate feature.
- **Health probing.** `Ready` means a verified live process, not a healthy
  cluster.
- **Concurrent reconciliation.** Serial reconciliation lets one stopping machine
  stall every other. Knowingly not fixed, with the trigger condition and the
  per-key-locking argument recorded on `driverkit.Run` itself
  (`driverkit/driverkit.go:113-135`). It is a stall; the alternative failure is
  two QEMU processes against one state dir, which is a corrupted disk.

## The tension this design had to answer

`driverkit.go` forbade, in terms, what this change does:

> *"Observe asks the EXTERNAL SYSTEM whether the resource exists... **It must
> never consult a local state file** — talosctl's `cluster show` deserialises
> state.yaml and reports a long-dead cluster as present, which is the failure
> this signature exists to prevent."*

A stopped machine is precisely "state on disk, no process," so detecting one
means reading exactly what that comment refused.

**The rule worth keeping is narrower than the sentence that stated it: never
report a dead thing as `Ready`.** talosctl's sin was reporting a dead cluster as
present *and usable*. This design reports a dead machine as present *and
stopped*, which claims nothing is usable.

It ends up **strengthening** the invariant: `Ready=True` now requires a
**verified** live process — the old code trusted a bare PID, which after a host
reboot can be a completely unrelated process. Reading disk only ever
distinguishes `Absent` from `Stopped`, and neither claims usability. The
rewritten contract is on the `State` type and on `hvf.Observe`.

## Decisions

### D1 — Tri-state `Observe`, one new verb

```go
// State is what the EXTERNAL SYSTEM reports, never what we wish were true.
type State int

const (
    Absent  State = iota // no disks: never created, or destroyed
    Stopped              // disks exist, nothing is running
    Running              // a VERIFIED process for THIS machine is alive
)
```

```go
Observe(ctx, m) (state State, status map[string]interface{}, err error)

// Create brings the machine to Running from EITHER Absent or Stopped, so it is
// as much "start" as "create" — the name is kept for compatibility, not
// accuracy. From Absent it allocates disks and boots. From Stopped it re-execs
// the hypervisor against the EXISTING disks: the installed OS and the user's
// PVCs are reused, never recreated. ensureQcow2 has always behaved this way;
// this comment is the first time the interface admits it.
// Must remain safe to retry after a partial failure.
Create(ctx, m) error

// Stop takes Running -> Stopped WITHOUT destroying anything. Idempotent:
// already-stopped is success. It asks the GUEST to shut down; it does not kill
// the hypervisor, which is a power cut the guest never learns about.
Stop(ctx, m) error

Destroy(ctx, m) error
```

Rejected: **four verbs** (`Create` + `Start`). `create()` already implements both
paths, so `Start` would be a second interface method delegating to one body —
ceremony without separation, and every future driver author forced to implement
both.

Rejected: **keeping `exists bool` and reporting power in `status`.** The state
machine would key off a magic string in an untyped map, and `exists` would
silently change meaning from "is running" to "has disks" while keeping its type.
Every caller keeps compiling while meaning something different — source
compatibility bought with semantic breakage.

`Create` keeps its name to avoid breaking a generic interface; the docstring
carries the honesty instead. Shipped as written, `driverkit/driverkit.go:47-102`.

### D2 — `reconcile` becomes a state machine

Desired state is `spec.powerState`, defaulting to `Running`.

| observed | desired | action |
|---|---|---|
| Absent | Running | `Create` |
| Stopped | Running | `Create` (= start) |
| Running | Running | publish, no-op |
| Running | Stopped | `Stop` |
| Stopped | Stopped | publish, no-op |
| Absent | Stopped | `Create`, then `Stop` on the next tick |

**On `Absent`+`Stopped`:** converge rather than refuse. Talos cannot be installed
without booting, so "exists but never booted" is empty disks impersonating a
machine. Converging costs one wasted boot in a rare case; refusing leaves the
resource permanently un-converged, which is not how a Kubernetes controller
should behave.

Shipped as the pure function `plan(observed, desired) (create, stop bool)`,
`driverkit/driverkit.go:241`, so the table is asserted without an API server.

**Shipped beyond the design: the standalone verbs do not read
`spec.powerState`.** A manifest carrying `powerState: Stopped` boots anyway under
`tinq apply` or `tinq up`. That path runs *before* any control plane exists to
hold desired state, and its one job is to get a node up; `tinq stop` is the verb
that halts a machine there. The asymmetry is user-visible, so it is stated on
`standalone` (`cmd/tinq/main.go:237-244`), in the CRD's `powerState` description
and in the README rather than left to be discovered.

### D3 — Two conditions, `Synced` and `Ready`

`publish()` wrote only `Ready`, which cannot distinguish *deliberately stopped*
from *failed to start* — and a stopped machine showing a bare `Ready=False`
forever cries wolf.

This repo already aims at Crossplane (`rbac.yaml`, the `provider-talos`
references), so adopt its convention:

- **`Synced`** — the reconciler applied spec without error. A machine stopped on
  purpose is `Synced=True`.
- **`Ready`** — the machine is running. Stopped ⇒ `Ready=False, reason=Stopped`.

**Shipped beyond the design: `Synced` earns a printer column too.** The design
asked only for a `PowerState` column. That is not enough: a machine stopped on
purpose and a machine whose `Create` just failed both print `Power=Stopped
Ready=False` — byte-identical rows, and one of them is an incident. The CRD
therefore carries `Power`, `Ready` and `Synced`, ordered to match Crossplane's
`READY`/`SYNCED` convention (`crd/talosmachine.yaml:45-62`).

The status body is built by the pure `statusPatch`, split from the `Patch` call
for the same reason `plan` is a function: a failed verb reporting `Synced=True`
would be a lie, and a lie no test could catch is one that ships.

### D4 — `Observe` keys `Absent` on `system.qcow2`, not the state dir

`system.qcow2` is the discriminator because it is exactly what `create()` would
reuse. Both partial-failure paths then land correctly with no special case:
died before `ensureQcow2` ⇒ `Absent` ⇒ Create retries; disks made but QEMU never
launched ⇒ `Stopped` ⇒ Create re-execs.

**`Observe` stays read-only.** Unlinking a stale pidfile on discovering `Stopped`
would be tidy, and is refused: an observer with side effects is how a status call
becomes a mutation. QEMU's `-pidfile` truncates on next start, so staleness
resolves itself.

**Shipped differently in one place:** the design sketched a second hand-rolled
scan of `spec.hostForwards` to build `apiEndpoint`. The implementation calls
`talosEndpoint(m)` instead, which is the same function `up` uses to reach the
node. Two answers to one question is two answers waiting to disagree, and the
hand-rolled loop had its own bug — it reported `127.0.0.1:0` for an entry with a
`guestPort` and no `hostPort`, an address published as status that cannot answer
(`cmd/tinq/main.go:467-475`).

### D5 — `platform.ProcessMatches` (the PID-reuse fix)

`processAlive` was `syscall.Kill(pid, 0) == nil`. It verified only that *some*
signalable process held that PID — never that it was this machine's QEMU.

A stale pidfile used to be transient. **Stop/start makes "state dir present,
stale pidfile" the normal resting state**, consulted by every `Observe`, and
after a host reboot PIDs are reallocated from low numbers. Two consequences:

1. `Observe` reports a stopped machine as `Running`, so start no-ops and the
   cluster never returns.
2. `destroy()` and `Stop` SIGTERM then SIGKILL that PID — **killing an unrelated
   process**, and with several machines, plausibly a *different machine's* QEMU.

This is a latent bug that the feature makes reachable and load-bearing, so it
belongs to this change.

The state dir path is machine-unique and **already appears in the QEMU argv**
(`-pidfile <dir>/qemu.pid`, `-drive file=<dir>/system.qcow2`), so nothing new has
to be recorded:

- **Linux:** read `/proc/<pid>/cmdline` (NUL-separated), check it contains the
  state dir path. No fork, exact.
- **macOS:** no `/proc`. `proc_pidpath` yields only the executable, which proves
  it is *a* QEMU but not *which machine's* — useless once two run. Shell out to
  `ps -o command= -p <pid>`. Crude, but this repo already shells out to
  `qemu-img` and `qemu-system-*`.

It lives in `platform/` because it is host-specific, which is what that package
is for.

**Shipped with two corrections the design did not see, both found by testing:**

- **The signature takes `dir`, not a free-form `token`, and matches at a path
  BOUNDARY.** A bare directory match is a PREFIX match. Machine UIDs are
  `bootstrap-<ns>-<name>` built from user-chosen names, so a machine called `t`
  has a state dir that is a strict prefix of `t2`'s — and a plain substring
  search says `t`'s pid matches `t2`'s running QEMU. That is the exact failure
  this function exists to prevent, reached from the other direction: `t`'s stale
  pidfile names `t2`'s pid, so `t`'s `Stop` SIGKILLs `t2`'s VM. `machineToken`
  appends the separator, and every caller goes through it rather than assembling
  a token three times (`platform/process.go:51-73`).
- **`ps` needs `-ww`.** Without it `ps` truncates to the terminal width and a
  QEMU argv is long enough that the state-dir token lands past the cut — a
  running machine reads as stopped, so `Observe` keeps trying to start a VM that
  is already up. `ps` only truncates when it believes it has a width, so writing
  to a pipe hides it, which is why a plain `go test` did not catch it
  (`platform/process.go:97-113`).

**Known limitation, stated on the function:** it assumes QEMU runs as our own
uid. `kill(pid, 0)` returns `EPERM` for a live process owned by another user, so
a QEMU launched under a different uid (sudo, a systemd unit) would read as
`Stopped` forever while the VM keeps running.

### D6 — `Stop`: ask the guest, escalate loudly

```
1. Observe; if not Running → return nil                      (idempotent)
2. read <stateDir>/talosconfig → cluster.AuthenticatedClient
   → client.Shutdown(ctx), bounded by a 15s REQUEST timeout
3. wait for the process to exit    (poll ProcessMatches, 60s)
4. still alive → SIGTERM, wait 5s
5. still alive → SIGKILL, wait 5s
```

`Shutdown` is on **MachineService** (`client.Shutdown`, machinery
`client/client.go`), not `LifecycleService` — Lifecycle carries only `Install`
and `Upgrade`.

**SIGTERM to the QEMU process is a power cut**, not a shutdown: it kills the
emulator and the guest never learns anything happened. etcd survives it (the WAL
is fsynced), but "reboot" should not mean "yank the cord" every time, and a
workload's in-flight writes are the real exposure.

**Maintenance-mode machines cannot be shut down gracefully** — `Shutdown` needs
authentication they cannot satisfy. They have no talosconfig, so `shutdownGuest`
returns without dialling and they go straight to the signal path. That is safe
rather than a compromise: a maintenance node is a booted ISO with no applied
config and nothing persistent to corrupt.

**Escalation must be announced.** A silent SIGKILL after a "graceful" shutdown is
how you discover much later that your stops were never clean.

**Shipped, with four things the design did not have:**

- **The talosconfig is read from the machine's own state dir, never from
  `$TALOSCONFIG`.** The credentials that stop a machine must be *that* machine's,
  and the operator's environment may be pointed anywhere. It is a secret: never
  logged, never interpolated into an error, which two tests assert
  (`TestShutdownGuestNeverQuotesTheTalosconfig`).
- **A 15s budget on the Shutdown REQUEST**, distinct from the 60s power-off
  wait. `ctx` reaches `Stop` unbounded (cobra's, or the controller loop's), and a
  gRPC call is only fail-fast once the channel reports `TRANSIENT_FAILURE`. A
  guest whose apid accepts the TCP connection and never completes the TLS
  handshake leaves the channel `CONNECTING` forever, and the call blocks with
  it — so `tinq stop` on a half-wedged node would hang instead of falling
  through to the ladder. A wedged guest must still be stoppable.
- **Every signal is gated on `ProcessMatches`, the first one included**, and the
  gate is `pid <= 0`, not `pid == 0`. `kill(0, sig)` is not a no-op: POSIX sends
  it to every process in our own process group, and `readPid` reports 0 for a
  pidfile that has gone — which a concurrent `destroy` causes — so an ungated
  ladder would SIGTERM and then SIGKILL `tinq` itself. `kill(-1, sig)` is worse
  still, and `readPid` runs the file through `strconv.Atoi`, so a partially
  written pidfile reading `-1` yields a negative pid rather than a parse error.
  Measured against the ungated version: `halt(strangerPid, dir)` returned nil in
  ~10µs having SIGTERMed a live unrelated process, and reported success
  (`cmd/tinq/main.go:619-689`).
- **`destroy` shares that ladder** rather than keeping its own copy. It used to
  have an inline 50 × 100ms loop over a bare `kill(pid, 0)` — the same semantics
  written twice — and the two copies diverged exactly where it hurts: the
  `destroy` copy signalled whatever the pidfile named, gating nothing.

The design predicted one of these correctly and it is worth keeping the note:
the likeliest outcome of a real `client.Shutdown` is an error *caused* by the
guest going away (the RPC drops mid-power-off), which logs "graceful shutdown
unavailable" and arrives at the ladder holding a pid that has just exited. That
is the recycled-pid case, reached by the ordinary success path.

### D7 — `Destroy` stays a power cut, deliberately

It deletes the disks immediately afterwards, so a clean guest shutdown buys
nothing and costs up to 60s. Say so in the code: the asymmetry between `Stop` and
`Destroy` is the entire point of the feature, and an unexplained asymmetry reads
as an oversight. Shipped at `cmd/tinq/main.go:1007-1011`.

### D8 — `up` becomes idempotent

`up` brings a machine to Running from whatever state it is in. `stop` is the only
genuinely new CLI verb.

**Bootstrap-state detection needs no new mechanism.** `cluster/client.go` already
documents the discriminator:

> *"That mutual authentication is also a DISCRIMINATOR, and the waits below rely
> on it: a node still in maintenance mode cannot satisfy it... That is what makes
> 'the authenticated API answers' a meaningful stage signal **without asking the
> node what stage it is in**."*

So `WaitMaintenance` versus `WaitAPI` already separates the two, by which client
can complete a handshake — the same philosophy as `Observe`.

```
Create()                          # re-exec qemu on the EXISTING disks
│
├─ talosconfig absent  → an `apply` machine, never configured
│                        WaitMaintenance → config → apply-config
│                        → bootstrap → kubeconfig → storage
│
└─ talosconfig present → steps 5, 6 and 7 ANNOUNCED AS SKIPPED
                         WaitBootstrapReady (a thin alias for WaitAPI)
                         → bootstrap (see D9) → kubeconfig → storage
```

Reading `talosconfig` from the state dir is fetching a **credential**, not a
status: the authenticated call cannot be attempted without it, and the claim
still comes from whether the handshake succeeds. A talosconfig sitting beside a
node that never took its config fails the wait rather than being believed.

etcd needs no help — single node, data on the system disk, Talos restarts it.

**Shipped, with three things the design did not have:**

- **Skipped steps keep their own numbers and are printed.** The transcript is
  ten numbered steps and the numbering is what an operator reads the sequence
  by; a resumed run jumping from `[ 4/10]` to `[ 8/10]` would read as a bring-up
  that lost three steps rather than one that had already passed them. Closing
  the gap by renumbering is worse: two meanings for `[ 5/10]` and nothing to
  tell them apart (`cluster/up.go:300-331`).
- **Step 10 RE-RUNS rather than being skipped.** `InstallStorage` is a
  server-side apply of a pinned manifest, so it converges on the same objects
  instead of failing `AlreadyExists`. Skipping it would leave a machine stopped
  between steps 9 and 10 with no StorageClass, and a PVC stuck `Pending` as the
  only symptom.
- **`up` adopts a Running VM but must NOT adopt a Stopped one.** The `Boot`
  closure tests `state == Running`, not `state != Absent`. A stopped machine has
  disks and no process, so there is no pid to adopt — `status` is the
  `{stateDir}` map, `toInt(nil)` is 0, and `cluster.Up` would report a VM whose
  process does not exist and then wait out the whole maintenance budget against
  an address nothing is listening on. Widening that test is a hang, not a
  misprint (`cmd/tinq/main.go:405-419`).

### D9 — Tolerate "already bootstrapped" instead of probing for it

`up` applies config, reboots, *then* bootstraps etcd. Stop in that window and
`WaitAPI` **succeeds** (apid is serving with real PKI) while etcd was never
bootstrapped. Skipping bootstrap would then hang waiting for a node that can
never become Ready.

Rather than add a probe: **attempt the bootstrap and treat "already
bootstrapped" as success.** Talos rejects a second bootstrap on a non-empty etcd
data directory, so tolerating that specific rejection makes the step idempotent
and collapses the edge case into the normal path.

**The exact error is now pinned down, and it is the gRPC CODE that matches, not
the message.** Talos v1.13.7,
`internal/app/machined/internal/server/v1alpha1/v1alpha1_server.go:457`:

```go
if entries, _ := os.ReadDir(constants.EtcdDataPath); len(entries) > 0 {
    return nil, status.Error(codes.AlreadyExists, "etcd data directory is not empty")
}
```

That is the only `AlreadyExists` the `Bootstrap` RPC can return. The code is part
of the API contract and the sentence is not, so matching on the sentence is a
bring-up that breaks on an upstream rewording — and breaks by swallowing a real
failure or by failing a healthy cluster, neither of which the transcript could
explain. `client.StatusCode` rather than grpc's `status.Code` because it
unwraps: the error arrives through a `%w`, and machinery wraps multi-node replies
in a multierror that `status.Code` reads as `Unknown`
(`cluster/up.go:655-677`).

### D10 — CRD surface

```yaml
spec:
  powerState:
    type: string
    enum: [Running, Stopped]
    default: Running          # existing machines keep working unchanged
status:
  powerState: { type: string } # observed, may lag spec
```

Shipped, plus the `Power` and `Synced` printer columns argued for under D3.

**Shipped beyond the design: `status.powerState` has no default, and the column
is empty until the machine converges.** `publish` runs only on the converged
tick — a create or stop tick returns and lets the *next* tick observe — so a
machine converging on `Stopped` from `Absent` prints `<none> → <none> → <none> →
Stopped`. The wasted boot D2 accepts is visible in tinq's own log and in the VM
actually running, not in `kubectl`. Documented in the README rather than left as
a surprise.

## Testing

`driverkit` had **no covering tests** before this change, and this change put a
state machine there.

- **`reconcile`'s transition table** — pure logic over (observed, desired) →
  expected driver calls, with a fake `Driver`. Highest value per line in the
  change.
- **`Observe`'s state determination** — table test over (`system.qcow2` present,
  pidfile present, process matches) → expected `State`, using filesystem
  fixtures.
- **`Stop`'s escalation ladder** — asserted against a decoy process that ignores
  SIGTERM, so "graceful timed out, escalated to SIGTERM, then SIGKILL" is proven
  rather than hoped for.
- **`ProcessMatches`** — the cmdline parsing is separated from the syscall so
  parsing is testable without spawning processes, and `psCmdline` is called
  directly so the `-ww` regression is covered on a Linux CI runner.

**Seams that did not exist** — `platform.ProcessMatches` and the Talos client
construction had to be injectable. That was part of the work, not a bonus.

## Forward compatibility

Multiple nodes and clusters are wanted eventually. This design does not foreclose
them, and one part becomes more necessary:

- **Multi-cluster already works.** State lives at `~/.hvf/<site>/<uid>/`, so
  clusters are isolated by `site`.
- **`TalosMachine` is deliberately one-VM-per-resource**, so N machines is N CRs
  with per-machine `powerState`. Nothing here assumes a single machine.
- **`ProcessMatches` matters more with N machines.** With one VM a mismatched PID
  is an annoyance; with several, `Stop`/`destroy` could SIGKILL a *different
  machine's* QEMU — and the prefix-collision fix under D5 is exactly that failure
  found before it happened.
- **Serial reconciliation is the thing to revisit when a second machine can
  exist.** The trigger is that condition, not a date.

**Known boundary: `Stop` is quorum-unaware.** Complete for a single node. For an
HA control plane, stopping 2 of 3 loses etcd quorum, and a well-behaved stop
would need `etcd leave` or an outright refusal. Named here so it is a known edge
rather than a surprise.

## Outcome

**Verified on real hardware (Linux/KVM, v1.13.7 amd64):**

- `tinq stop` on a bootstrapped node asked the guest over the Talos API and the
  guest powered itself off in **36s** — inside the 60s graceful budget, so the
  signal ladder was never reached.
- `tinq stop` then `tinq up` resumed the same bootstrapped node in **1m14s**.
  The transcript printed steps 5, 6 and 7 as skipped with their reasons, and
  step 8 as "already bootstrapped (the node refused a second one)" — which is
  D9's `codes.AlreadyExists` path executing against a real etcd.

**Verified by test, not by hardware:**

- The `Absent → Running → Stopped` convergence and the `Synced`/`Ready` split.
  `driverkit` is exercised through a fake `Driver`, so what is proven is the
  state machine, not a VM.
- The SIGTERM → SIGKILL escalation, against a decoy process that ignores
  SIGTERM. No live Talos guest has ever needed that ladder in a real run,
  because the one real stop answered gracefully.

**Not verified, and it should stay written down:**

- **macOS has never run any of this.** `psCmdline` is called directly by a test
  on Linux, which proves the parsing and the `-ww` flag, and proves nothing
  about `ps` on darwin. The `runtime.GOOS` dispatch that selects it has never
  executed on darwin. Inherited unverified from the platform-abstraction work
  and still unverified here.
- **The write-talosconfig-before-apply window.** Step 6 writes the talosconfig
  *before* step 7 applies it, deliberately, so the artifacts that explain a
  failed apply survive it. A config written to the state dir but never accepted
  by the node therefore leaves the two disagreeing — the file says configured,
  the node is still in maintenance mode — and no wait on this side can end.
  **A retry cannot repair that one**; it is `tinq destroy` and try again, which
  the error message says. Idempotent is not the same as resumable, and this is
  the case where the difference shows.

**Stale elsewhere in the repo, as a consequence:** `README.md`'s "Not done yet"
bullets still say the graceful rung has never met a real guest and that the
`stop` → `up` round trip has never been run end to end. Both predate the runs
above and are now wrong. Correcting them is a separate change to a separate
file.

## Open questions

1. ~~The exact Talos error returned by a second `Bootstrap` on a non-empty etcd
   data directory (D9).~~ **Answered:** `codes.AlreadyExists`, "etcd data
   directory is not empty", raised in exactly one place — v1.13.7
   `internal/app/machined/internal/server/v1alpha1/v1alpha1_server.go:457`. The
   code is matched; the sentence is not. Verified against a real second
   bootstrap during the `stop` → `up` run above.
2. ~~Whether `Stop` should refuse when the machine is mid-bootstrap, or stop
   anyway and rely on D9's idempotency to resume.~~ **Answered: stop anyway.**
   D9 tolerates the second bootstrap and the window is exactly what it exists
   for. There is no probe to add and nothing to keep in step.
3. **macOS verification of `ProcessMatches` — still open.** The `ps` path has
   never executed on darwin, as the platform-abstraction work was, and it is now
   load-bearing for `Stop` and `destroy` rather than only for `Observe`.
