# The seed: a bootstrap stack that brings Talos clusters up from scratch and outlives them

Date: 2026-08-07
Branch: `feat/seed` (not yet created)
Status: designed, not yet implemented.

## Goal

A **self-contained bootstrap stack**, provisioned from this repo, that stands up
next to a Talos/k8s cluster and provides everything the cluster needs to be
built — and rebuilt — from scratch: an OCI registry, git hosting, Python/Go/Rust
package hosting, an internal CA, DNS, netboot for bare metal, correct time, and
durable state.

It runs as a set of **podman quadlets** on one Linux host, driven by **Ansible**,
and the same definition brings it up on a development VM and on a bare-metal
management node. It is deliberately **not** Kubernetes: the seed's whole job is
to exist before, and independently of, any cluster it builds, so that a cluster
failure never takes down the thing you would use to rebuild it.

The outcome is `ansible-playbook -i inventory/<host> seed/site.yml` producing a
host from which `tinq up` (VMs) and `tinq adopt` (hardware) can bring clusters to
Ready with every artifact pulled from the seed, and from which those clusters can
be rebuilt after a total loss.

## Non-goals

- **Running the seed on Kubernetes.** A management/seed cluster (Cluster-API /
  Sidero / Omni shape) is the obvious alternative and is rejected here: it makes
  the recovery tool depend on the substrate it recovers. See D1.
- **Reusing the existing Proxmox/Ansible homelab.** That homelab already runs
  Gitea and Authentik in LXC, and the seed could point at them. It does not: the
  driver is ownership + recovery, and a seed that depends on the Proxmox host
  being up is neither self-contained nor independently recoverable. Patterns are
  borrowed (see Context); services are not shared. This is a cleanroom.
- **A VLAN / dedicated provisioning segment on day one.** Proxy-DHCP on the flat
  LAN is the default; the DHCP mode is a single knob so a VLAN is a later config
  change, not a redesign. See D5.
- **A self-hosted Talos Image Factory.** Assets are mirrored by a fetch script
  now; running `image-factory` is the documented upgrade path for when many
  custom extension combinations are needed. See D6.
- **`matchbox` / per-MAC provisioning profiles.** A static `boot.ipxe` boots one
  node profile. matchbox arrives when there are differing profiles to template.
- **Multi-node / multi-cluster topology.** The seed serves however many clusters
  tinq can build, but nothing here models cluster fabrics; that is tinq's gap,
  not the seed's.
- **arm64 netboot.** Netboot targets amd64 bare metal. Development VMs boot
  through tinq/qemu regardless of host arch and never PXE, so this is not a gap.

## Context: what exists, and what it costs

Verified by reading `homelab` at `8a66019` and `talos-in-qemu` at `8eafa93`.

Today the "container repository" is a single `distribution` v3.0.0 registry run
as a **systemd user unit on the workstation** (`homelab/k8s/registry/`), bound
`0.0.0.0:5000`, **no authentication**, its access gated only by a hand-written
firewalld rich-rule per node IP. Talos nodes reach it through `spec.registries`
mirrors — `10.0.2.2:5000` for QEMU guests via user-mode networking's host alias,
`192.168.1.165:5000` for the bare-metal node — over **plain HTTP**, because the
`http://` scheme on the endpoint is the only switch that makes containerd speak
cleartext to a registry with no certificate.

Three costs are already written down in that registry's own `config.yml` and in
tinq's README, and the seed exists to retire them:

1. **No trust anchor.** Every mirror is `http://` or would need
   `insecureSkipVerify`. There is no CA, so there is no third option.
2. **Access control is a per-host firewall rule that breaks when a lease moves.**
   `config.yml` says so explicitly, and refuses to hardcode the workstation's LAN
   address for the same reason.
3. **The registry is pinned to a laptop.** It is a systemd user unit on a
   developer workstation, which is also — per the operator — the box that both
   runs the development VMs and orchestrates the bare-metal installs.

The existing Proxmox homelab has already proven two patterns the seed reuses as
fresh code, not shared services:

