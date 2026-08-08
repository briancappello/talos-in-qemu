# Seed Phase 0 — Host Pattern and Trust Root Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up the seed host pattern — Ansible driving podman **system** quadlets — and bring up `step-ca` and Caddy so every later service gets a real certificate from an internal CA the whole fleet trusts.

**Architecture:** One Ansible role (`seed/roles/seed`) renders systemd quadlet units into `/etc/containers/systemd/`; systemd supervises them. Services share a podman network named `seed` and reach each other by network alias (`ca.lab`). `step-ca` auto-initialises its root on first run; Caddy obtains per-host certificates from it over ACME. Names resolve via `/etc/hosts` and podman network aliases until dnsmasq supersedes both in Phase 2. Deploying to a dev VM versus a bare-metal node differs by one inventory line.

**Tech Stack:** Ansible (`ansible-core` ≥ 2.16, `ansible.builtin` only — no extra collections), podman ≥ 4.4 (Quadlet), `smallstep/step-ca`, `caddy`, a systemd + podman host (reference: Debian 13).

## Global Constraints

Every task's requirements implicitly include this section. Values are copied from `docs/superpowers/specs/2026-08-07-seed-bootstrap-stack-design.md`.

- **k8s-free.** No Kubernetes anywhere in the seed. Services are podman **system** (rootful) quadlets in `/etc/containers/systemd/` — a deliberate departure from chunk-0's rootless *user* unit, because the seed binds privileged ports (443/80 now; 53/67/69 in Phase 2) and writes the host trust store.
- **One definition, two hosts.** The same `seed/site.yml` runs against `seed-dev` (a cloud-init VM) and `seed-metal`. Tasks MUST be host-agnostic — no hostnames, IPs, or arch baked into a task.
- **State root:** everything persistent lives under `/var/lib/seed/{step-ca,caddy,secrets,...}`. Secrets are `0600`, owned `root:root`.
- **Generate-on-first-run, idempotent.** Secrets are minted on first run, never pre-seeded; a second `site.yml` converge reports **zero changes**. This is the acceptance floor for every task that touches state.
- **Domain:** flat `.lab` (`ca.lab`, `hello.lab`, later `registry.lab`, `git.lab`, …). Phase 0 resolves names via `/etc/hosts` + podman network aliases; dnsmasq replaces this in Phase 2.
- **Image pins:** every `Image=` is pinned to an exact tag in `roles/seed/defaults/main.yml`. No `:latest`.
- **Cleanroom:** no runtime dependency on the `homelab` repo. Patterns may be reproduced as fresh code; nothing is imported or shared.
- **Branch:** `feat/seed`. **Commits:** conventional lowercase subjects, stage by explicit path (never `git add -A`/`.`/`-a`), no `Co-Authored-By`, no AI attribution.
- **Verification is behavioural.** A task is done when its check passes against a running container, not when a file exists. The Phase gate is `seed/acceptance/phase0.sh` returning success over HTTPS with **no** `-k`.

## File Structure

Created in this phase (later phases extend, never restructure):

```
seed/
  site.yml                              # the playbook: applies roles/seed to the `seed` group
  ansible.cfg                           # inventory path, stdout callback
  inventory/
    hosts.yml                           # seed-dev, seed-metal -> group `seed`
    group_vars/
      seed.yml                          # seed_domain, seed_state_root, image pins (non-secret)
      vault.yml                         # ansible-vault; pinned/override secrets (may be empty)
  roles/seed/
    defaults/main.yml                   # versions, ports, paths (overridable)
    tasks/
      main.yml                          # include_tasks in order: podman, secrets, step_ca, caddy
      podman.yml                        # install podman, create the `seed` network quadlet
      secrets.yml                       # reusable: ensure_secret pattern
      step_ca.yml                       # render + start step-ca, capture root
      caddy.yml                         # render + start Caddy, ACME to step-ca
    templates/
      seed.network.j2                   # the shared podman network quadlet
      step-ca.container.j2
      caddy.container.j2
      Caddyfile.j2
    handlers/main.yml                   # systemctl daemon-reload
  acceptance/
    phase0.sh                           # the phase gate
```

