## 1. Pre-merge coordination

- [x] 1.1 Confirm homelab `pull-seed-into-homelab` has extracted `seed/` (history-preserving `filter-repo`), or that `feat/seed` is preserved as the extraction source — DONE: homelab imported seed/ byte-identical (58/58 files, 28 role commits preserved) and committed (1ffe13e); `feat/seed` also remains fully intact here
- [x] 1.2 Review the `main...feat/seed` diff **outside** `seed/` (the tinq work to land): `cmd/tinq/{adopt,reconfigure,main}`, `cluster/up.go` + `registries_test.go`, `crd/talosmachine.yaml`, `driverkit/`, `platform/` — reviewed: 124 commits, ~20,724 insertions outside seed/ (cmd/tinq/{main,reconfigure}, crd/talosmachine.yaml, driverkit, platform, examples, plans/specs)
- [x] 1.3 Baseline: `go test ./...` on `feat/seed` (record current pass/fail) — `go build ./...` rc=0; `go test ./...` PASS (cluster, cmd/tinq 6.07s, driverkit, platform); baseline green

## 2. Merge to main

- [x] 2.1 Merge `feat/seed` → `main` (merge commit 35ebe2b, `--no-ff`; operator chose merge over rebase)
- [x] 2.2 `go test ./...` green on the merge result (adopt/reconfigure/up/registries/driverkit/platform) — build rc=0; cluster 56s, cmd/tinq 6.2s, driverkit, platform all PASS

## 3. Drop the seed (now homelab's) — DEFERRED (destructive; gated on homelab prod reconverge)

- [ ] 3.1 `git rm -r seed/` — DEFERRED: keep `seed/` intact until homelab `pull-seed-into-homelab` §7.3 (prod reconverge) proves the import behaviour-equivalent. Tree is already verified byte-identical, but the fallback must survive until prod is green
- [ ] 3.2 Move seed-only docs (`docs/superpowers/{plans,specs}/2026-08-07-seed-*`) to homelab / remove here; **keep** tinq's own plans (`machine-lifecycle`, `baremetal-foundation`, `static-network`) — DEFERRED with §3.1 (docs already landed in homelab via the import; removal here rides with the seed/ drop)
- [ ] 3.3 `go build ./... && go test ./...` still green with `seed/` removed (no tinq package depended on seed content) — DEFERRED with §3.1

## 4. Verify + integrate

- [x] 4.1 From `main`: `tinq adopt` / `reconfigure` and a `spec.registries.ca` config build and pass their tests — verified on merged main: `go test ./cmd/tinq -run 'Adopt|Reconfigure'` PASS, `go test ./cluster -run 'Registr|CA'` PASS, `caFile` present in `crd/talosmachine.yaml`
- [x] 4.2 Update homelab references that pointed at `feat/seed` to consume tinq from `main` (`../talos-in-qemu`) — N/A code-wise: homelab's only functional reference is `TINQ_DIR ?= $(abspath ../talos-in-qemu)` (branch-agnostic); the checkout now sits on `main` which carries the tinq work. No `../talos-in-qemu/seed` or `feat/seed` pin exists in homelab build/runtime
- [ ] 4.3 `openspec archive land-feat-seed-tinq-to-main` once verified — DEFERRED until §3 (seed drop) completes