- **Gitea as a package registry.** `homelab`'s `gitea_publish_pypi` role builds
  with `uv` and publishes to `gitea.<domain>/api/packages/<owner>/pypi`,
  idempotent, auto-provisioning a scoped token on first run. Gitea's package
  surface is the same for Go, Cargo, Helm and OCI — one tool, one API.
- **Authentik as OIDC**, with OAuth applications registered programmatically
  (`authentik/tasks/oauth_app_register.yml`).

## The reframe: most of "B and C" is one tool you already run

The request named "container registry, git hosting, python/go/rust package
hosting" as separate services. Gitea is natively all but one of them: git, an
OCI registry, PyPI, a Go module proxy, a Cargo registry, Helm charts. The seed
does **not** treat these as five builds. It treats them as one — with a single,
deliberate exception below.

## Decisions

### D1 — Packaging: podman quadlets driven by Ansible, k8s-free

Each service is a systemd `.container` quadlet; systemd supervises ordering,
restart and health. Ansible installs podman, renders the quadlets, seeds
first-run secrets, and verifies. The deployment target is an inventory host, so
**"development VM" and "bare-metal management node" differ by one inventory line**
and nothing else.

**Why not a Talos seed cluster.** tinq could build a single-node cluster and the
seed could run as k8s workloads — it would even dogfood the product. It is
rejected because the recovery driver forbids it: the seed's job is to bring
clusters up from nothing and to survive a cluster's death, and a seed that is
itself a cluster shares etcd, CNI and CSI failure modes with the thing it is
meant to rebuild. The registry that holds the images you need to reinstall
Kubernetes must not require Kubernetes to serve them.

**Why quadlets and not compose.** The workstation registry is already a systemd
unit; this generalises it rather than importing a second supervisor. Compose
drags in a runtime and, for Docker, a rootful daemon — the exact thing tinq
exists to avoid ("no root, no container runtime"). systemd gives ordering,
restart and health-gating natively, which is what a stack whose job is to come
back cleanly after a bad day actually needs.

### D2 — Self-contained: the seed owns its Gitea and Authentik

The seed runs its **own** Gitea and Authentik, not the Proxmox homelab's. The
cost is duplication of two services the operator already runs; the benefit is the
whole point of the exercise — a development VM stands up the entire stack with
the Proxmox host unplugged, and the bare-metal seed has no upstream it can lose.

### D3 — The image registry is split off Gitea, and it is `zot`

Gitea does everything except the one thing that must be simplest. The OCI
registry is the only component squarely on the cold-rebuild path: Talos pulls its
`installer` image and every workload image from it. So it is separated from
Gitea's Postgres-backed complexity and run as **`zot`** — a single Go binary,
file-backed, no database, that is also a **pull-through cache** for docker.io and
ghcr. One binary covers both "registry" and "upstream cache", and its restore is
copying a directory of blobs.

- **`zot`** — OCI registry + pull-through cache. Recovery-critical. File-backed.
- **Gitea** — git + PyPI + Go proxy + Cargo (+ Helm/OCI for convenience). Needed
  for development, not for bare-cluster recovery.

They are decoupled on purpose: a Gitea or Postgres outage must never stop an
image pull. This is the single most important structural decision in the design,
and D7 and D11 exist to keep it true.

### D4 — Trust root: `step-ca`, fronted by Caddy, distributed to Talos

`step-ca` is the first service up and the root of trust. Its root key is
generated on first run, persisted `0600`, and is the seed's crown jewel. It runs
an ACME provisioner; **Caddy** fronts every service and obtains and renews
certificates from step-ca automatically, which is why Caddy is chosen over the
homelab's nginx+certbot — short-lived internal certs renew themselves with near
zero configuration.

The step-ca **root certificate** is distributed three ways: to the service
containers, to the developer workstation, and to **Talos nodes**. The third is a
tinq change:

- **`spec.registries` gains a `ca` field.** Today a mirror entry is `{host,
  endpoint}` and the `http://` scheme is the cleartext switch. The seed serves
  `https://registry.lab`, so the entry gains an optional `ca` (PEM or path) that
  emits `machine.registries.config.<host>.tls.ca` in the generated machine
  config. This is the third option the current design lacks: not cleartext, not
  `insecureSkipVerify`, but a verified chain to the seed's own CA. It lands with
  unit tests on the emission, in `cluster/`'s existing config-generation suite,
  for both the `up` and `adopt` paths.