Each responsibility is one file: provisioning primitives (`podman.yml`, `secrets.yml`) are separated from service bring-up (`step_ca.yml`, `caddy.yml`) so a later phase adds `zot.yml`/`gitea.yml` beside them without touching the primitives.

---

### Task 1: Role skeleton, inventory, and the state root

**Files:**
- Create: `seed/ansible.cfg`
- Create: `seed/site.yml`
- Create: `seed/inventory/hosts.yml`
- Create: `seed/inventory/group_vars/seed.yml`
- Create: `seed/roles/seed/defaults/main.yml`
- Create: `seed/roles/seed/tasks/main.yml`
- Create: `seed/roles/seed/handlers/main.yml`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: the `seed` inventory group; the `seed` role; variables `seed_domain` (`lab`), `seed_state_root` (`/var/lib/seed`); the pinned image variables `seed_stepca_image`, `seed_caddy_image`; a `reload systemd` handler. Every later task and phase applies `roles/seed` and reads these.

- [ ] **Step 1: Run the check before anything exists, to see it fail**

Run: `cd seed && ansible-playbook --syntax-check site.yml`
Expected: FAIL — `the playbook: site.yml could not be found`.

- [ ] **Step 2: Create `seed/ansible.cfg`**

```ini
[defaults]
inventory = inventory/hosts.yml
roles_path = roles
stdout_callback = yaml
host_key_checking = False
```

- [ ] **Step 3: Create `seed/site.yml`**

```yaml
---
- name: Seed bootstrap stack
  hosts: seed
  become: true
  roles:
    - seed
```

- [ ] **Step 4: Create `seed/inventory/hosts.yml`**

Addresses are environment-specific; the comments say what to set them to. Both hosts are plain Linux boxes (NOT Talos nodes).

```yaml
---
all:
  children:
    seed:
      hosts:
        seed-dev:
          ansible_host: 192.168.122.10   # a cloud-init VM on the workstation (libvirt NAT default range)
        seed-metal:
          ansible_host: 192.168.1.10     # the bare-metal management node
      vars:
        ansible_user: root
```

- [ ] **Step 5: Create `seed/inventory/group_vars/seed.yml`**

Env-specific overrides live here; the committed file is intentionally minimal — the role's `defaults/main.yml` carries the real values.

```yaml
---
# Override role defaults per environment here, e.g.:
# seed_domain: lab
```

- [ ] **Step 6: Create `seed/roles/seed/defaults/main.yml`**

```yaml
---
# Domain and paths
seed_domain: lab
seed_state_root: /var/lib/seed

# Container images — pinned, never :latest (Global Constraints)
seed_stepca_image: docker.io/smallstep/step-ca:0.28.1
seed_caddy_image: docker.io/library/caddy:2.10.2
```

- [ ] **Step 7: Create `seed/roles/seed/tasks/main.yml`**

Later tasks append `include_tasks` lines below the state-root block, in dependency order.

```yaml
---
- name: Assert required variables are set
  ansible.builtin.assert:
    that:
      - seed_domain is defined and (seed_domain | length > 0)
      - seed_state_root is defined and seed_state_root.startswith('/')
    fail_msg: "seed_domain and an absolute seed_state_root are required"

- name: Ensure the state root exists
  ansible.builtin.file:
    path: "{{ seed_state_root }}"
    state: directory
    owner: root
    group: root
    mode: "0700"
```

- [ ] **Step 8: Create `seed/roles/seed/handlers/main.yml`**

```yaml
---
- name: reload systemd
  ansible.builtin.systemd_service:
    daemon_reload: true
```

- [ ] **Step 9: Syntax-check passes**

Run: `cd seed && ansible-playbook --syntax-check site.yml`
Expected: PASS — `playbook: site.yml`.

- [ ] **Step 10: Converge against the dev host, then prove idempotence**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `ok`/`changed` for the state-root task; play recap `failed=0`.
Run it a second time.
Expected: `changed=0` — the idempotence floor from Global Constraints.

- [ ] **Step 11: Commit**

```bash
git add seed/ansible.cfg seed/site.yml seed/inventory seed/roles/seed/defaults seed/roles/seed/tasks seed/roles/seed/handlers
git commit -m "feat(seed): role skeleton, inventory, and the state root"
```

### Task 2: podman and the shared quadlet network

