# Machine Lifecycle Implementation Plan — COMPLETED RECORD

> **This document is a RECORD, not instructions.** The work described here is
> done and merged into `feat/lifecycle`. Every step below is checked, and where
> the implementation went somewhere the plan did not, a **Shipped** note says so
> and names the commit. Do not execute this file; read it to find out why the
> code looks the way it does, and read the tests to find out what is actually
> guaranteed.
>
> Design spec: `docs/superpowers/specs/2026-07-31-machine-lifecycle-design.md`.
> Decisions are referenced as D1–D10.

**Goal:** Give `TalosMachine` a power state distinct from its existence, so
stopping a machine keeps its disks and only `Destroy` deletes them.

**Architecture:** `Observe` returns a tri-state (`Absent`/`Stopped`/`Running`)
instead of a bool; `Stop` joins the `Driver` interface; `reconcile` becomes a
state machine comparing `spec.powerState` to what the host reports. Liveness
stops trusting a bare PID and starts verifying the process is *this machine's*
QEMU.

**Tech Stack:** Go 1.26.5, `k8s.io/apimachinery` unstructured, machinery
v1.13.7, `spf13/cobra`, standard `testing`. No new module dependencies.

## Global Constraints — all held

- **No new module dependencies.** Held. `go.mod` gained one line, and it is a
  correction rather than an addition: `google.golang.org/grpc` moved out of the
  indirect block because `cluster/up.go` now imports `codes` directly
  (`eac9cb4`).
- **`go build ./... && go vet ./... && go test ./...` passes at every commit.**
- Go **1.26.5**, machinery **v1.13.7** — pinned, not bumped.
- Conventional commits, lowercase, no `Co-Authored-By`, no AI attribution.
- Staged by explicit path throughout. No `git add -A`, `git add .`, or
  `git commit -a`.
- **Comment style is load-bearing in this repo:** comments explain *why*, and
  name the failure they prevent. A comment restating the code is worse than
  none.

## Starting point — what the tree actually was

This branch is `feat/lifecycle`, twelve commits on `upstream/main`. There is no
branch stack: `feat/platform-abstraction` and `feat/cluster-bringup` were
squash-merged upstream before this work started, and the CLI was converted from
stdlib `flag` to cobra in the same window (`e1b28cd`).

That is the opposite of what an earlier draft of this plan assumed, and it moved
two things from "someone else's branch" into scope:

- **`cluster/` is present.** `AuthenticatedClient`, `WaitAPI`,
  `WaitBootstrapReady` and `WaitMaintenance` all exist, so D6's graceful rung
  had no reason to be a stub and D8/D9 had no reason to wait.
- **`up` is present**, as a cobra subcommand alongside `apply` and `destroy`.
  The new verb is therefore `tinq stop <machine.yaml>`, a subcommand — **not a
  `-stop` flag**, which is what the original Task 5 described and what no
  version of this code ever had.

## File Structure — as built

```
platform/process.go          NEW  ProcessMatches, machineToken, psCmdline, linuxCmdline
platform/process_test.go     NEW
driverkit/driverkit.go       MOD  State, Observe signature, Stop, plan, statusPatch, publish
driverkit/driverkit_test.go  NEW  transition table + Synced/Ready matrix + reconcile
cmd/tinq/main.go             MOD  hvf.Observe, hvf.Stop, hvf.shutdownGuest, halt, waitGone,
                                  the `stop` cobra subcommand, Destroy's asymmetry comment
cmd/tinq/main_test.go        MOD  Observe states, Stop, halt, shutdownGuest, destroy
cluster/up.go                MOD  idempotent Up: skip 5-7, tolerate AlreadyExists
cluster/up_test.go           MOD  hook-seam assertions for the resumed path
crd/talosmachine.yaml        MOD  spec.powerState, status.powerState, Power + Synced columns
README.md                    MOD  stop vs destroy, powerState, `up` idempotency
```

---

### Task 1: `platform.ProcessMatches` — DONE (`e7d5ac1`)

**Files:** `platform/process.go`, `platform/process_test.go` (both new).

**Produces:** `ProcessMatches(pid int, dir string) bool` — true only when `pid`
is live *and* its command line contains this machine's state dir at a path
boundary.

**Why:** `processAlive` was `syscall.Kill(pid, 0) == nil`, which proves only that
*some* signalable process holds that PID. Stop/start makes "stale pidfile" the
normal resting state, and after a host reboot PIDs are reallocated from low
numbers. See D5.

- [x] **Step 1: Write the failing test**

```go
// The live-process cases run against THIS test binary, so they exercise the
// real host-specific path on whatever platform runs them.
func TestProcessMatchesOurself(t *testing.T)
func TestProcessMatchesRejectsWrongToken(t *testing.T)
func TestProcessMatchesRejectsDeadPid(t *testing.T)   // pids 0, -1, -12345
func TestParseCmdlineJoinsNulSeparatedArgs(t *testing.T)
```

`TestParseCmdlineJoinsNulSeparatedArgs` drives the pure parser directly, so the
NUL-separation quirk is testable without spawning anything:

```go
raw := []byte("qemu-system-x86_64\x00-pidfile\x00/state/dir/qemu.pid\x00")
got := parseCmdline(raw)
// must contain /state/dir, must not contain a NUL
```

- [x] **Step 2: Run it to verify it fails** — FAIL, `undefined: ProcessMatches`.

- [x] **Step 3: Write the implementation**

Cheap liveness first (`kill(pid, 0)`), then the command line. An unreadable
command line returns false: an unproven match is the bug this function exists to
prevent.

- [x] **Step 4: Keep the Linux reader in the SAME file — no build tags**

`platform/platform.go` states the package rule: *"Everything here is decided at
RUNTIME rather than by build tags. Build tags would leave the macOS path
uncompiled on Linux — no type checking, no tests, no compile error until someone
builds on a Mac."* `linuxCmdline` uses only `os.ReadFile` and `strconv`, so it
compiles on darwin unchanged and the `runtime.GOOS` guard selects it.

- [x] **Step 5: Run the tests to verify they pass** — all four PASS.

- [x] **Step 6: Verify the whole package still builds and vets** — clean.

- [x] **Step 7: Commit**

```bash
git add platform/process.go platform/process_test.go
git commit -m "platform: prove a pid is THIS machine's qemu, not merely alive"
```

**Shipped beyond the plan — two real defects found while writing the tests:**

1. **The token is `machineToken(dir)`, not the bare `dir`.** A bare directory
   match is a PREFIX match, and machine UIDs are `bootstrap-<ns>-<name>` built
   from user-chosen names — so a machine called `t` has a state dir that is a
   strict prefix of `t2`'s. Measured against a real qemu-shaped argv:
   `.../bootstrap-default-t` matched `t2`'s running QEMU, when it must not. The
   consequence is the worst one this package exists to prevent: `t`'s stale
   pidfile names `t2`'s pid, so `t`'s `Stop`/`destroy` SIGKILLs `t2`'s VM. Every
   path QEMU carries the dir in is a path *under* it, so appending the separator
   makes a genuine match always succeed and a prefix match always fail. Covered
   by `TestProcessMatchesDoesNotMatchAPrefixOfAnotherMachine` and
   `TestMachineTokenEndsAtAPathBoundary`.
2. **`ps` needs `-ww`.** Without it `ps` truncates to the terminal width, and a
   QEMU argv is long enough that the state-dir token lands past the cut — a
   running machine reads as stopped, so `Observe` keeps trying to start a VM
   that is already up. `ps` only truncates when it believes it has a width (a
   tty, or `COLUMNS` set); writing to a pipe it does not, which is exactly why a
   plain `go test` did not catch it. `psCmdline` was split out of
   `processCmdline` so `TestPsCmdlineSurvivesNarrowTerminal` can call it
   directly and cover the regression on a Linux CI runner, rather than leaving
   it to the `runtime.GOOS` dispatch that never fires there.

The plan's Step 7 named `platform/process_linux.go` and `platform/process_other.go`.
Those files do not exist and never should have: Step 4 forbids build tags, and
one file with a `runtime.GOOS` guard is what Step 4 asks for. The commit staged
`process.go` and `process_test.go`.

---

### Task 2: Tri-state `Observe` — DONE (`e4c9f6c`)

**Files:** `driverkit/driverkit.go`, `cmd/tinq/main.go`, `cmd/tinq/main_test.go`.

**Produces:** `driverkit.State` (`Absent`/`Stopped`/`Running`) and the new
`Observe` signature.

**This task was behaviour-preserving.** A machine with disks and no process
reported `false` before, so `reconcile` called `Create`, which re-execs QEMU on
the existing disks. After it, `Absent` and `Stopped` both still led to `Create`.
Only the *representation* changed; Task 4 gave the two states different
outcomes.

- [x] **Step 1: Write the failing test**

```go
func TestObserveReportsAbsentWithoutSystemDisk(t *testing.T)
func TestObserveReportsStoppedWhenDisksExistButNoProcess(t *testing.T)
```

The second one is the load-bearing case, and the fixture is the whole point: it
writes a pidfile naming a pid that **is alive but is not our QEMU** — this test
binary's own. Before `ProcessMatches` that reported `Running`, which is the bug.

```go
if err := os.WriteFile(filepath.Join(dir, "qemu.pid"),
    []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil { t.Fatal(err) }
// want driverkit.Stopped: the live pid is not this machine's qemu
```

- [x] **Step 2: Run it to verify it fails** — FAIL, `undefined: driverkit.Absent`.

- [x] **Step 3: Add the `State` type to driverkit**

With the docstring that carries the relaxation argument — the rule worth keeping
is "never report a dead thing as Ready", not "never read a file", and the
invariant gets *stronger* because `Running` now demands a verified process.

- [x] **Step 4: Change the `Observe` signature and `Create`'s docstring**

- [x] **Step 5: Update `reconcile` minimally — no behaviour change yet**

- [x] **Step 6: Update `hvf.Observe`**