This retires cost #1 from Context.

### D5 — DNS and netboot: one `dnsmasq`, proxy-DHCP on the flat LAN

`dnsmasq` does double duty: it resolves `*.lab` to the seed host, and it serves
the boot plane. It runs in **proxy-DHCP** mode — it appends PXE boot information
(next-server, arch-matched boot file) to the LAN's existing DHCP, and does **not**
assign addresses. A second authoritative DHCP server on a home LAN fights the
router; proxy-DHCP coexists.

The netboot chain, for amd64 bare metal:

1. Node powers on, NIC sends DHCP/PXE.
2. dnsmasq proxy-DHCP answers with boot info only; the router still leases the
   address.
3. Arch-matched TFTP: BIOS clients get `undionly.kpxe`, UEFI clients `ipxe.efi`,
   tagged on DHCP option 93.
4. iPXE chainloads `http://boot.lab/boot.ipxe` over HTTP — TFTP chokes on large
   kernels.
5. The script boots the Talos `vmlinuz` + `initramfs` with `talos.platform=metal`
   and **no** `talos.config`, so the node lands in **maintenance mode**, takes a
   lease, and waits for `tinq adopt`.

**The mode is a single knob.** `dnsmasq`'s DHCP stanza is templated so
`proxy` (default) versus `authoritative` + a subnet is one variable. Promoting
the seed to own a VLAN later — for isolation of the adopt trust window (see
Risks), or segment-level firewalling that survives lease churn (cost #2) — is a
configuration change, not a redesign. The trigger to actually do it is named in
Risks; it is not day-one work.

### D6 — Two boot artifacts, two services; assets mirrored, not manufactured

The maintenance boot and the on-disk install pull different artifacts from
different services, and the split falls out of D3:

- **The asset server** (a Caddy vhost over `assets/`) serves the maintenance
  boot: the iPXE binaries, `boot.ipxe`, `vmlinuz`, `initramfs`.
- **`zot`** serves the `installer` image the node pulls **after** adopt applies
  config, to write Talos to disk.

Assets are obtained by a **fetch script** that `curl`s the specific
`vmlinuz`/`initramfs`/`installer` for a pinned Talos version from Sidero's factory
into `assets/` and `zot` — the same shape as the README's ISO fetch. A
self-hosted `image-factory` is the documented upgrade path, not the default,
because for a small number of node profiles a fetch script is a directory of
files and the factory is a service to run.

### D7 — Identity is for humans; the recovery path does not depend on it

Authentik provides SSO for the human-facing services — Gitea's UI, Caddy-guarded
admin endpoints. It needs Postgres **and Redis**, which makes it the heaviest
component in the stack; that weight is the honest cost of wanting real SSO.

**`zot` does not authenticate against Authentik.** It uses a local htpasswd:
push is authenticated, pull is open on the recovery path. If image pulls required
the SSO stack to be up, an Authentik or Redis outage would block a cluster
rebuild — re-coupling exactly what D3 separated. Identity is a convenience for
operators, never a dependency of the registry.

### D8 — Time: `chrony`, handed out by DHCP

The seed runs `chrony` so a node with no internet reaches correct time; etcd and
TLS both fail on skew, silently and late. dnsmasq advertises the seed as the NTP
server via DHCP option 42, and a node's `machine.time.servers` points at
`time.lab`.

### D9 — Secrets: generated on first run, persisted, idempotent

Secrets are **minted on first run, not pre-seeded**: the step-ca root, Postgres,
Gitea and Authentik credentials, `zot`'s htpasswd, OIDC client secrets. The role
follows the exact idempotency shape the homelab `gitea_publish_pypi` role already
uses for its token — check, create if absent, persist, reuse thereafter — so a
re-run changes nothing. Anything an operator wants to pin (a known admin
password) goes in a cleanroom `ansible-vault` file under `seed/inventory/`.

Persisted state lives under one root, which is what D11 backs up and what "restore
the seed" copies:

```
/var/lib/seed/{step-ca,postgres,redis,gitea,zot,authentik,caddy,minio,tftp,assets,secrets}
```

### D10 — A predictable adopt endpoint, and no change to adopt's contract

`dnsmasq` holds a **DHCP reservation by MAC**, so a known node always takes a
known address — that address is the `maintenanceEndpoint` in its machine file,
and it reuses the `hardwareAddr` identity tinq already models. Power on, then
`tinq adopt node.yaml`.

The boot plane is **Ansible and quadlet configuration only**. It changes nothing
in tinq's adopt path: adopt still drives an already-maintenance node at a known
address through the same ten steps. Netboot merely replaces "boot from USB" as
how the node reaches maintenance mode. The only tinq code change in this whole
design is D4's `ca` field.

### D11 — State and backup: MinIO is a target and a store, never zot's backend

Phase 3 is in the build, and it forces a decision that would otherwise quietly
undo D3. **MinIO does not become `zot`'s live storage backend.** If zot's blobs
lived in MinIO, zot would need MinIO up to serve a pull — re-coupling the
recovery hot path D3 exists to keep independent. Therefore:

- **`zot` and Gitea stay on local disk.** Hot path, dependency-free.
- **MinIO is a durable backup target** — `restic` snapshots the `/var/lib/seed`
  state dirs (Postgres via dump, not a live file copy) into a MinIO bucket — **and
  a native S3 store** for net-new consumers that want a bucket: opentofu state
  now, Velero and Loki when clusters exist.
- **The restic target is a knob.** Local MinIO is the default and the first tier.
  Backup to a MinIO on the **same host** protects against data corruption but not
  host loss, so it is not disaster recovery until the knob points off-host. The
  knob is how you get there; the honesty is that same-host backup alone is not DR.
- **The acceptance gate is a restore drill, not a backup job.** Snapshot, wipe a
  service's state, restore, confirm the service returns with its data. A backup
  that has never been restored is a hope.

## Architecture summary

```
                          +--------------------- seed host (VM or metal) ---------------------+
  Talos node  --DNS------>| dnsmasq   proxy-DHCP + TFTP + DNS (.lab)   [mode: knob -> VLAN]    |
  (netboot)   --TFTP/HTTP>| asset srv vmlinuz/initramfs/boot.ipxe (Caddy vhost)               |
              --pull----->| zot       OCI registry + pull-through cache (htpasswd, file-backed)|
              --time----->| chrony    NTP (DHCP option 42)                                     |
                          | step-ca   internal CA  --ACME-->  Caddy (TLS for all *.lab)        |
  operator    --https---->| Gitea     git + PyPI + Go + Cargo + Helm   --OIDC--> Authentik     |
                          | Postgres + Redis   (back Gitea + Authentik)                        |
                          | MinIO     restic backup target + S3 store (opentofu, future k8s)   |
                          +-------------------------------------------------------------------+
                                        restic --> MinIO (local now; off-host = knob)
```

Dependency order (systemd `After=` / health-gated):

```
step-ca  -> caddy
postgres, redis -> authentik -> gitea (OIDC)
dnsmasq, chrony   (independent)
zot               (fewest deps, by design)
minio             (independent); restic timer -> minio
```

## Phasing

Every phase ends at an end-to-end acceptance gate — user-visible behaviour, the
standard chunk-0 set: a Pod whose image exists nowhere else reaching Ready.

**Phase 0 — host pattern + trust root.** `seed/` skeleton (role, `inventory/`
with `seed-dev` and `seed-metal`, `site.yml`), podman + quadlet scaffolding,
step-ca (first-run root + distribution), Caddy (ACME to step-ca), the
generate-on-first-run secrets machinery.
Gate: `https://ca.lab` presents a chain a client trusting the root validates,
with no `-k`.

**Phase 1 — registry + source.** zot (registry + pull-through cache + htpasswd),
Postgres + Redis, Gitea (git + PyPI/Go/Cargo/Helm), Authentik + OAuth app
registration, and the tinq `spec.registries` `ca` field.
Gate: image push then pull **from a Talos guest** over https with the step-ca CA;
one round-trip per package type (`pip install`, `go get`, `cargo add`); SSO login
to Gitea via Authentik; a docker.io pull served through zot's cache.

**Phase 2 — boot plane.** dnsmasq (proxy-DHCP knob + TFTP + DNS), asset server +
Talos asset fetch script, static `boot.ipxe`, chrony + DHCP option 42.
Gate: a **qemu PXE guest** on a seed-served libvirt network boots to Talos
maintenance off the seed, then `tinq adopt` drives it to Ready with the installer
pulled from zot; then one 5900X hardware run confirms the real NIC.

**Phase 3 — state + backup.** MinIO quadlet, restic timer, the target knob,
opentofu-state bucket.
Gate: the restore drill — snapshot, wipe zot's and Gitea's state, restore from
MinIO, confirm both return with their data and a Talos guest can still pull.

## Testing

- **Idempotence.** The playbook converges twice; the second run reports zero
  changes. This is the smallest proof the role is declarative and not a script.
- **One-definition parity by construction.** The same `site.yml` and the same
  acceptance suite run against `seed-dev` (a cloud-init VM on the Meteor Lake
  box) and `seed-metal`. No separate proof of portability is needed — it is the
  same run against two inventory hosts.
- **Rehearse the boot plane in a VM before hardware.** A qemu guest set to PXE
  boot on a seed-served libvirt network exercises the entire chain —
  proxy-DHCP, TFTP, iPXE, Talos maintenance — unprivileged, so the only thing the
  5900X adds is a real NIC. This is the pattern tinq already uses for adopt.
- **Molecule is optional and later.** A CI matrix is a want, not a day-one build;
  the idempotence check plus the acceptance suite is the floor.

## Risks and unverified assumptions

1. **The adopt trust window still applies, and netboot widens who can answer.**
   tinq's maintenance-mode apply is `InsecureSkipVerify` by construction — before
   config lands there is no trust anchor — and it carries the cluster's CAs and
   machine token. Netboot means a node reaches maintenance mode automatically on
   whatever segment it is plugged into. The mitigation is operational and is
   exactly the VLAN knob in D5: isolate the provisioning segment when the shared
   LAN is not a segment you would hand the cluster's CA to. Recorded, not solved;
   solving it is upstream of this design.
2. **Proxy-DHCP relies on the client merging two offers.** A minority of NIC PXE
   ROMs race or ignore the proxy offer, producing intermittent "sometimes it
   netboots". The D5 knob to authoritative DHCP (on a VLAN) is the deterministic
   fallback if this bites.
3. **Authentik is the heaviest component and the least aligned with "simpler than
   what it bootstraps".** It is included because SSO was an explicit requirement;
   D7 contains the blast radius by keeping the registry off it. If the weight is
   not worth it, Gitea's built-in auth is the smaller fallback and Authentik
   becomes a later addition.
4. **Secrets and the step-ca root live on host disk until the off-host restic
   target is set.** Until D11's knob points off-host, a host loss loses the CA
   root and every credential. The restore drill proves recovery from a surviving
   backup; it does not manufacture an off-host one.
5. **`zot` pull-through caching of ghcr/docker.io is assumed sufficient for
   air-gap.** True air-gap needs every transitive image warmed into the cache
   first; the cache is lazy and only holds what has been pulled once. "From
   scratch with the internet unplugged" requires a prior warm-up pass, which is
   an operational step, not a service.
6. **amd64 only for netboot.** The bare-metal target is amd64; the design does
   not serve arm64 boot files. Adding them is additive in dnsmasq and the asset
   fetch script, with no arm64 hardware to prove it against today.

## Out of scope — the next branches

- **The VLAN provisioning segment.** The knob exists; the segment, its
  inter-VLAN routing and the seed's role on it do not. Risk 1 is its trigger.
- **`image-factory` and `matchbox`.** Mirrored assets and a static `boot.ipxe`
  first; both services arrive when profiles multiply.
- **An off-host / offsite restic provider.** The knob is built; a specific second
  target (a NAS, a cloud bucket, the other seed instance) is a configuration
  decision, not this design.
- **Serving multi-node cluster fabrics.** Bounded by tinq's own single-node
  limit, not the seed's.