Proves the whole quadlet loop — render a unit into `/etc/containers/systemd/`, `daemon-reload`, systemd generates and runs it — on the simplest possible unit (a network) before any real service depends on it.

**Files:**
- Create: `seed/roles/seed/templates/seed.network.j2`
- Create: `seed/roles/seed/tasks/podman.yml`
- Modify: `seed/roles/seed/tasks/main.yml` (add the `podman.yml` include)

**Interfaces:**
- Consumes: `seed_state_root`, the `reload systemd` handler (Task 1).
- Produces: podman installed; a podman network named **`seed`** (from `/etc/containers/systemd/seed.network`). Every later service attaches with `Network=seed` and is reachable by other services under its `NetworkAlias`.

- [ ] **Step 1: See the goal unmet on the host**

Run (on the target, or `ansible seed-dev -m shell -a`): `podman network exists seed; echo rc=$?`
Expected: FAIL — `podman: command not found`, or `rc=1` (network absent).

- [ ] **Step 2: Create `seed/roles/seed/templates/seed.network.j2`**

`NetworkName=` is load-bearing: without it the network would be named `systemd-seed`, and every later `Network=seed` reference would miss.

```ini
[Unit]
Description=Seed shared container network

[Network]
NetworkName=seed
```

- [ ] **Step 3: Create `seed/roles/seed/tasks/podman.yml`**

`flush_handlers` forces the `daemon-reload` to happen *before* the start task on the run that first renders the unit — and only when the template actually changed, so an idempotent run does nothing.

```yaml
---
- name: Install podman
  ansible.builtin.package:
    name: podman
    state: present

- name: Ensure the Quadlet unit directory exists
  ansible.builtin.file:
    path: /etc/containers/systemd
    state: directory
    owner: root
    group: root
    mode: "0755"

- name: Render the shared podman network quadlet
  ansible.builtin.template:
    src: seed.network.j2
    dest: /etc/containers/systemd/seed.network
    owner: root
    group: root
    mode: "0644"
  notify: reload systemd

- name: Apply pending quadlet changes before starting units
  ansible.builtin.meta: flush_handlers

- name: Start the generated network unit
  ansible.builtin.systemd_service:
    name: seed-network.service
    state: started
```

- [ ] **Step 4: Wire the include into `seed/roles/seed/tasks/main.yml`**

Append below the state-root task:

```yaml
- name: Provision podman and the shared network
  ansible.builtin.include_tasks: podman.yml
```

- [ ] **Step 5: Converge**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed` on the podman install (first time) and the network render; `failed=0`.

- [ ] **Step 6: Verify the network exists**

Run (on the target): `podman network exists seed; echo rc=$?`
Expected: PASS — `rc=0`.

- [ ] **Step 7: Prove idempotence**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed=0` — no daemon-reload, no restart (Global Constraints floor).

- [ ] **Step 8: Commit**

```bash
git add seed/roles/seed/templates/seed.network.j2 seed/roles/seed/tasks/podman.yml seed/roles/seed/tasks/main.yml
git commit -m "feat(seed): podman and the shared quadlet network"
```

### Task 3: Generate-on-first-run secret machinery

The reusable primitive behind D9. `step-ca` (Task 4) is its first caller; every later phase (Postgres, Gitea, Authentik, zot htpasswd) reuses it unchanged. The shape is borrowed from homelab's `gitea_publish_pypi` token handling: check, create if absent, persist, never rewrite.

**Files:**
- Create: `seed/roles/seed/tasks/secrets.yml`
- Modify: `seed/roles/seed/tasks/main.yml` (call it once, for `step_ca_password`)

**Interfaces:**
- Consumes: `seed_state_root` (Task 1).
- Produces: an includable contract — `include_tasks: secrets.yml` with `secret_name` (required) and `secret_length` (optional, default 32) writes `{{ seed_state_root }}/secrets/{{ secret_name }}` at `0600`, generating random content only if absent. Also produces the concrete secret **`step_ca_password`**, consumed by Task 4.

- [ ] **Step 1: See the goal unmet**

Run (on the target): `test -f /var/lib/seed/secrets/step_ca_password; echo rc=$?`
Expected: FAIL — `rc=1`.

