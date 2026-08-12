## Why

`feat/seed` is a large, unmerged feature branch (~20k lines outside `seed/`) that carries the **core tinq functionality** — the `tinq adopt` and `reconfigure` commands, `spec.registries.ca` (trust a registry's private CA), driverkit/platform work, and CRD updates — none of which is in `main`. Critically, **`tinq adopt` — the command used in production to bring up the am4 Talos node — exists only on this branch.** Shipping features off an unmerged branch is fragile.

The `seed/` subtree of this branch is being extracted to the homelab repo (homelab change `pull-seed-into-homelab`), which removes the reason the branch stayed unmerged. This change lands the branch's tinq work in `main` and drops `seed/` (now homelab's).

## What Changes

- Extract/confirm `seed/` has been imported to homelab (via `pull-seed-into-homelab`), then **remove `seed/` from this repo**.
- **Merge the remaining `feat/seed` tinq work to `main`**: the `adopt` + `reconfigure` commands (`cmd/tinq/`), `spec.registries.ca` / caFile support (`cluster/up.go`, `crd/talosmachine.yaml`), and the driverkit/platform changes, plus the tinq-side plans/specs (machine-lifecycle, baremetal-foundation, static-network).
- Gate the merge on the existing test suites (adopt_test, registries_test, up_test, driverkit_test, platform_test) passing on the merge result.

Non-goals: no new tinq features; this is landing already-written, in-use work. The seed's *provisioning* is not part of this repo anymore (it lives in homelab).

## Capabilities

### New Capabilities
- `tinq-adopt`: the `tinq adopt` command (and `reconfigure`) — bring up / re-apply config to a Talos node that tinq did not create — available in `main`.
- `registry-ca-trust`: `spec.registries.ca` / `caFile` support so a Talos node trusts a registry served over a private CA (e.g. the seed's `registry.lab`) — available in `main`.

### Modified Capabilities
<!-- None. openspec/specs/ is empty (fresh init); all capabilities are new. -->

## Impact

- **`main` gains** the tinq adopt/reconfigure commands, registry-CA trust, driverkit/platform work, and CRD changes (~20k lines currently only on `feat/seed`).
- **`seed/` removed** from this repo (now `lib/ansible/roles/seed` etc. in homelab).
- **Cross-repo ordering**: coordinate with homelab `pull-seed-into-homelab` — the seed import (history-preserving `filter-repo` from `feat/seed`) should complete first so nothing is lost when `seed/` is dropped here.
- **Downstream**: homelab depends on this — its am4 Talos VM and the seed registry mirror (`caFile`) rely on `tinq adopt` + registry-CA being in a stable `main`, not a branch.
