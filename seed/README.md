# The seed — a self-contained bootstrap stack

A self-contained stack that stands up **next to** a Talos/Kubernetes cluster and
provides everything the cluster needs to be built, and rebuilt, from scratch: an
OCI registry, git hosting, Python/Go/Rust package hosting, SSO, an internal CA,
DNS, bare-metal netboot, time, and durable backups.

It runs as **podman quadlets driven by Ansible** on one Linux host — deliberately
**not** Kubernetes, so it exists before, and independently of, any cluster it
builds. The same definition brings it up on a development VM or on a bare-metal
management node; a host is just an inventory entry.

- **Design:** `docs/superpowers/specs/2026-08-07-seed-bootstrap-stack-design.md`
- **Implementation plans:** `docs/superpowers/plans/2026-08-07-seed-phase-*`
- **Per-task build log + gotchas:** `.superpowers/sdd/progress.md`

## What runs

| Service | URL | Purpose |
|---|---|---|
| **zot** | `https://registry.lab` | OCI registry + pull-through cache (docker.io, ghcr.io) |
| **Gitea** | `https://git.lab` | git + PyPI / Go / Cargo / Helm package registries |
| **Authentik** | `https://auth.lab` | SSO / OIDC |
| **MinIO** | `https://s3.lab` | S3 store + restic backup target |
| **step-ca** | `ca.lab:9000` (host loopback) | internal CA — every service's TLS chains to it |
| **Caddy** | `:80` / `:443` | reverse proxy + ACME client (issues every cert from step-ca) |
| **dnsmasq** | host `:53/:67/:69` | DNS for `*.lab` + proxy-DHCP + TFTP (bare-metal netboot) |
| **chrony** | host `:123` | NTP for internet-less nodes |
| Postgres, Redis | internal only | datastores for Gitea + Authentik |

Only `:80` and `:443` are exposed on the host — **everything is reached through
Caddy over HTTPS by hostname** (`https://<svc>.lab`). Postgres, Redis and zot's
raw port are internal to the `seed` podman network.

## Current instance

The reference deployment is a throwaway libvirt VM:

- host `seed-dev`, `root@192.168.122.10` (Debian 13)
- reachable over SSH with the operator's default key
- pristine revert: `~/.local/share/seed-vm/golden-seed-dev.qcow2`

> This is a **development** VM. For anything durable, deploy the role to a
> persistent host (add it to `inventory/` — the LAN facts go in
> `host_vars/<host>.yml`, see `host_vars/seed-dev.yml`).

## Using the seed from elsewhere

Two things must be set up before anything works from a client machine.

### 1. Trust the internal CA

Every service serves a cert issued by the internal step-ca. Fetch its root and
trust it — **never use `-k`/insecure**, verified TLS is the whole point:

```sh
scp root@192.168.122.10:/etc/ssl/seed/root_ca.crt ./seed-root_ca.crt
# then: curl --cacert seed-root_ca.crt …, or install into the client trust store
```

### 2. Resolve `*.lab`

The names resolve differently depending on where you are — this is the most
common trip-up:

- **On the seed host:** already resolves via `/etc/hosts` → `127.0.0.1`.
- **From another machine/VM:** point DNS at the seed's dnsmasq, **or** add
  `/etc/hosts` lines (`192.168.122.10 registry.lab git.lab auth.lab s3.lab …`),
  **or** use `curl --resolve registry.lab:443:192.168.122.10`.
- **From a Talos node:** set the seed as the node's nameserver
  (`spec.baremetal.network.nameservers`) — that's how `registry.lab` resolves
  in-cluster.
- **From a container on the seed's `seed` podman network:** resolves
  automatically (aardvark-dns) to the peer container's IP.

### Credentials

Generated on first run, stored `0600/0644 root:root` under
`/var/lib/seed/secrets/` on the seed. Read them as root:

```sh
ssh root@192.168.122.10 cat /var/lib/seed/secrets/<name>
```

Names: `zot_password`, `gitea_admin_password`, `minio_root_password`,
`authentik_bootstrap_token`, `authentik_bootstrap_password`,
`gitea_oidc_client_secret` (plus internal `*_db_password`, `step_ca_password`,
`restic_password`).

## Per-service quick start

**Registry (zot)** — user `seed`. Pull is anonymous/open; push needs auth (so a
cluster rebuild never depends on the identity stack). Also a pull-through cache:
`registry.lab/library/<x>` and `registry.lab/<ghcr-path>` fetch-and-cache on
demand.