- [ ] **Step 2: Create `seed/roles/seed/tasks/secrets.yml`**

`no_log` keeps the value out of the transcript. The `when` guard is what makes it generate-once: a re-run skips the write entirely, so the secret is stable.

```yaml
---
# Ensure one named secret exists. Caller vars:
#   secret_name    (required) - file name under {{ seed_state_root }}/secrets/
#   secret_length  (optional) - default 32
- name: "secrets | ensure the secrets dir exists"
  ansible.builtin.file:
    path: "{{ seed_state_root }}/secrets"
    state: directory
    owner: root
    group: root
    mode: "0700"

- name: "secrets | stat {{ secret_name }}"
  ansible.builtin.stat:
    path: "{{ seed_state_root }}/secrets/{{ secret_name }}"
  register: _seed_secret_stat

- name: "secrets | generate {{ secret_name }} on first run"
  ansible.builtin.copy:
    content: "{{ lookup('ansible.builtin.password', '/dev/null', length=secret_length | default(32), chars=['ascii_letters', 'digits']) }}"
    dest: "{{ seed_state_root }}/secrets/{{ secret_name }}"
    owner: root
    group: root
    mode: "0600"
  when: not _seed_secret_stat.stat.exists
  no_log: true
```

- [ ] **Step 3: Call it for `step_ca_password` in `seed/roles/seed/tasks/main.yml`**

Append below the `podman.yml` include:

```yaml
- name: Ensure the step-ca CA password exists
  ansible.builtin.include_tasks: secrets.yml
  vars:
    secret_name: step_ca_password
```

- [ ] **Step 4: Converge**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed` on the generate task; `failed=0`.

- [ ] **Step 5: Verify the secret's presence and mode**

Run (on the target): `stat -c '%a %U' /var/lib/seed/secrets/step_ca_password && wc -c < /var/lib/seed/secrets/step_ca_password`
Expected: PASS — `600 root` and a byte count of `32`.

- [ ] **Step 6: Prove it is generate-once (stable) and idempotent**

Run (on the target): `sha256sum /var/lib/seed/secrets/step_ca_password` — note the hash.
Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed=0`, and the generate task shows `skipping`.
Run (on the target): `sha256sum /var/lib/seed/secrets/step_ca_password`
Expected: identical hash — the secret was not rewritten.

- [ ] **Step 7: Commit**

```bash
git add seed/roles/seed/tasks/secrets.yml seed/roles/seed/tasks/main.yml
git commit -m "feat(seed): generate-on-first-run secret machinery"
```

### Task 4: step-ca, the internal trust root

The `smallstep/step-ca` image auto-initialises its root and intermediate on first run when the volume is empty and `DOCKER_STEPCA_INIT_*` is set, and `DOCKER_STEPCA_INIT_ACME=true` enables the ACME provisioner Caddy needs in Task 6. The init password comes from Task 3's `step_ca_password`, mounted at a *separate* path (not over `/home/step`, which the image writes to) so the image can persist its own copy for unattended restarts.

**Files:**
- Create: `seed/roles/seed/templates/step-ca.container.j2`
- Create: `seed/roles/seed/tasks/step_ca.yml`
- Modify: `seed/roles/seed/tasks/main.yml` (include `step_ca.yml`)

**Interfaces:**
- Consumes: `seed_stepca_image`, `seed_domain`, `seed_state_root`, the `seed` network (Task 2), the `step_ca_password` secret (Task 3), the `reload systemd` handler.
- Produces: `step-ca.service` running; reachable in-network as **`ca.lab`** and on the host at `https://127.0.0.1:9000`; ACME directory at `https://ca.lab:9000/acme/acme/directory`; the **root certificate** at `{{ seed_state_root }}/step-ca/certs/root_ca.crt`. Consumed by Task 5 (host trust) and Task 6 (Caddy ACME).

> **SELinux note (applies to every `Volume=` from here on):** on SELinux hosts (Fedora/RHEL) append `:Z` to each `Volume=` line so podman relabels the mount. The reference host is Debian (no SELinux), so the templates below omit it; add it if you target Fedora.

- [ ] **Step 1: See the goal unmet**

Run (on the target): `systemctl is-active step-ca.service`
Expected: FAIL — `inactive` or `Unit step-ca.service could not be found`.