Keyed on `system.qcow2`, not the state dir, so both partial-failure paths land
correctly with no special case. Read-only: unlinking a stale pidfile here would
be tidy and is refused, because an observer with side effects is how a status
call quietly becomes a mutation.

- [x] **Step 7: Update `standalone`'s verb switch**

`destroy` now runs for `Stopped` as well as `Running` — correct, since a stopped
machine still has disks to remove, and skipping it is exactly the residue the
SCC rule forbids.

- [x] **Step 8: Run the tests** — all PASS.

- [x] **Step 9: Commit**

**Shipped beyond the plan:**

- **`apiEndpoint` comes from `talosEndpoint(m)`, not a second hand-rolled scan
  of `spec.hostForwards`.** The plan said to keep the inline loop because a
  later branch would extract it; there is no later branch, and the loop had a
  bug of its own — it reported `127.0.0.1:0` for an entry with a `guestPort` and
  no `hostPort`, an address published as status that cannot answer. `Observe`'s
  `apiEndpoint` and the endpoint `up` hands `cluster.Up` are two answers to one
  question, and nothing but a shared call keeps them equal. Covered by
  `TestObserveReportsTheSameEndpointUpUses`.
- **A third `Observe` case:** `TestObserveReportsAbsentWhenDirExistsButDiskDoesNot`
  — the state dir existing is not the discriminator, `system.qcow2` is.
- **`processAlive` was not deleted in this commit.** `destroy` still called it,
  so it was switched to the shared ladder first and the dead function removed on
  its own in `0a73681`. A deletion that has to be argued for is a deletion that
  gets its own commit.

---

### Task 3: The `Stop` verb — DONE (`e4c9f6c`)

**Files:** `driverkit/driverkit.go`, `cmd/tinq/main.go`, `cmd/tinq/main_test.go`.

- [x] **Step 1: Write the failing test**

```go
func TestStopIsIdempotentOnAnAbsentMachine(t *testing.T)
func TestStopIsIdempotentOnAStoppedMachine(t *testing.T)
```

- [x] **Step 2: Run it to verify it fails** — FAIL, `h.Stop undefined`.

- [x] **Step 3: Add `Stop` to the interface**

With the sentence that makes it a contract rather than a name: *"It must ask the
resource to stop, not merely kill whatever is hosting it. For a VM those are
different events — the second is a power cut the guest never learns about."*

- [x] **Step 4: Implement `hvf.Stop`**

```
1. Observe; if not Running → return nil              (idempotent)
2. shutdownGuest  → ask the guest over the Talos API (Task 6)
3. waitGone(60s)  → the guest powering itself off
4. SIGTERM, waitGone(5s)
5. SIGKILL, waitGone(5s)
```

- [x] **Step 5: Add the deliberate-asymmetry comment to `Destroy`**

*"Destroy does NOT ask the guest to shut down, and that is deliberate: the disks
are deleted immediately below, so a clean shutdown buys nothing and costs up to a
minute. The asymmetry with Stop is the entire point of having both — an
unexplained asymmetry reads as an oversight, so this says it out loud."*

- [x] **Step 6: Run the tests** — all PASS.

- [x] **Step 7: Commit**

**Shipped beyond the plan — the signal ladder is `halt`, and the gate is the
point:**

The plan wrote the ladder inline in `Stop` and left `destroy`'s own copy alone.
That was wrong, and the two copies had already diverged where it hurts:
`destroy`'s loop signalled whatever the pidfile named, gating nothing. The ladder
is now `halt(ctx, pid, dir)`, called by both, and the rule lives with the
signals so no caller can route around it:

**Every `syscall.Kill` is gated on `ProcessMatches`, THE FIRST ONE INCLUDED.**
Gating only the later rungs is not a weaker version of this rule, it is the
absence of it — the first signal is the one that lands. Measured against the
ungated version: `halt(strangerPid, dir)` returned **nil in ~10µs having
SIGTERMed a live unrelated process**, and reported success, because `waitGone`
read the non-match as "gone".

The gate is `pid <= 0`, not `pid == 0`, and the negative half earns its place:

- `kill(0, sig)` is **not** a no-op. POSIX sends it to every process in our own
  process group, and `readPid` reports 0 for a pidfile that has gone — which a
  concurrent `destroy` causes, since it removes the whole state dir. An ungated
  ladder would SIGTERM and then SIGKILL `tinq` itself.
- `kill(-1, sig)` is strictly worse: it signals every process the caller may
  signal. `readPid` runs the pidfile through `strconv.Atoi`, so a corrupt or
  partially written one reading `-1` yields a **negative** pid rather than a
  parse failure.

Covered by `TestHaltRefusesToSignalPidZero`,
`TestHaltRefusesToSignalAPidItCannotProveIsOurs`,
`TestDestroyDoesNotSignalAPidItCannotProveIsOurs`.