```sh
pw=$(ssh root@192.168.122.10 cat /var/lib/seed/secrets/zot_password)
podman login registry.lab -u seed -p "$pw"     # CA trusted + name resolving first
podman push registry.lab/<repo>:<tag>
podman pull registry.lab/<repo>:<tag>           # no login needed
```

**Git + packages (Gitea)** — admin `seedadmin`. Package registries at
`https://git.lab/api/packages/seedadmin/{pypi,go,cargo}` (+ Helm/OCI).

```sh
pw=$(ssh root@192.168.122.10 cat /var/lib/seed/secrets/gitea_admin_password)
git clone https://seedadmin:$pw@git.lab/seedadmin/<repo>
# PyPI: uv publish --publish-url https://git.lab/api/packages/seedadmin/pypi
#       (index-url .../pypi/simple/). Gitea serves /pypi/simple/<pkg>/,
#       NOT a root /simple/ index.
```

**SSO (Authentik)** — admin `akadmin`; API at `/api/v3/…` with
`Authorization: Bearer $(authentik_bootstrap_token)`.

**S3 (MinIO)** — access key `seed`, secret = `minio_root_password`. Buckets:
`seed-backups` (restic), `tofu-state` (opentofu). A store/backup target, **not**
a live backend for zot/Gitea.

## Pointing a Talos cluster at the seed

Use `tinq` with `spec.registries` carrying the **`ca`/`caFile`** field — verified
HTTPS against the seed's CA, not `http://` or `insecureSkipVerify`. Worked,
rehearsal-proven example: **`seed/examples/seed-adopt.yaml`**.

```yaml
  registries:
    - { host: ghcr.io,      endpoint: https://registry.lab, caFile: /path/to/seed-root_ca.crt }
    - { host: registry.lab, endpoint: https://registry.lab, caFile: /path/to/seed-root_ca.crt }
    # caFile is read on the host running tinq.
    # Leave registry.k8s.io un-mirrored — k8s control-plane images pull upstream.
```

Day-0, hands-free: a node netboots off the seed (dnsmasq proxy-DHCP + iPXE +
`http://boot.lab`) into Talos maintenance, then `tinq adopt <machine>.yaml`
drives it to Ready with the installer pulled from the seed's zot. This was
verified end-to-end with a QEMU PXE guest; the bare-metal (5900X) run is the one
remaining step.

## Operating the role

```sh
cd seed
ansible-playbook site.yml --limit seed-dev          # converge (idempotent: changed=0 on re-run)
ansible seed-dev -m script -a 'acceptance/phase0.sh'   # trust chain gate
ansible seed-dev -m script -a 'acceptance/phase1.sh'   # registry/packages/SSO gate
ansible seed-dev -m script -a 'acceptance/phase3.sh'   # backup restore drill
```

`acceptance/phase*.sh` are the end-to-end gates. `phase3.sh` is destructive by
design (wipes zot+gitea state, restores from restic, verifies) — it has a
recovery trap so a failed restore doesn't leave the host broken.

## Extending the seed

The role has firm patterns — follow them or things break:

- **New HTTPS vhost:** append its short-name to `seed_caddy_vhosts` in
  `roles/seed/defaults/main.yml` (that NetworkAlias is what lets step-ca reach
  Caddy to validate the ACME challenge), add a `conf.d/<x>.caddy` snippet that
  `notify: restart caddy`, and add `<x>.lab` to the `/etc/hosts` bridge.
- **New secret:** use the `secrets.yml` generate-once primitive. Single-consumer
  secrets `0600`; a secret read by more than one container is `0644 root:root`
  (the `0700` parent dir is the trust boundary).
- **New container:** rootful podman SYSTEM quadlet in `/etc/containers/systemd/`.
  If the image runs as a non-root user, host-chown its data dir to a named
  `seed_<svc>_uid` (or use `Volume=…:U` for small mounts); root-running images
  (zot, MinIO, Caddy) need nothing. When the quadlet template changes,
  `notify: restart <svc>` (a `daemon-reload` alone won't restart a running
  container).
- **Network-core daemons** (dnsmasq, chrony) are **host packages**, not quadlets
  — they need host networking and privileged UDP.
- Keep `/var/lib/seed` and `/var/lib/seed/secrets` at `0700` — the CA keys and
  secrets rely on the parent dir gating access. Public, regenerable boot
  artifacts live outside it (`/srv/tftp`).

## Known deferred items

Triaged non-blocking (detail in `.superpowers/sdd/progress.md`): container uids
collide with host system users (reallocate to a high range someday); a few
first-run bootstrap commands pass a secret through process argv; iPXE binaries
track boot.ipxe.org rolling builds. None affects normal use.