- [ ] **Step 2: Create `seed/roles/seed/templates/step-ca.container.j2`**

```ini
[Unit]
Description=step-ca internal CA (seed trust root)
After=network-online.target seed-network.service
Wants=network-online.target

[Container]
Image={{ seed_stepca_image }}
ContainerName=step-ca
Network=seed
NetworkAlias=ca.{{ seed_domain }}
Volume={{ seed_state_root }}/step-ca:/home/step
Volume={{ seed_state_root }}/secrets/step_ca_password:/run/seed_password:ro
Environment=DOCKER_STEPCA_INIT_NAME=Seed
Environment=DOCKER_STEPCA_INIT_DNS_NAMES=ca.{{ seed_domain }},localhost
Environment=DOCKER_STEPCA_INIT_ACME=true
Environment=DOCKER_STEPCA_INIT_PASSWORD_FILE=/run/seed_password
PublishPort=9000:9000

[Service]
Restart=on-failure
TimeoutStartSec=120

[Install]
WantedBy=default.target
```

- [ ] **Step 3: Create `seed/roles/seed/tasks/step_ca.yml`**

```yaml
---
- name: Ensure the step-ca state dir exists
  ansible.builtin.file:
    path: "{{ seed_state_root }}/step-ca"
    state: directory
    owner: root
    group: root
    mode: "0700"

- name: Render the step-ca quadlet
  ansible.builtin.template:
    src: step-ca.container.j2
    dest: /etc/containers/systemd/step-ca.container
    owner: root
    group: root
    mode: "0644"
  notify: reload systemd

- name: Apply pending quadlet changes
  ansible.builtin.meta: flush_handlers

- name: Start step-ca
  ansible.builtin.systemd_service:
    name: step-ca.service
    state: started

- name: Wait for the step-ca root certificate to be written
  ansible.builtin.wait_for:
    path: "{{ seed_state_root }}/step-ca/certs/root_ca.crt"
    timeout: 90
```

- [ ] **Step 4: Wire the include into `seed/roles/seed/tasks/main.yml`**

Append below the `step_ca_password` secret task:

```yaml
- name: Bring up step-ca (internal CA)
  ansible.builtin.include_tasks: step_ca.yml
```

- [ ] **Step 5: Converge**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed` on the quadlet render; the `wait_for` succeeds; `failed=0`.

- [ ] **Step 6: Verify the CA is healthy and the root exists**

Run (on the target):
```bash
systemctl is-active step-ca.service
test -f /var/lib/seed/step-ca/certs/root_ca.crt && echo root-present
curl -ks https://127.0.0.1:9000/health
curl -ks https://127.0.0.1:9000/acme/acme/directory | grep -o newAccount
```
Expected: `active`; `root-present`; `{"status":"ok"}`; `newAccount` (proves the ACME provisioner is live). `-k` is acceptable here — this probes step-ca's own leaf before trust is distributed; the no-`-k` proof is Tasks 5 and 6.

- [ ] **Step 7: Prove unattended restart (the password persists)**

Run (on the target): `systemctl restart step-ca.service && sleep 5 && systemctl is-active step-ca.service`
Expected: `active` — step-ca decrypts its keys without a prompt, so the init password was persisted into the volume.
*If this returns `failed` with a password prompt in `journalctl -u step-ca`, the pinned image tag does not auto-persist: add `Volume=.../secrets/step_ca_password:/home/step/secrets/password:ro` and set the CA's `password` file in `ca.json`. The primary path above is the documented behaviour for the pinned tag.*

- [ ] **Step 8: Prove idempotence**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed=0`.

- [ ] **Step 9: Commit**

```bash
git add seed/roles/seed/templates/step-ca.container.j2 seed/roles/seed/tasks/step_ca.yml seed/roles/seed/tasks/main.yml
git commit -m "feat(seed): step-ca, the internal trust root"
```

### Task 5: Distribute the step-ca root to the host trust store

Copy the root to a stable path (`/etc/ssl/seed/root_ca.crt`) that Caddy mounts and that Talos will later carry in `spec.registries.ca`, install it into the host CA bundle so the host trusts seed services with no `-k`, and add the Phase-0 `/etc/hosts` bridge so `*.lab` resolves before dnsmasq exists. Trust-store paths branch on OS family so the one definition works on a Debian *or* a Fedora host.

