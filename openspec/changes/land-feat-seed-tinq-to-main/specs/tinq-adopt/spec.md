## ADDED Requirements

### Requirement: `tinq adopt` is available in main
`main` SHALL provide a `tinq adopt <machine.yaml>` command that takes a machine already booted into Talos maintenance mode (which tinq did not create) and drives it to a Ready single-node cluster, keyed by `spec.baremetal`.

#### Scenario: Adopt a maintenance-mode node
- **WHEN** `tinq adopt` runs against a `TalosMachine` with `spec.baremetal.maintenanceEndpoint` set and a valid `systemDiskSerial`
- **THEN** the node is configured, installed, bootstrapped, and reaches `Ready`, with kubeconfig/talosconfig written to the machine's state dir

#### Scenario: Refuses without a disk selector
- **WHEN** `tinq adopt` runs with no `systemDiskSerial`/`systemDiskWWID`
- **THEN** it prints the node's disks and refuses, rather than installing to an arbitrary disk

### Requirement: `tinq reconfigure` is available in main
`main` SHALL provide a `tinq reconfigure <machine.yaml>` command that regenerates a running machine's config from its manifest against the existing secrets bundle and applies it over the authenticated API.

#### Scenario: Apply an edited manifest to a running node
- **WHEN** `tinq reconfigure` runs against an already-configured machine with an edited manifest (e.g. an added registry mirror)
- **THEN** the new config is applied over the Talos API without minting new CAs or wiping disks

### Requirement: Landing preserves the branch's tinq test coverage
The merge to `main` SHALL keep the tinq test suites green (`adopt_test`, `reconfigure`-related, `up_test`, `main_test`).

#### Scenario: Tests pass on the merge result
- **WHEN** the tinq work is merged to `main`
- **THEN** `go test ./...` passes for the adopt/reconfigure/up/main packages
