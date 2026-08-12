## 1. Pre-merge coordination

- [x] 1.1 Confirm homelab `pull-seed-into-homelab` has extracted `seed/` (history-preserving `filter-repo`), or that `feat/seed` is preserved as the extraction source — DONE: homelab imported seed/ byte-identical (58/58 files, 28 role commits preserved) and committed (1ffe13e); `feat/seed` also remains fully intact here
- [x] 1.2 Review the `main...feat/seed` diff **outside** `seed/` (the tinq work to land): `cmd/tinq/{adopt,reconfigure,main}`, `cluster/up.go` + `registries_test.go`, `crd/talosmachine.yaml`, `driverkit/`, `platform/` — reviewed: 124 commits, ~20,724 insertions outside seed/ (cmd/tinq/{main,reconfigure}, crd/talosmachine.yaml, driverkit, platform, examples, plans/specs)
- [x] 1.3 Baseline: `go test ./...` on `feat/seed` (record current pass/fail) — `go build ./...` rc=0; `go test ./...` PASS (cluster, cmd/tinq 6.07s, driverkit, platform); baseline green

## 2. Merge to main

- [ ] 2.1 Merge `feat/seed` → `main` (merge commit)
- [ ] 2.2 `go test ./...` green on the merge result (adopt/reconfigure/up/registries/driverkit/platform)

## 3. Drop the seed (now homelab's)

- [ ] 3.1 `git rm -r seed/`
- [ ] 3.2 Move seed-only docs (`docs/superpowers/{plans,specs}/2026-08-07-seed-*`) to homelab / remove here; **keep** tinq's own plans (`machine-lifecycle`, `baremetal-foundation`, `static-network`)
- [ ] 3.3 `go build ./... && go test ./...` still green with `seed/` removed (no tinq package depended on seed content)

## 4. Verify + integrate

- [ ] 4.1 From `main`: `tinq adopt` / `reconfigure` and a `spec.registries.ca` config build and pass their tests
- [ ] 4.2 Update homelab references that pointed at `feat/seed` to consume tinq from `main` (`../talos-in-qemu`)
- [ ] 4.3 `openspec archive land-feat-seed-tinq-to-main` once verified