**Files:**
- Modify: `seed/roles/seed/tasks/step_ca.yml` (append export + trust + hosts tasks)
- Modify: `seed/roles/seed/handlers/main.yml` (add the CA-bundle refresh handlers)

**Interfaces:**
- Consumes: the root at `{{ seed_state_root }}/step-ca/certs/root_ca.crt` (Task 4); `seed_domain`.
- Produces: **`/etc/ssl/seed/root_ca.crt`** (the stable path Task 6 mounts into Caddy); the seed root installed in the host trust store; `/etc/hosts` entries mapping `ca.lab` and `hello.lab` to `127.0.0.1`.

- [ ] **Step 1: See the goal unmet**

Run (on the target): `curl -s https://ca.lab:9000/health`
Expected: FAIL — name does not resolve, or `curl: (60) SSL certificate problem: unable to get local issuer certificate`.

- [ ] **Step 2: Add the CA-bundle refresh handlers to `seed/roles/seed/handlers/main.yml`**

Append below the `reload systemd` handler:

```yaml
- name: update ca trust (debian)
  ansible.builtin.command: update-ca-certificates
  changed_when: true

- name: update ca trust (redhat)
  ansible.builtin.command: update-ca-trust extract
  changed_when: true
```

- [ ] **Step 3: Append export, trust, and hosts tasks to `seed/roles/seed/tasks/step_ca.yml`**

```yaml
- name: Ensure the seed cert export dir exists
  ansible.builtin.file:
    path: /etc/ssl/seed
    state: directory
    owner: root
    group: root
    mode: "0755"

- name: Export the step-ca root to a stable path
  ansible.builtin.copy:
    src: "{{ seed_state_root }}/step-ca/certs/root_ca.crt"
    dest: /etc/ssl/seed/root_ca.crt
    remote_src: true
    owner: root
    group: root
    mode: "0644"

- name: Install the step-ca root into the host trust store (Debian family)
  ansible.builtin.copy:
    src: /etc/ssl/seed/root_ca.crt
    dest: /usr/local/share/ca-certificates/seed-root.crt
    remote_src: true
    mode: "0644"
  when: ansible_os_family == "Debian"
  notify: update ca trust (debian)

- name: Install the step-ca root into the host trust store (RedHat family)
  ansible.builtin.copy:
    src: /etc/ssl/seed/root_ca.crt
    dest: /etc/pki/ca-trust/source/anchors/seed-root.crt
    remote_src: true
    mode: "0644"
  when: ansible_os_family == "RedHat"
  notify: update ca trust (redhat)

- name: Resolve seed service names locally (Phase-0 bridge; dnsmasq replaces this in Phase 2)
  ansible.builtin.blockinfile:
    path: /etc/hosts
    marker: "# {mark} SEED PHASE-0 NAMES"
    block: |
      127.0.0.1 ca.{{ seed_domain }} hello.{{ seed_domain }}
```

- [ ] **Step 4: Converge**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed` on the export, the trust install, and the CA-bundle handler; `failed=0`.

- [ ] **Step 5: Verify the host trusts the seed CA with no `-k`**

Run (on the target):
```bash
test -f /etc/ssl/seed/root_ca.crt && echo export-present
curl -s https://ca.lab:9000/health
```
Expected: PASS — `export-present`, then `{"status":"ok"}` with **no** `-k` and **no** `--cacert`: the name resolves via `/etc/hosts` and the chain validates against the system bundle.

- [ ] **Step 6: Prove idempotence**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed=0` — the copies match, so neither CA-bundle handler fires.

- [ ] **Step 7: Commit**

```bash
git add seed/roles/seed/tasks/step_ca.yml seed/roles/seed/handlers/main.yml
git commit -m "feat(seed): distribute the step-ca root to the host trust store"
```

### Task 6: Caddy, ACME against step-ca, and the Phase 0 gate

The proof of the whole phase: Caddy obtains a certificate for `hello.lab` from step-ca over ACME (trusting the root via `acme_ca_root`, reaching the CA by the `ca.lab` network alias), and a client trusting the seed root fetches it with **no `-k`**. The Caddyfile pattern here is what Phase 1 extends with `registry.lab`, `git.lab`, `auth.lab`.

