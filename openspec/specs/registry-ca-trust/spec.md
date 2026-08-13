# registry-ca-trust Specification

## Purpose
TBD - created by archiving change land-feat-seed-tinq-to-main. Update Purpose after archive.
## Requirements
### Requirement: Talos nodes trust a registry's private CA
`main` SHALL support `spec.registries[].ca` / `caFile` on a `TalosMachine`, so a node configured by tinq trusts a registry served over a private CA (e.g. the seed's `registry.lab`) via verified HTTPS — not `http://` and not insecure-skip-verify.

#### Scenario: Node pulls from a private-CA registry mirror
- **WHEN** a `TalosMachine` sets a `registries` mirror with a `caFile` pointing at the registry's CA
- **THEN** the generated Talos machine config carries that CA and the node pulls images from the mirror over verified TLS

#### Scenario: caFile is read on the tinq host
- **WHEN** `caFile` is a path on the machine running tinq
- **THEN** tinq reads and embeds the CA at config-generation time (the node never needs the file itself)

### Requirement: Registry-CA behavior is covered by tests
The merge SHALL retain the registries test coverage (`cluster/registries_test.go`).

#### Scenario: Registry-CA tests pass on the merge result
- **WHEN** the work is merged to `main`
- **THEN** `go test ./cluster/...` passes, including the registries CA cases

