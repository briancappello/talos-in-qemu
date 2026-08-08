# Seed Phase 3 — State and Backup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Durable state and a proven restore. MinIO gives the seed an S3 store (a restic backup target now; opentofu/Velero buckets later); restic snapshots the seed's state into it on a timer; and a **restore drill** proves the snapshots actually come back — because a backup that has never been restored is a hope, not a backup.

**Architecture:** MinIO is a quadlet on the `seed` network behind Caddy at `s3.lab` (D11). It is **not** zot's or Gitea's live backend — those stay on local disk, dependency-free, so a MinIO outage never stops an image pull. restic runs as a host-level systemd timer, dumping Postgres and snapshotting `/var/lib/seed` into a MinIO bucket. The restic repository is a single-variable **knob**: local MinIO by default, an off-host repo when you want real disaster recovery.

**Tech Stack:** Everything from Phases 0–2, plus MinIO, the MinIO client `mc`, and host `restic`.

## Global Constraints

Phases 0–2 constraints hold. Additional:

- **MinIO is a target and a store, never a live backend for zot or Gitea (D11).** Do not point zot's `rootDirectory` or Gitea's storage at S3.
- **Same-host backup is not disaster recovery.** The default restic repo is MinIO *on the seed*, which protects against data corruption, not host loss. `seed_restic_repository` is the knob that points off-host; the honesty stays in the docs and this constraint.
- **The acceptance is a restore, not a backup.** The phase is not done when a snapshot exists; it is done when a wiped service is restored from one and serves its data again.
- **Phase gate:** `seed/acceptance/phase3.sh` — take a snapshot, wipe zot's and Gitea's state, restore, and confirm an image pulls from zot and a Gitea repo is back.

## File Structure

Extends the Phase 0–2 role. New files:

```
seed/roles/seed/
  templates/
    minio.container.j2
    s3.caddy.j2
    seed-backup.env.j2       # 0600: restic repo + creds (rendered from secrets)
    seed-backup.sh.j2        # pg dump + restic backup + forget
    seed-backup.service.j2   # oneshot
    seed-backup.timer.j2     # daily
  tasks/
    minio.yml
    backup.yml
seed/acceptance/phase3.sh
```

Modified: `tasks/main.yml`, `defaults/main.yml`.

---

### Task 1: MinIO and the buckets

An S3 store behind `s3.lab`, with the `seed-backups` (restic target) and `tofu-state` buckets. Root password from a mounted file via MinIO's `_FILE` convention.

**Files:**
- Create: `seed/roles/seed/templates/minio.container.j2`, `s3.caddy.j2`, `seed/roles/seed/tasks/minio.yml`
- Modify: `seed/roles/seed/tasks/main.yml`, `seed/roles/seed/defaults/main.yml`, `seed/roles/seed/handlers/main.yml` (`restart minio`)

**Interfaces:**
- Consumes: `seed` network, `secrets.yml`, Caddy conf.d.
- Produces: `minio.service` at `https://s3.lab` (alias `minio`/`s3.lab`); root user `{{ seed_registry_user }}` (`seed`), password in `secrets/minio_root_password`; buckets `seed-backups`, `tofu-state`. Consumed by Task 2.

- [ ] **Step 1: See the goal unmet** — `systemctl is-active minio.service` → `inactive`.

- [ ] **Step 2: Add to `seed/roles/seed/defaults/main.yml`**

```yaml
seed_minio_image: quay.io/minio/minio:RELEASE.2024-08-17T01-24-54Z
seed_mc_image: quay.io/minio/mc:RELEASE.2024-08-17T01-24-54Z
```

- [ ] **Step 3: Create `seed/roles/seed/templates/minio.container.j2`**