**Files:**
- Create: `seed/roles/seed/templates/Caddyfile.j2`
- Create: `seed/roles/seed/templates/caddy.container.j2`
- Create: `seed/roles/seed/tasks/caddy.yml`
- Create: `seed/acceptance/phase0.sh`
- Modify: `seed/roles/seed/handlers/main.yml` (add `restart caddy`)
- Modify: `seed/roles/seed/tasks/main.yml` (include `caddy.yml`)

**Interfaces:**
- Consumes: `/etc/ssl/seed/root_ca.crt` (Task 5), the step-ca ACME directory and `ca.lab` alias (Task 4), the `seed` network (Task 2), `seed_caddy_image`, `seed_domain`, the `reload systemd` handler.
- Produces: `caddy.service` running; `https://hello.lab` served with a step-ca leaf; `seed/acceptance/phase0.sh` (the phase gate).

- [ ] **Step 1: See the goal unmet**

Run (on the target): `curl -s --cacert /etc/ssl/seed/root_ca.crt --resolve hello.lab:443:127.0.0.1 https://hello.lab/`
Expected: FAIL — `curl: (7) Failed to connect ... Connection refused`.

- [ ] **Step 2: Create `seed/roles/seed/templates/Caddyfile.j2`**

`acme_ca_root` is what lets Caddy trust step-ca's own TLS while talking ACME to it; `ca.{{ seed_domain }}` resolves through the podman network alias from Task 4.

```caddyfile
{
	acme_ca https://ca.{{ seed_domain }}:9000/acme/acme/directory
	acme_ca_root /etc/ssl/seed/root_ca.crt
	email seed@{{ seed_domain }}
}

hello.{{ seed_domain }} {
	respond "seed ok"
}
```

- [ ] **Step 3: Create `seed/roles/seed/templates/caddy.container.j2`**

```ini
[Unit]
Description=Caddy reverse proxy and ACME client (seed TLS)
After=network-online.target seed-network.service step-ca.service
Wants=network-online.target
Requires=step-ca.service

[Container]
Image={{ seed_caddy_image }}
ContainerName=caddy
Network=seed
Volume={{ seed_state_root }}/caddy/Caddyfile:/etc/caddy/Caddyfile:ro
Volume={{ seed_state_root }}/caddy/data:/data
Volume=/etc/ssl/seed/root_ca.crt:/etc/ssl/seed/root_ca.crt:ro
PublishPort=80:80
PublishPort=443:443

[Service]
Restart=on-failure

[Install]
WantedBy=default.target
```

- [ ] **Step 4: Create `seed/roles/seed/tasks/caddy.yml`**

```yaml
---
- name: Ensure Caddy state dirs exist
  ansible.builtin.file:
    path: "{{ item }}"
    state: directory
    owner: root
    group: root
    mode: "0755"
  loop:
    - "{{ seed_state_root }}/caddy"
    - "{{ seed_state_root }}/caddy/data"

- name: Render the Caddyfile
  ansible.builtin.template:
    src: Caddyfile.j2
    dest: "{{ seed_state_root }}/caddy/Caddyfile"
    owner: root
    group: root
    mode: "0644"
  notify: restart caddy

- name: Render the Caddy quadlet
  ansible.builtin.template:
    src: caddy.container.j2
    dest: /etc/containers/systemd/caddy.container
    owner: root
    group: root
    mode: "0644"
  notify: reload systemd

- name: Apply pending quadlet and Caddyfile changes
  ansible.builtin.meta: flush_handlers

- name: Start Caddy
  ansible.builtin.systemd_service:
    name: caddy.service
    state: started
```

- [ ] **Step 5: Add the `restart caddy` handler to `seed/roles/seed/handlers/main.yml`**

Append **after** `reload systemd` (handlers run in definition order, so the daemon-reload that generates `caddy.service` must come first):

```yaml
- name: restart caddy
  ansible.builtin.systemd_service:
    name: caddy.service
    state: restarted
```

- [ ] **Step 6: Wire the include into `seed/roles/seed/tasks/main.yml`**

Append below the step-ca include:

```yaml
- name: Bring up Caddy (TLS front door)
  ansible.builtin.include_tasks: caddy.yml
```

- [ ] **Step 7: Create `seed/acceptance/phase0.sh`**

`--retry-all-errors` absorbs the few-second window while Caddy completes the ACME order. The second check proves the leaf chains to step-ca, not to a public CA that happened to be trusted.

```bash
#!/usr/bin/env bash
# Phase 0 gate: the host trusts the seed CA, and Caddy serves a real cert for hello.lab.
set -euo pipefail

root=/etc/ssl/seed/root_ca.crt
host="hello.${SEED_DOMAIN:-lab}"

body=$(curl -fsS --retry 15 --retry-delay 2 --retry-all-errors \
         --cacert "$root" --resolve "${host}:443:127.0.0.1" "https://${host}/")
if [ "$body" != "seed ok" ]; then
  echo "FAIL: body was '${body}', expected 'seed ok'"
  exit 1
fi

issuer=$(echo | openssl s_client -connect 127.0.0.1:443 -servername "$host" \
           -CAfile "$root" 2>/dev/null | openssl x509 -noout -issuer)
case "$issuer" in
  *Seed*|*Intermediate*) : ;;
  *) echo "FAIL: unexpected issuer: ${issuer}"; exit 1 ;;
esac

echo "phase0 ok: '${body}' issued by [${issuer}]"
```

Make it executable: `chmod +x seed/acceptance/phase0.sh`

- [ ] **Step 8: Converge**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed` on the Caddyfile and quadlet renders; `caddy.service` started; `failed=0`.

- [ ] **Step 9: Run the phase gate**

Run: `cd seed && ansible seed-dev -m script -a 'acceptance/phase0.sh'`
Expected: PASS — `phase0 ok: 'seed ok' issued by [issuer=...Seed Intermediate CA...]`, rc 0.

- [ ] **Step 10: Prove idempotence**

Run: `cd seed && ansible-playbook site.yml --limit seed-dev`
Expected: `changed=0` — no daemon-reload, no Caddy restart.

- [ ] **Step 11: Commit**

```bash
git add seed/roles/seed/templates/Caddyfile.j2 seed/roles/seed/templates/caddy.container.j2 seed/roles/seed/tasks/caddy.yml seed/roles/seed/tasks/main.yml seed/roles/seed/handlers/main.yml seed/acceptance/phase0.sh
git commit -m "feat(seed): caddy fronting hello.lab with a step-ca cert (phase 0 gate)"
```

---

## Phase 0 Done — Definition of Done

- `ansible-playbook site.yml --limit seed-dev` converges and, run twice, reports `changed=0`.
- `seed/acceptance/phase0.sh` prints `phase0 ok` — a client trusting only the seed root fetches `https://hello.lab` with no `-k`, and the leaf chains to step-ca.
- The same play applies to `seed-metal` by changing `--limit` (portability is the same run against a second inventory host, per the spec's Testing section).
- Foundations now in place for Phase 1: the `seed` network, the quadlet render→reload→start pattern, `ensure_secret` (`secrets.yml`), the internal CA at `ca.lab`, the host trust store carrying the seed root, and Caddy ready to gain `registry.lab` / `git.lab` / `auth.lab` vhosts.

## Self-Review

- **Spec coverage (Phase 0 slice of the spec):** D1 quadlets/Ansible/k8s-free — Tasks 1,2. D4 step-ca + Caddy ACME + root distribution — Tasks 4,5,6. D9 generate-on-first-run secrets + `/var/lib/seed` layout — Task 3. Phase-0 `/etc/hosts` DNS bridge — Task 5. The `spec.registries` `ca` field (D4) is a **tinq** change deferred to Phase 1, where a Talos guest first consumes the CA; noted, not dropped.
- **Placeholder scan:** none. Every file has complete content; the one contingency note (Task 4, Step 7) is a labelled fallback with a concrete alternative, not a TODO.
- **Interface/type consistency:** the network name `seed`, the secret path `{{ seed_state_root }}/secrets/<name>`, the root path `/etc/ssl/seed/root_ca.crt`, the alias `ca.lab`, and the ACME directory URL are used identically everywhere they appear across Tasks 2–6.