Escalation itself is proven, not assumed, against a decoy process that ignores
SIGTERM: `TestHaltEscalatesToSIGKILLWhenSIGTERMIsIgnored`,
`TestDestroyEscalatesToSIGKILLAndStillSweepsTheState`,
`TestStopHaltsAVerifiedProcessAndKeepsTheDisks` (which also asserts the disks are
still there afterwards — the entire feature in one assertion).

---

### Task 4: `spec.powerState` and the reconcile state machine — DONE (`e4c9f6c`, `90379ec`)

**Files:** `crd/talosmachine.yaml`, `driverkit/driverkit.go`,
`driverkit/driverkit_test.go`.

**`driverkit` had no tests at all before this.** This task added the first, and
it covers the highest-value logic in the change.

- [x] **Step 1: Write the failing test — the transition table from D2**

```go
{"absent wants running -> create",                Absent,  "Running", true,  false},
{"stopped wants running -> create (start)",       Stopped, "Running", true,  false},
{"running wants running -> noop",                 Running, "Running", false, false},
{"running wants stopped -> stop",                 Running, "Stopped", false, true},
{"stopped wants stopped -> noop",                 Stopped, "Stopped", false, false},
{"absent wants stopped -> create, converge next", Absent,  "Stopped", true,  false},
{"empty powerState defaults to Running",          Stopped, "",        true,  false},
```

Plus `TestDesiredPowerStateDefaultsToRunning`.

- [x] **Step 2: Run it to verify it fails** — FAIL, `undefined: plan`,
      `undefined: desiredPowerState`.

- [x] **Step 3: Implement `plan` and `desiredPowerState`**

Pure functions, so the table is asserted without an API server.

- [x] **Step 4: Rewrite `reconcile` to use it**

- [x] **Step 5: Update `publish` for two conditions**

- [x] **Step 6: Update the CRD** — `spec.powerState` (enum, default `Running`),
      `status.powerState`, printer columns.

- [x] **Step 7: Verify the CRD is still valid YAML and schema-sane**

```bash
go test ./... && python3 -c "import yaml,sys; d=yaml.safe_load(open('crd/talosmachine.yaml')); \
p=d['spec']['versions'][0]['schema']['openAPIV3Schema']['properties']; \
assert p['spec']['properties']['powerState']['default']=='Running'; \
assert 'powerState' in p['status']['properties']; print('crd ok')"
```
PASS, then `crd ok`.

- [x] **Step 8: Run everything** — all PASS, including the seven
      `TestPlanTransitions` subtests.

- [x] **Step 9: Commit** — driverkit in `e4c9f6c`, the CRD on its own in `90379ec`.

**Shipped beyond the plan:**

