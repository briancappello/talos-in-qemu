## Context

`feat/seed` interleaves two bodies of work: the **seed** (`seed/`, an Ansible/podman provisioning stack) and the **tinq core** (`adopt`/`reconfigure` commands, `spec.registries.ca`, driverkit/platform, CRD, and tinq's own plans/specs). Only the seed kept the branch from merging; it is now moving to homelab (`pull-seed-into-homelab`). The tinq work is already used in production (`tinq adopt` brought up the am4 node) but lives only on this branch.

## Goals / Non-Goals

**Goals:**
- Land `tinq adopt`/`reconfigure` and `spec.registries.ca` in `main`, test-gated.
- Remove `seed/` from this repo (now homelab's).
- Coordinate with the homelab seed import so no history is lost.

**Non-Goals:**
- No new tinq features — this is landing already-written, in-use code.
- The seed's provisioning is out of this repo entirely.

## Decisions

- **Merge `feat/seed` → `main`, then drop `seed/` in a cleanup commit.** The branch's commits interleave seed and tinq work, so cherry-picking the tinq commits is fragile. Merging brings everything, then `git rm -r seed/` removes the tree. *Alternative rejected:* history-rewriting the branch to exclude seed commits before merge (heavy, error-prone for ~20k lines).
- **Accept seed commits remaining in `main` history; only the tree drops `seed/`.** homelab is authoritative for the seed going forward (it has the filter-repo'd history); the historical commits lingering here are harmless. *Alternative rejected:* filtering seed commits out of history (rewrite churn for cosmetic cleanliness).
- **Homelab seed import goes first (or from a preserved ref).** The homelab `filter-repo` reads `feat/seed`'s `seed/` history; keep `feat/seed` intact until that import is verified so nothing is lost when `seed/` is dropped here.
- **Gate on `go test ./...`.** The code is in use, but the merge to `main` must keep the adopt/reconfigure/up/registries/driverkit/platform suites green.

## Risks / Trade-offs

- **Landing ~20k lines could destabilize `main`** → it's already-written, in-use code; gate on `go test ./...` and review the non-seed diff before merge.
- **Dropping `seed/` before the homelab import loses it** → sequence the homelab `filter-repo` first; keep `feat/seed` until homelab's import is verified.
- **Interleaved commits** → merge-then-remove sidesteps cherry-pick fragility.
- **Docs split** → seed-specific plans/specs (`2026-08-07-seed-*`) move to homelab; tinq's own plans (`machine-lifecycle`, `baremetal-foundation`, `static-network`) stay here.

## Migration Plan

1. Confirm homelab `pull-seed-into-homelab` has extracted `seed/` (or `feat/seed` is preserved as the extraction source).
2. Review the `main...feat/seed` diff outside `seed/` (the tinq work to land).
3. Merge `feat/seed` → `main` (merge commit); `go test ./...` green.
4. Cleanup commit: `git rm -r seed/`; move seed-only docs to homelab, keep tinq's own; `go build/test` still green.
5. Verify `tinq adopt`/`reconfigure` + registries-CA build and test from `main`.
6. Update homelab references (`../talos-in-qemu`) to consume tinq from `main` rather than `feat/seed`.

**Rollback:** if `main` destabilizes, revert the merge commit; `feat/seed` remains intact.

## Open Questions

- Merge commit vs rebase `feat/seed` onto `main`? (Lean: merge — preserves branch history, no rewrite.)
- Any seed-side test/helper the tinq packages depend on that must stay when `seed/` is removed? (Verify with `go build/test` after the drop.)