```ini
[Unit]
Description=MinIO (S3 store + restic target)
After=network-online.target seed-network.service
Wants=network-online.target

[Container]
Image={{ seed_minio_image }}
ContainerName=minio
Network=seed
NetworkAlias=s3.{{ seed_domain }}
Volume={{ seed_state_root }}/minio/data:/data
Volume={{ seed_state_root }}/secrets/minio_root_password:/run/secrets/minio_root_password:ro
Environment=MINIO_ROOT_USER={{ seed_registry_user }}
Environment=MINIO_ROOT_PASSWORD_FILE=/run/secrets/minio_root_password
Exec=server /data --console-address :9001

[Service]
Restart=on-failure

[Install]
WantedBy=default.target
```

- [ ] **Step 4: Create `seed/roles/seed/templates/s3.caddy.j2`**

```caddyfile
s3.{{ seed_domain }} {
	reverse_proxy minio:9000
}
```

- [ ] **Step 5: Add the `restart minio` handler to `seed/roles/seed/handlers/main.yml`**

```yaml
- name: restart minio
  ansible.builtin.systemd_service:
    name: minio.service
    state: restarted
```

- [ ] **Step 6: Create `seed/roles/seed/tasks/minio.yml`**

```yaml
---
- name: Ensure MinIO data dir exists
  ansible.builtin.file:
    path: "{{ seed_state_root }}/minio/data"
    state: directory
    owner: root
    group: root
    mode: "0700"

- name: Ensure the MinIO root password exists
  ansible.builtin.include_tasks: secrets.yml
  vars:
    secret_name: minio_root_password

- name: Render the MinIO quadlet
  ansible.builtin.template:
    src: minio.container.j2
    dest: /etc/containers/systemd/minio.container
    owner: root
    group: root
    mode: "0644"
  notify: reload systemd

- name: Add the MinIO Caddy vhost
  ansible.builtin.template:
    src: s3.caddy.j2
    dest: "{{ seed_state_root }}/caddy/conf.d/s3.caddy"
    owner: root
    group: root
    mode: "0644"
  notify: restart caddy

- name: Apply pending changes
  ansible.builtin.meta: flush_handlers

- name: Start MinIO
  ansible.builtin.systemd_service:
    name: minio.service
    state: started

- name: Wait for MinIO
  ansible.builtin.uri:
    url: "https://s3.{{ seed_domain }}/minio/health/live"
    ca_path: /etc/ssl/seed/root_ca.crt
  register: _minio_up
  until: _minio_up.status == 200
  retries: 30
  delay: 2

- name: Read the MinIO root password
  ansible.builtin.slurp:
    src: "{{ seed_state_root }}/secrets/minio_root_password"
  register: _minio_pw
  no_log: true

- name: Create the buckets (idempotent)
  ansible.builtin.command: >
    podman run --rm --network seed {{ seed_mc_image }}
    sh -c "mc alias set s ${MC_HOST} {{ seed_registry_user }} '{{ _minio_pw.content | b64decode }}' &&
           mc mb -p s/seed-backups && mc mb -p s/tofu-state"
  environment:
    MC_HOST: "http://minio:9000"
  register: _mc
  changed_when: "'created' in _mc.stdout"
  failed_when: _mc.rc != 0 and 'already own it' not in _mc.stderr
  no_log: true
```

- [ ] **Step 7: Wire into `seed/roles/seed/tasks/main.yml`** (append at the end):

```yaml
- name: Bring up MinIO (S3 store + backup target)
  ansible.builtin.include_tasks: minio.yml
```

- [ ] **Step 8: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: MinIO up; health wait passes; buckets created.

- [ ] **Step 9: Gate — MinIO healthy with both buckets**

Run (on the target):
```bash
curl -sf -o /dev/null -w '%{http_code}\n' https://s3.lab/minio/health/live
pw=$(cat /var/lib/seed/secrets/minio_root_password)
podman run --rm --network seed quay.io/minio/mc:RELEASE.2024-08-17T01-24-54Z \
  sh -c "mc alias set s http://minio:9000 seed '$pw' && mc ls s/"
```
Expected: `200`; `mc ls` lists `seed-backups/` and `tofu-state/`.

- [ ] **Step 10: Idempotence** — re-run; expected `changed=0` (buckets already owned).

- [ ] **Step 11: Commit**