- **`publish` split into `publish` + `statusPatch`, and `synced` is a real
  parameter.** The plan described the failure-path handling ("adjust the
  signature as needed") instead of writing it, and flagged that as a soft spot.
  It was one: the shape that works is a pure `statusPatch(generation, st,
  observed, synced, ready, reason, msg) []byte` split from the `Patch` call, for
  the same reason `plan` is a function. A failed verb reporting `Synced=True`
  would be a lie, and a lie no test could catch is one that ships. Covered by
  `TestStatusPatchSplitsSyncedFromReady`, `TestStatusPatchKeepsDriverFields`,
  `TestReconcileCreateFailureIsNotSynced`, `TestReconcileStopFailureIsNotSynced`,
  `TestReconcileConvergedStoppedIsSyncedNotReady`,
  `TestReconcileConvergedRunningIsSyncedAndReady`.
- **`boolCondition`, because Kubernetes conditions are tri-state strings.**
  `"true"` lowercase is not one of the three, and `strconv.FormatBool` produces
  exactly that.
- **`TestPlanNeverAsksForBothVerbs`** — an invariant the table cannot state,
  since a `plan` returning `(true, true)` would satisfy every row above and then
  create-then-stop in one tick.
- **A `Synced` printer column, which D3 did not ask for.** Without it the
  condition split buys nothing at the only place anyone looks: a machine stopped
  on purpose and a machine whose `Create` just failed both print
  `Power=Stopped Ready=False` — byte-identical rows, one of them an incident.
- **`TestReconcileFailedDestroyKeepsFinalizer`** and
  `TestReconcileObserveFailurePublishesNothing` — the GC contract and the
  "publish nothing you did not observe" rule, neither of which had a test before.

---

### Task 5: The `stop` CLI verb and docs — DONE (`4a80511`, `02f7cb3`)

**Files:** `cmd/tinq/main.go`, `README.md`.

> **The original Task 5 was wrong and is corrected here.** It described adding
> `flag.String("stop", ...)` to `main()` and dispatching on `*stopF != ""`. The
> CLI is cobra (`e1b28cd`, upstream): verbs are subcommands, `main()` has no
> flag dispatch to add to, and `tinq -stop machine.yaml` is a usage error. What
> shipped is a `*cobra.Command`.

- [x] **Step 1: Write the failing check**

```bash
go build -o /tmp/tinq ./cmd/tinq && /tmp/tinq stop --help
```
Expected before: `unknown command "stop" for "tinq"`, exit 1.

- [x] **Step 2: Add the subcommand**

```go
stop := &cobra.Command{
    Use:   "stop <machine.yaml>",
    Short: "Halt the VM but KEEP its disks, then exit",
    Long:  "A shutdown, not a teardown. ...",
    Args:  cobra.ExactArgs(1),
    RunE:  runVerb("stop"),
}
```

`runVerb` is the shared closure every standalone verb uses, so `stop` runs the
identical `Observe`/`Create`/`Stop`/`Destroy` the controller loop runs. Two ways
to build or halt a machine would drift.

- [x] **Step 3: Handle the verb in `standalone`**

```go
case "stop":
    if state == driverkit.Absent  { log.Printf("nothing to stop: %s", d.dir(m)); return nil }
    if state == driverkit.Stopped { log.Printf("already stopped: %s", d.dir(m)); return nil }
    return d.Stop(ctx, m)
```

Both early returns exist so a re-run is *quiet*, not merely harmless. `Stop`
already re-Observes and returns nil for anything that is not `Running`, so
without them the operator gets no word on why nothing happened — and "nothing to
stop" and "already stopped" are different facts. The first means no disks exist;
the second means they do, and survive.

- [x] **Step 4: Run the check** — `tinq stop --help` prints, exit 0. Covered by
      `TestStandaloneStopHaltsAndDoesNotFallThroughToCreate`, which is the
      regression that matters: `stop` sharing a switch with the default
      create-path is one missing `case` away from starting the machine it was
      asked to halt.

- [x] **Step 5: Document it in the README**

Verbs, not flags, and that is what makes the pair safe: `tinq stop a.yaml` and
`tinq destroy a.yaml` cannot be confused by a dropped character the way `-stop`
and `-destroy` could.

- [x] **Step 6: Full verification** — build, vet, test, `gofmt -l` clean.

- [x] **Step 7: Commit** — code in `4a80511`, README in `02f7cb3`.

**Shipped beyond the plan:** the README also documents **where `spec.powerState`
is ignored**. The standalone verbs never read it, so a manifest carrying
`powerState: Stopped` boots anyway under `tinq apply` or `tinq up` — a valid
value, silently ignored. That is deliberate (bootstrap runs before any control
plane holds desired state) and it is user-visible, so it is stated in the README,
in the CRD description and on `standalone` itself rather than left to be
discovered.

---

### Task 6: The graceful rung of `Stop` — DONE (`de79fdc`)

> **Not in the original plan.** It was deferred to a branch that no longer
> exists. `cluster.AuthenticatedClient` is present in this tree, so the stub
> that always returned "not implemented" had no reason to survive, and while it
> did **every stop was a power cut**.

**Files:** `cmd/tinq/main.go`, `cmd/tinq/main_test.go`, `README.md`.

- [x] **Step 1: Write the failing tests — the paths that ARE testable**

Nothing in CI can answer a Talos `Shutdown`, so what is asserted is every
failure path and the secret-handling rule:

```go
func TestShutdownGuestFailsFastOnAMachineWithNoTalosconfig(t *testing.T)
func TestShutdownGuestNeverQuotesTheTalosconfig(t *testing.T)
func TestStopFallsBackToSignalsWhenTheGuestCannotBeAsked(t *testing.T)
```

- [x] **Step 2: Implement `shutdownGuest`**

Read `<stateDir>/talosconfig` → `cluster.AuthenticatedClient(ctx, talosconfig,
talosEndpoint(m))` → `c.Shutdown(ctx)` → close.

`MachineService.Shutdown`, exposed on the client — **not** `LifecycleService`,
which carries only `Install` and `Upgrade`. It returns once the node has
*accepted* the request; Talos runs the shutdown sequence after replying. So nil
means "asked", not "gone", and `Stop` is right to wait on the process afterwards
rather than trust it.

- [x] **Step 3: Bound the request, separately from the power-off**

```go
shutdownRequestTimeout = 15 * time.Second
```

`ctx` reaches `Stop` unbounded (cobra's, or the controller loop's), and a gRPC
call is only fail-fast once the channel reports `TRANSIENT_FAILURE`. A guest
whose apid accepts the TCP connection and then never completes the TLS handshake
leaves the channel `CONNECTING` forever, and the call blocks with it — so
`tinq stop` on a half-wedged node would hang instead of falling through to the
signal ladder. **A wedged guest must still be stoppable**; that is the failure
this bounds. It is the budget for the *request*, not for the power-off, which is
`gracefulStopTimeout` and is waited out against the process.

- [x] **Step 4: The talosconfig is a SECRET**

Never logged, never interpolated into an error.
`TestShutdownGuestNeverQuotesTheTalosconfig` writes a recognisable sentinel into
the file and asserts it appears in no error string. `os.ReadFile`'s own error
quotes the *path* and never the contents, which is the only reason wrapping it is
safe — and the comment says so, so nothing below relaxes it.

- [x] **Step 5: Maintenance mode is refused without dialling**

A machine created by `apply` and never bootstrapped has no talosconfig, so this
returns immediately rather than spending the request budget discovering it cannot
authenticate. Safe rather than a compromise: a maintenance node is a booted ISO
with no applied config and nothing persistent to corrupt.

- [x] **Step 6: Say in the README that the clean path is not proven**

At the time: only the failure paths were tested, so the clean power-off was
expected-to-work rather than proven, and the README said so rather than claiming
otherwise. **That bullet is now stale** — see "Outcome" below.

- [x] **Step 7: Commit**

---

### Task 7: `up` idempotency and bootstrap tolerance — DONE (`37b4aa2`, `0a7c121`, `277466a`)

> **Not in the original plan** (D8, D9). Deferred to a branch that no longer
> exists. `tinq stop` keeps a machine's disks, so the next command an operator
> types is `tinq up` — and it could not work: the node boots the system it
> INSTALLED, never re-enters maintenance mode, and step 5 spent its whole
> five-minute budget proving that before failing.

**Files:** `cluster/up.go`, `cluster/up_test.go`, `cmd/tinq/main.go`, `README.md`.

- [x] **Step 1: Skip 5–7 when the machine already has a talosconfig**

Read `<stateDir>/talosconfig`. Present ⇒ announce steps 5, 6 and 7 as skipped,
each with its reason, then wait for the **authenticated** API
(`WaitBootstrapReady`, a thin alias for `WaitAPI`) instead of the maintenance
one.

**The file is a CREDENTIAL, not a status**, and that distinction is the whole
reason this read does not break the rule `Observe` obeys. Nothing about the node
is believed, and nothing *can* be: an authenticated call is impossible without
this file, so having it is a precondition of asking, and the claim still comes
from whether the node completes the mutual TLS handshake. A node in maintenance
mode cannot complete it, so a talosconfig sitting beside a node that never took
its config **fails the wait rather than being believed**.

Regenerating instead would be worse than slow: it mints a fresh secrets bundle
whose CA is not the one this node was installed with, so the new talosconfig
could not authenticate to it — and overwriting the old one takes away the only
way back in.

- [x] **Step 2: Keep the step numbering**

Skipped steps are announced under their own numbers. A run that jumped from
`[ 4/10]` to `[ 8/10]` would read as a bring-up that lost three steps rather than
one that had already passed them; renumbering to close the gap is worse, because
then `[ 5/10]` has two meanings and nothing tells them apart.

- [x] **Step 3: Attempt the bootstrap, tolerate the refusal (D9)**

```go
switch err := hooks.bootstrap(ctx, talosconfig, opts.TalosEndpoint); {
case err == nil:              // etcd bootstrapped
case alreadyBootstrapped(err): // the node refused a second one
default:                      return fail(err)
}
```

There is a window — config applied, node rebooted, etcd not yet bootstrapped —
where the authenticated wait **succeeds** and etcd does not exist. Probing the
wait's answer instead of asking the node would wait forever in step 9 for a node
that can never report Ready.

- [x] **Step 4: Pin the matcher to the gRPC CODE, from vendored source**

Talos v1.13.7,
`internal/app/machined/internal/server/v1alpha1/v1alpha1_server.go:457`:

```go
if entries, _ := os.ReadDir(constants.EtcdDataPath); len(entries) > 0 {
    return nil, status.Error(codes.AlreadyExists, "etcd data directory is not empty")
}
```

That is the only `AlreadyExists` the `Bootstrap` RPC can return. The code is part
of the API contract and the sentence is not, so matching the sentence is a
bring-up that breaks on an upstream rewording — and breaks by *swallowing a real
failure* or by *failing a healthy cluster*, neither of which the transcript could
explain.

`client.StatusCode`, not grpc's `status.Code`, because it **unwraps**: the error
arrives through `bootstrapEtcd`'s `%w`, and machinery wraps multi-node replies in
a multierror that `status.Code` reads as `Unknown`.

This closes open question 1 from the design spec, which said the error "must be
verified against machinery v1.13.7, not assumed."

- [x] **Step 5: Step 10 RE-RUNS rather than being skipped**

`InstallStorage` is a server-side apply of a pinned manifest, so it converges on
the same objects instead of failing `AlreadyExists`. Skipping it would leave a
machine stopped between steps 9 and 10 with no StorageClass forever, and a PVC
stuck `Pending` as the only symptom.

- [x] **Step 6: `up` must NOT adopt a Stopped VM**

The `Boot` closure tests `state == driverkit.Running`, not `state != Absent`. A
stopped machine has disks and no process, so there is no pid to adopt — `status`
is the `{stateDir}` map, `toInt(nil)` is 0, and `cluster.Up` would report a VM
whose process does not exist and then wait out the whole maintenance budget
against an address nothing is listening on. **Widening that test is a hang, not
a misprint.** Covered by `TestUpOptionsAdoptsAnAlreadyRunningVM` and
`TestUpOptionsDoesNotAdoptAStoppedVM`.

- [x] **Step 7: Correct every doc that said otherwise**

The README said a stopped machine starts again under `apply` — which leaves a
booted VM, not a cluster — and that `up` is not resumable, which stopped being
true. `stop`'s own cobra help said the same thing. Both fixed in `0a7c121`. The
post-bring-up summary still told the operator to run `-destroy` and `-up`, both
usage errors since the cobra conversion; fixed in `277466a`.

- [x] **Step 8: Commit**

**Idempotent is not resumable, and the difference is written into the error.**
Step 6 writes the talosconfig *before* step 7 applies it, deliberately, so the
artifacts that explain a failed apply survive it. A config written to the state
dir but never accepted by the node therefore leaves the two disagreeing — the
file says configured, the node is still in maintenance mode — and no wait on this
side can end. **That one a retry cannot repair**; it is `tinq destroy` and try
again, which the failure message says in full.

---

### Task 8: A cancelled stop stops waiting — DONE (`72e42ef`)

> **Not in the original plan, and it is a defect the plan introduced.** The
> plan's `waitGone` took no `ctx` and polled with a bare `time.Sleep`.
> `driverkit.Run` only looks at `ctx` **between** reconcile ticks, never during
> one, so a Ctrl-C landing mid-stop was ignored for up to **~85s** (15s shutdown
> RPC + 60s graceful + 5s SIGTERM + 5s SIGKILL) with no output to say why.

**Files:** `cmd/tinq/main.go`, `cmd/tinq/main_test.go`, `driverkit/driverkit.go`.

- [x] **Step 1: `waitGone` takes a ctx and selects on `ctx.Done()`**

A sleeping wait is an uninterruptible one. The poll is now a `select` on
`ctx.Done()` and a `time.Ticker` — a ticker rather than a per-iteration timer,
because the loop can run 600 times across the graceful budget and there is
nothing to leak on the way out. Cancellation is observed within one poll
interval (100ms).

- [x] **Step 2: `waitGone` returns `(gone bool, err error)` — three outcomes,
      kept distinct**

| result | meaning | may escalate? |
|---|---|---|
| `(true, nil)` | gone | — |
| `(false, nil)` | deadline passed and it is STILL ours | **yes, only this one** |
| `(false, err)` | the wait was ABANDONED, having learned nothing | no |

Collapsing the third into either of the others is the bug this signature exists
to prevent: read as "gone" it reports a success that did not happen; read as
"still running" it escalates to SIGKILL on a Ctrl-C, and **Ctrl-C means "stop
what you are doing", not "kill it harder"**.

- [x] **Step 3: A cancelled ladder stops where it stands**

It never escalates and it never returns nil. `Stop` and `destroy` **name** the
abandoned wait rather than returning a generic error — the difference between an
operator who re-runs the stop and one who believes the machine is down.

- [x] **Step 4: A cancelled teardown must NOT sweep**

`destroy` leaves the state dir when `halt` was abandoned. The QEMU is probably
still live and still has `system.qcow2` open; deleting the state dir out from
under it is not a teardown, it is corruption with a running writer. Blocking is
safe precisely because `destroy` is idempotent: the next delete tick retries, and
until then the finalizer holds, which `driverkit`'s `reconcile` already calls the
correct outcome.

A cancel arriving between `halt`'s return and that check reads as cancellation
for an error that was really "survived SIGKILL". That costs one retry of an
idempotent sweep; the reverse mistake costs a disk.

- [x] **Step 5: The first SIGTERM is NOT gated on ctx**

Cancellation cuts the *waiting* short, which is what took ~85s. The signal itself
is instant, already gated on `ProcessMatches`, and skipping it would mean an
interrupted `destroy` left a QEMU running that it had not even asked to exit.

- [x] **Step 6: An already-gone machine still destroys on a dead context**

`halt`'s pid/`ProcessMatches` gate returns before any wait, so teardown still
needs neither a live hypervisor nor a reachable node — the constraint
`cluster-bringup` set and this must not break.

- [x] **Step 7: Tests**

```go
func TestHaltAbandonsTheWaitWhenTheContextIsCancelled(t *testing.T)
func TestDestroyDoesNotSweepStateWhenTheContextIsCancelled(t *testing.T)
func TestDestroyOfAnAlreadyGoneMachineSucceedsOnACancelledContext(t *testing.T)
```

- [x] **Step 8: Record what is deliberately NOT fixed**

Reconciliation is **serial**: one machine that is stopping holds the loop for up
to ~85s and every other machine waits it out. Knowingly not fixed, and the
reasoning is on `driverkit.Run` itself (`driverkit/driverkit.go:113-135`) rather
than restated here:

- There is one machine. Multi-node is blocked on QEMU user-mode networking and
  is a stated non-goal.
- **The fix is not `go reconcile(...)`.** Concurrent reconciliation needs
  per-key locking to guarantee one machine is never reconciled twice at once.
  Without it, two ticks overlap and two QEMU processes run against a single state
  dir — which the driver's own `Boot` closure calls corrupting the disk they
  share. A stall is recoverable; that is not.

The trigger is a **condition, not a date**: revisit when a second machine can
exist. Cancellation is not part of that deferral and is fixed here — the ~85s is
the stall one machine imposes on another, not the delay an operator sees.

- [x] **Step 9: Commit**

---

## Outcome

**Verified on real hardware (Linux/KVM, Talos v1.13.7 amd64):**

- **`tinq stop` asked the guest and it powered itself off in 36s** — inside the
  60s graceful budget, so the signal ladder was never reached. That is Task 6's
  clean path executing against a real Talos node, which is exactly what
  `de79fdc`'s README bullet said was not yet proven.
- **`tinq stop` then `tinq up` resumed a bootstrapped node in 1m14s.** The
  transcript printed steps 5, 6 and 7 as skipped with their reasons and step 8 as
  "already bootstrapped (the node refused a second one)" — D9's
  `codes.AlreadyExists` path, executing against a real etcd data directory
  rather than against a hook seam.

**Verified by test, not by hardware:**

- The `Absent → Running → Stopped` convergence, the transition table and the
  `Synced`/`Ready` matrix. `driverkit` runs against a fake `Driver`, so what is
  proven is the state machine.
- The SIGTERM → SIGKILL escalation and every `ProcessMatches` gate, against a
  decoy process that ignores SIGTERM. No real stop has needed the ladder, because
  the one real stop answered gracefully.
- Cancellation, `shutdownGuest`'s failure paths, and the secret-handling rule.

**Not verified, and it stays written down:**

- **macOS has never run any of this.** `psCmdline` is called directly by a test
  on Linux, which proves the parsing and the `-ww` flag and proves nothing about
  `ps` on darwin. The `runtime.GOOS` dispatch that selects it has never executed
  on darwin. Inherited unverified from the platform-abstraction work, and now
  load-bearing for `Stop` and `destroy` rather than only for `Observe`.
- **The write-talosconfig-before-apply window.** A config written to the state
  dir but never accepted by the node leaves the file and the node disagreeing,
  and no wait on this side can end. **A retry cannot repair it** — `tinq destroy`
  and try again, which the error says. That path has not been reproduced
  deliberately.

**Stale elsewhere as a result:** `README.md`'s "Not done yet" bullets still say
the graceful rung has never met a real guest and that the `stop` → `up` round
trip has never been run end to end. Both predate the runs above. Correcting them
is a separate change to a separate file.

## Self-Review

**Spec coverage.** D1 → Tasks 2, 3. D2 → Task 4. D3 → Task 4 (`statusPatch`,
plus the `Synced` column). D4 → Task 2. D5 → Task 1. D6 → Tasks 3 and 6, **in
full**, graceful rung included. D7 → Task 3 Step 5. D8 → Task 7. D9 → Task 7
Steps 3–4. D10 → Task 4 Step 6. Testing section → Tasks 1, 2, 4. Nothing in the
design is unimplemented.

**Where the implementation departed from this plan, and why:**

| Plan said | Shipped | Why |
|---|---|---|
| `-stop` flag | `tinq stop` subcommand | the CLI is cobra (`e1b28cd`) |
| `ProcessMatches(pid, token)`, substring | `(pid, dir)`, path-boundary | prefix collision SIGKILLs another machine's VM |
| `ps -o command=` | `ps -ww -o command=` | width truncation hides the token |
| ladder inline in `Stop` | shared `halt`, gated first signal | `destroy`'s copy had already diverged |
| `waitGone(pid, dir, d) bool` | `waitGone(ctx, ...) (bool, error)` | a sleeping wait ignores Ctrl-C for ~85s |
| `publish` "adjust as needed" | pure `statusPatch`, explicit `synced` | the soft spot this plan flagged, closed |
| `shutdownGuest` stub | real Talos `Shutdown` | `cluster/` is in this tree |
| D8/D9 deferred | shipped | same |
| separate `process_linux.go` / `process_other.go` | one `process.go` | build tags are forbidden by `platform`'s own rule |

**Type and name consistency, as built:** `driverkit.State` with
`Absent`/`Stopped`/`Running`; `platform.ProcessMatches(pid int, dir string) bool`;
`plan(observed State, desired string) (create, stop bool)`;
`desiredPowerState(m) string`;
`waitGone(ctx context.Context, pid int, dir string, d time.Duration) (bool, error)`;
`halt(ctx context.Context, pid int, dir string) error`;
`statusPatch(generation int64, st map[string]interface{}, observed State, synced, ready bool, reason, msg string) []byte`.

**Soft spots this plan flagged, resolved:**

- `publish`'s `Synced` handling on failure paths was described rather than
  written. Closed in Task 4 by splitting `statusPatch` out as a pure function
  with `synced` as an explicit parameter, and by four `TestReconcile*` cases.
- `processCmdline` picks between `/proc` and `ps` at runtime via `runtime.GOOS`,
  not by build tags, so both branches stay compiled and type-checked on every
  host. The `ps` branch is only *executed* on macOS, so its test drives
  `psCmdline` directly — which is what caught the `-ww` truncation.
- `TestProcessMatchesOurself` relies on the test binary's own path appearing in
  its argv. True for `go test`, and if a future toolchain changes that the test
  fails loudly rather than silently passing.