```bash
git add seed/roles/seed/templates/minio.container.j2 seed/roles/seed/templates/s3.caddy.j2 seed/roles/seed/tasks/minio.yml seed/roles/seed/tasks/main.yml seed/roles/seed/handlers/main.yml seed/roles/seed/defaults/main.yml
git commit -m "feat(seed): minio s3 store and buckets"
```

### Task 2: restic backup on a timer

A host `restic` timer that dumps Postgres and snapshots `/var/lib/seed` into the `seed-backups` bucket. Secrets stay out of the world-readable script via a `0600` `EnvironmentFile`. The repository is the off-host knob.

**Files:**
- Create: `seed/roles/seed/templates/seed-backup.env.j2`, `seed-backup.sh.j2`, `seed-backup.service.j2`, `seed-backup.timer.j2`, `seed/roles/seed/tasks/backup.yml`
- Modify: `seed/roles/seed/tasks/main.yml`, `seed/roles/seed/defaults/main.yml`

**Interfaces:**
- Consumes: MinIO + root password (Task 1), Postgres (Phase 1), `secrets.yml`.
- Produces: `/usr/local/bin/seed-backup`; `seed-backup.timer` (daily); the `restic_password` secret; snapshots in `seed-backups`. Consumed by Task 3.

- [ ] **Step 1: See the goal unmet** — `systemctl is-enabled seed-backup.timer` → not found.

- [ ] **Step 2: Add to `seed/roles/seed/defaults/main.yml`** (the off-host knob):

```yaml
# Default: MinIO on the seed (protects against corruption, NOT host loss).
# Point off-host for real disaster recovery, e.g. s3:https://nas.lan:9000/seed
seed_restic_repository: "s3:https://s3.{{ seed_domain }}/seed-backups"
```

- [ ] **Step 3: Create `seed/roles/seed/templates/seed-backup.env.j2`** (rendered `0600`)

```jinja
RESTIC_REPOSITORY={{ seed_restic_repository }}
RESTIC_PASSWORD_FILE={{ seed_state_root }}/secrets/restic_password
AWS_ACCESS_KEY_ID={{ seed_registry_user }}
AWS_SECRET_ACCESS_KEY={{ _minio_pw }}
```

- [ ] **Step 4: Create `seed/roles/seed/templates/seed-backup.sh.j2`**

No secrets in here (they arrive via the service's `EnvironmentFile`), so it is safe at `0755`. Postgres is dumped rather than copied live; MinIO's own data is never in the set (it is the target).

```bash
#!/usr/bin/env bash
set -euo pipefail
umask 077
mkdir -p {{ seed_state_root }}/backup
podman exec postgres pg_dumpall -U postgres > {{ seed_state_root }}/backup/postgres.sql
restic snapshots >/dev/null 2>&1 || restic init
restic backup \
  {{ seed_state_root }}/zot {{ seed_state_root }}/gitea {{ seed_state_root }}/authentik \
  {{ seed_state_root }}/step-ca {{ seed_state_root }}/caddy {{ seed_state_root }}/secrets \
  {{ seed_state_root }}/assets {{ seed_state_root }}/tftp \
  {{ seed_state_root }}/backup/postgres.sql
restic forget --keep-daily 7 --keep-weekly 4 --prune
```

- [ ] **Step 5: Create the unit templates**

`seed-backup.service.j2`:
```ini
[Unit]
Description=Seed backup to restic
After=minio.service postgres.service
Wants=minio.service

[Service]
Type=oneshot
EnvironmentFile={{ seed_state_root }}/backup/seed-backup.env
ExecStart=/usr/local/bin/seed-backup
```

`seed-backup.timer.j2`:
```ini
[Unit]
Description=Daily seed backup

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
```

- [ ] **Step 6: Create `seed/roles/seed/tasks/backup.yml`**

```yaml
---
- name: Install restic
  ansible.builtin.package:
    name: restic
    state: present

- name: Ensure the restic password exists
  ansible.builtin.include_tasks: secrets.yml
  vars:
    secret_name: restic_password

- name: Ensure the backup dir exists
  ansible.builtin.file:
    path: "{{ seed_state_root }}/backup"
    state: directory
    owner: root
    group: root
    mode: "0700"

- name: Read the MinIO root password
  ansible.builtin.slurp:
    src: "{{ seed_state_root }}/secrets/minio_root_password"
  register: _minio_pw_b64
  no_log: true

- name: Bind the MinIO password
  ansible.builtin.set_fact:
    _minio_pw: "{{ _minio_pw_b64.content | b64decode }}"
  no_log: true

- name: Render the backup env file
  ansible.builtin.template:
    src: seed-backup.env.j2
    dest: "{{ seed_state_root }}/backup/seed-backup.env"
    owner: root
    group: root
    mode: "0600"
  no_log: true

- name: Install the backup script
  ansible.builtin.template:
    src: seed-backup.sh.j2
    dest: /usr/local/bin/seed-backup
    owner: root
    group: root
    mode: "0755"

- name: Install the backup units
  ansible.builtin.template:
    src: "{{ item }}.j2"
    dest: "/etc/systemd/system/{{ item }}"
    owner: root
    group: root
    mode: "0644"
  loop:
    - seed-backup.service
    - seed-backup.timer
  notify: reload systemd

- name: Apply pending changes
  ansible.builtin.meta: flush_handlers

- name: Enable the daily backup timer
  ansible.builtin.systemd_service:
    name: seed-backup.timer
    enabled: true
    state: started
```

- [ ] **Step 7: Wire into `seed/roles/seed/tasks/main.yml`** (append at the end):

```yaml
- name: Install the restic backup timer
  ansible.builtin.include_tasks: backup.yml
```

- [ ] **Step 8: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: restic installed; env/script/units rendered; timer enabled.

- [ ] **Step 9: Gate — a backup runs and lands a snapshot**

Run (on the target):
```bash
systemctl start seed-backup.service && systemctl is-failed seed-backup.service || true
set -a; . /var/lib/seed/backup/seed-backup.env; set +a
restic snapshots
systemctl is-enabled seed-backup.timer
```
Expected: the service completes (`inactive`/`dead`, not `failed`); `restic snapshots` lists at least one snapshot; the timer is `enabled`.

- [ ] **Step 10: Idempotence** — re-run the playbook; expected `changed=0`.

- [ ] **Step 11: Commit**

```bash
git add seed/roles/seed/templates/seed-backup.env.j2 seed/roles/seed/templates/seed-backup.sh.j2 seed/roles/seed/templates/seed-backup.service.j2 seed/roles/seed/templates/seed-backup.timer.j2 seed/roles/seed/tasks/backup.yml seed/roles/seed/tasks/main.yml seed/roles/seed/defaults/main.yml
git commit -m "feat(seed): restic backup timer to minio"
```

### Task 3: The restore drill

The phase — and the whole build — is not proven until a wiped service comes back. Snapshot, destroy zot's and Gitea's on-disk state, restore from restic, and confirm the data serves again.

**Files:**
- Create: `seed/acceptance/phase3.sh`

**Interfaces:**
- Consumes: the backup timer (Task 2), a known artifact from Phase 1 (`registry.lab/seed/busybox:test` and the `smoke` repo).
- Produces: the restore-drill gate.

- [ ] **Step 1: Precondition — the known artifacts exist**

From Phase 1 there should be an image `registry.lab/seed/busybox:test` and a Gitea repo `seedadmin/smoke`. If not, run `seed/acceptance/phase1.sh` first. Verify:
```bash
podman pull registry.lab/seed/busybox:test >/dev/null && echo image-ok
curl -fsS -o /dev/null https://git.lab/seedadmin/smoke && echo repo-ok
```
Expected: `image-ok`, `repo-ok`.

- [ ] **Step 2: Create `seed/acceptance/phase3.sh`**

```bash
#!/usr/bin/env bash
# Phase 3 gate: a RESTORE, not a backup. Wipe zot + gitea on-disk state,
# restore from restic, and prove the data is back.
set -euo pipefail
set -a; . /var/lib/seed/backup/seed-backup.env; set +a
D=${SEED_DOMAIN:-lab}

# 1. fresh snapshot of current state
systemctl start seed-backup.service

# 2. simulate loss: stop the services and wipe their on-disk state
systemctl stop zot.service gitea.service
rm -rf /var/lib/seed/zot/registry /var/lib/seed/gitea/data

# 3. restore exactly those paths from the latest snapshot
restic restore latest --target / \
  --include /var/lib/seed/zot/registry \
  --include /var/lib/seed/gitea/data

# 4. bring the services back
systemctl start zot.service gitea.service
sleep 5

# 5. prove it: the image pulls again, and the repo is back
podman rmi "registry.${D}/seed/busybox:test" >/dev/null 2>&1 || true
podman pull "registry.${D}/seed/busybox:test" >/dev/null && echo "zot: image restored"
curl -fsS -o /dev/null "https://git.${D}/seedadmin/smoke" && echo "gitea: repo restored"

echo "phase3 ok: restore verified"
```

- [ ] **Step 3: Run the drill**

```bash
chmod +x seed/acceptance/phase3.sh
cd seed && ansible seed-dev -m script -a 'acceptance/phase3.sh'
```
Expected: `zot: image restored`, `gitea: repo restored`, `phase3 ok: restore verified`. The registry blobs — the recovery-critical path — came back from a snapshot and served a pull.

- [ ] **Step 4: Note the fuller drill (documented, run when you want it)**

The drill above wipes on-disk state with Postgres intact. To rehearse **database** loss too: `podman exec postgres dropdb -U postgres gitea`, restore, then `podman exec -i postgres psql -U postgres < /var/lib/seed/backup/postgres.sql` and restart Gitea. The `postgres.sql` dump is in every snapshot for exactly this.

- [ ] **Step 5: Idempotence** — the drill is destructive-then-restorative by design; re-running it simply repeats the cycle and must end `phase3 ok` again. (Nothing in the role changed, so a plain playbook re-run is still `changed=0`.)

- [ ] **Step 6: Commit**

```bash
git add seed/acceptance/phase3.sh
git commit -m "test(seed): restore drill (wipe zot+gitea, restore, verify)"
```

---

## Phase 3 Done — Definition of Done

- MinIO serves `s3.lab` with `seed-backups` and `tofu-state` buckets; it is a target/store, never zot's or Gitea's live backend (D11).
- A daily restic timer snapshots `/var/lib/seed` (Postgres dumped, MinIO excluded) into `seed-backups`.
- `phase3.sh` passes: zot and Gitea state wiped and **restored**, the registry image and Gitea repo back.
- The off-host DR knob (`seed_restic_repository`) is in place; same-host backup is documented as not-yet-DR until it points elsewhere.

## Self-Review

- **Spec coverage:** D11 MinIO-as-target-not-backend — Task 1 + the Global Constraint. restic timer + off-host knob — Task 2. Restore drill as the gate — Task 3.
- **Placeholder scan:** none. The off-host repo example and the DB-loss drill are documented alternatives, not TODOs.
- **Consistency:** `seed_restic_repository`, `minio_root_password`, the `s3.lab`/`seed-backups` names, and the `/var/lib/seed` paths match across Tasks 1–3 and back to the Phase 2 state layout.

---

## The seed, end to end

With Phases 0–3 applied, `ansible-playbook -i inventory/<host> seed/site.yml` stands up — in a dev VM or on a bare-metal management node, from one definition — a self-contained stack that owns the supply chain (zot + cache), the source and packages (Gitea), identity (Authentik), trust (step-ca), day-0 netboot (dnsmasq/iPXE/assets/chrony), and durable, **restore-proven** state (MinIO + restic). tinq boots and adopts Talos clusters against it, pulling every image over verified HTTPS — and the whole thing is rebuildable from a restic snapshot after a total loss, which is the reason it exists.
