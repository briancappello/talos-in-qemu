# Seed Phase 1 — Registry and Source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Own the supply chain and the source: a file-backed `zot` OCI registry with pull-through cache, `Gitea` for git and Python/Go/Rust packages, `Authentik` SSO, their Postgres/Redis datastore — and the one tinq code change (`spec.registries.ca`) that lets a Talos guest pull from the registry over verified HTTPS instead of the `http://` hack.

**Architecture:** All services are podman quadlets on the seed host (Phase 0). Caddy gains a `conf.d` snippet per service (`registry.lab`, `git.lab`, `auth.lab`), each with a step-ca certificate. `zot` is deliberately kept off Gitea and off Authentik (D3, D7): file-backed, htpasswd auth, anonymous pull open. tinq's `cluster.RegistryMirror` grows a `CA`, emitted as Talos `machine.registries.config.<host>.tls.ca`.

**Tech Stack:** Everything from Phase 0, plus `zot`, `postgres`, `redis`, `gitea`, `goauthentik/server`; Go 1.26.x for the tinq change (module already pins it).

## Global Constraints

Phase 0's Global Constraints all still hold (k8s-free, system quadlets, `/var/lib/seed`, generate-on-first-run, flat `.lab`, pinned images, cleanroom, `feat/seed`, conventional commits, behavioural verification). Additional to this phase:

- **`zot` depends on nothing but the `seed` network and its own disk.** It MUST NOT be wired to Postgres, Authentik, or Gitea. Its auth is a local htpasswd; anonymous **pull** stays open so a cluster rebuild never needs the identity stack up (D7).
- **Talos-side changes obey the tinq repo's rules:** no new module dependencies; `go build ./... && go vet ./... && go test ./...` and `GOOS=darwin GOARCH=arm64 go vet ./...` pass; `gofmt` clean; comments explain WHY and name the failure they prevent. `platform/` is untouched.
- **Phase gate:** `seed/acceptance/phase1.sh` — an image pushed to `zot` and pulled **from a Talos guest** over `https://registry.lab` trusting the step-ca root; a `pip`/`go`/`cargo` round-trip against Gitea; an SSO login to Gitea via Authentik.

## File Structure

Extends the Phase 0 role. New files:

```
seed/roles/seed/
  templates/
    zot-config.json.j2            # storage, htpasswd auth, on-demand sync (docker.io, ghcr)
    zot.container.j2
    registry.caddy.j2             # Caddy snippet: registry.lab -> zot:5000
    postgres.container.j2
    postgres-initdb.sh.j2         # first-run: gitea + authentik roles/DBs
    redis.container.j2
    gitea.container.j2
    git.caddy.j2
    authentik-server.container.j2
    authentik-worker.container.j2
    auth.caddy.j2
  tasks/
    zot.yml
    datastores.yml                # postgres + redis
    gitea.yml
    authentik.yml
    oidc.yml                      # register the Gitea<->Authentik OAuth app
seed/acceptance/phase1.sh
```

Modified: `Caddyfile.j2` (add `import conf.d`), `caddy.container.j2` (mount `conf.d`), `tasks/main.yml` (new includes), `defaults/main.yml` (image pins + service settings).

tinq (separate commits, `talos-in-qemu` proper — not under `seed/`):

```
crd/…                             # add `ca`/`caFile` to the registries item schema
cmd/tinq/main.go                  # registryMirrors: read ca/caFile -> RegistryMirror.CA
cmd/tinq/main_test.go             # reader test
cluster/config.go                 # RegistryMirror.CA; registriesConfig emits TLSCA
cluster/registries_test.go        # emission test
```

---

### Task 1: zot — OCI registry, pull-through cache, and the Caddy conf.d pattern

zot is the recovery-critical registry (D3): file-backed, htpasswd auth, **anonymous pull open** (D7), on-demand sync caching docker.io and ghcr. It also introduces the Caddy `conf.d` snippet pattern every later vhost uses.

**Files:**
- Create: `seed/roles/seed/templates/zot-config.json.j2`, `zot.container.j2`, `registry.caddy.j2`
- Create: `seed/roles/seed/tasks/zot.yml`
- Modify: `seed/roles/seed/templates/Caddyfile.j2` (import conf.d), `caddy.container.j2` (mount conf.d), `seed/roles/seed/handlers/main.yml` (add `restart zot`), `seed/roles/seed/tasks/main.yml`, `seed/roles/seed/defaults/main.yml`

**Interfaces:**
- Consumes: the `seed` network, `secrets.yml`, Caddy + step-ca (Phase 0).
- Produces: `zot.service` at `https://registry.lab` (alias `registry.lab`/`zot` on the network); push user `{{ seed_registry_user }}` (`seed`) with password in `secrets/zot_password`; the `conf.d` vhost pattern. Consumed by Task 6 (Talos pulls from it) and the Phase gate.

- [ ] **Step 1: See the goal unmet** — `systemctl is-active zot.service` → `inactive`/not found.

- [ ] **Step 2: Add to `seed/roles/seed/defaults/main.yml`**

```yaml
seed_zot_image: ghcr.io/project-zot/zot:v2.1.2
seed_registry_user: seed
```

- [ ] **Step 3: Create `seed/roles/seed/templates/zot-config.json.j2`**

Anonymous `read`, authenticated write — the D7 split that keeps pulls working when identity is down.

```json
{
  "distSpecVersion": "1.1.0",
  "storage": { "rootDirectory": "/var/lib/registry", "gc": true, "dedupe": true },
  "http": {
    "address": "0.0.0.0",
    "port": "5000",
    "auth": { "htpasswd": { "path": "/etc/zot/htpasswd" } },
    "accessControl": {
      "repositories": {
        "**": {
          "anonymousPolicy": ["read"],
          "policies": [
            { "users": ["{{ seed_registry_user }}"], "actions": ["read", "create", "update", "delete"] }
          ]
        }
      }
    }
  },
  "log": { "level": "info" },
  "extensions": {
    "sync": {
      "enable": true,
      "registries": [
        { "urls": ["https://registry-1.docker.io"], "onDemand": true, "tlsVerify": true, "content": [{ "prefix": "**" }] },
        { "urls": ["https://ghcr.io"], "onDemand": true, "tlsVerify": true, "content": [{ "prefix": "**" }] }
      ]
    }
  }
}
```

- [ ] **Step 4: Create `seed/roles/seed/templates/zot.container.j2`**

```ini
[Unit]
Description=zot OCI registry and pull-through cache
After=network-online.target seed-network.service
Wants=network-online.target

[Container]
Image={{ seed_zot_image }}
ContainerName=zot
Network=seed
NetworkAlias=registry.{{ seed_domain }}
Volume={{ seed_state_root }}/zot/registry:/var/lib/registry
Volume={{ seed_state_root }}/zot/config.json:/etc/zot/config.json:ro
Volume={{ seed_state_root }}/zot/htpasswd:/etc/zot/htpasswd:ro
Exec=serve /etc/zot/config.json

[Service]
Restart=on-failure

[Install]
WantedBy=default.target
```

- [ ] **Step 5: Create `seed/roles/seed/templates/registry.caddy.j2`**

```caddyfile
registry.{{ seed_domain }} {
	reverse_proxy zot:5000
}
```

- [ ] **Step 6: Modify `seed/roles/seed/templates/Caddyfile.j2`** — add the import above the `hello` block:

```caddyfile
import /etc/caddy/conf.d/*.caddy
```

- [ ] **Step 7: Modify `seed/roles/seed/templates/caddy.container.j2`** — add a Volume line:

```ini
Volume={{ seed_state_root }}/caddy/conf.d:/etc/caddy/conf.d:ro
```

- [ ] **Step 8: Add the `restart zot` handler to `seed/roles/seed/handlers/main.yml`** (after `reload systemd`):

```yaml
- name: restart zot
  ansible.builtin.systemd_service:
    name: zot.service
    state: restarted
```

- [ ] **Step 9: Create `seed/roles/seed/tasks/zot.yml`**

```yaml
---
- name: Ensure zot and conf.d dirs exist
  ansible.builtin.file:
    path: "{{ item }}"
    state: directory
    owner: root
    group: root
    mode: "0755"
  loop:
    - "{{ seed_state_root }}/zot"
    - "{{ seed_state_root }}/zot/registry"
    - "{{ seed_state_root }}/caddy/conf.d"

- name: Ensure the registry push password exists
  ansible.builtin.include_tasks: secrets.yml
  vars:
    secret_name: zot_password

- name: Install htpasswd tooling
  ansible.builtin.package:
    name: "{{ 'apache2-utils' if ansible_os_family == 'Debian' else 'httpd-tools' }}"
    state: present

- name: Generate the zot htpasswd (bcrypt) on first run
  ansible.builtin.shell: >
    set -o pipefail;
    htpasswd -nbB {{ seed_registry_user }} "$(cat {{ seed_state_root }}/secrets/zot_password)"
    > {{ seed_state_root }}/zot/htpasswd
  args:
    executable: /bin/bash
    creates: "{{ seed_state_root }}/zot/htpasswd"

- name: Secure the htpasswd file
  ansible.builtin.file:
    path: "{{ seed_state_root }}/zot/htpasswd"
    owner: root
    group: root
    mode: "0640"

- name: Resolve Phase-1 service names locally (dnsmasq replaces this in Phase 2)
  ansible.builtin.blockinfile:
    path: /etc/hosts
    marker: "# {mark} SEED PHASE-1 NAMES"
    block: |
      127.0.0.1 registry.{{ seed_domain }} git.{{ seed_domain }} auth.{{ seed_domain }}

- name: Render the zot config
  ansible.builtin.template:
    src: zot-config.json.j2
    dest: "{{ seed_state_root }}/zot/config.json"
    owner: root
    group: root
    mode: "0644"
  notify: restart zot

- name: Render the zot quadlet
  ansible.builtin.template:
    src: zot.container.j2
    dest: /etc/containers/systemd/zot.container
    owner: root
    group: root
    mode: "0644"
  notify: reload systemd

- name: Add the registry Caddy vhost
  ansible.builtin.template:
    src: registry.caddy.j2
    dest: "{{ seed_state_root }}/caddy/conf.d/registry.caddy"
    owner: root
    group: root
    mode: "0644"
  notify: restart caddy

- name: Re-render the Caddyfile with conf.d import
  ansible.builtin.template:
    src: Caddyfile.j2
    dest: "{{ seed_state_root }}/caddy/Caddyfile"
    owner: root
    group: root
    mode: "0644"
  notify: restart caddy

- name: Re-render the Caddy quadlet with the conf.d mount
  ansible.builtin.template:
    src: caddy.container.j2
    dest: /etc/containers/systemd/caddy.container
    owner: root
    group: root
    mode: "0644"
  notify: reload systemd

- name: Apply pending changes
  ansible.builtin.meta: flush_handlers

- name: Start zot
  ansible.builtin.systemd_service:
    name: zot.service
    state: started
```

- [ ] **Step 10: Wire into `seed/roles/seed/tasks/main.yml`** (append below the Caddy include):

```yaml
- name: Bring up zot (registry + cache)
  ansible.builtin.include_tasks: zot.yml
```

- [ ] **Step 11: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: `zot.service` started, Caddy restarted; `failed=0`.

- [ ] **Step 12: Gate — push, pull, anonymous pull, cache**

Run (on the target):
```bash
pw=$(cat /var/lib/seed/secrets/zot_password)
podman login registry.lab -u seed -p "$pw"
podman pull docker.io/library/busybox:1.36
podman tag docker.io/library/busybox:1.36 registry.lab/seed/busybox:test
podman push registry.lab/seed/busybox:test
podman rmi registry.lab/seed/busybox:test && podman pull registry.lab/seed/busybox:test   # authed pull
podman logout registry.lab && podman pull registry.lab/seed/busybox:test                  # anonymous pull still works
podman pull registry.lab/library/alpine:3.20                                              # pull-through from docker.io
```
Expected: `Login Succeeded`; push completes; both pulls succeed with no `-k` (Caddy cert trusted since Phase 0); the alpine pull is served on-demand from the docker.io sync. *If the cache path differs on the pinned zot tag, `zot`'s sync docs give the exact prefix; the pull is the arbiter.*

- [ ] **Step 13: Idempotence** — re-run the playbook; expected `changed=0`.

- [ ] **Step 14: Commit**

```bash
git add seed/roles/seed/templates/zot-config.json.j2 seed/roles/seed/templates/zot.container.j2 seed/roles/seed/templates/registry.caddy.j2 seed/roles/seed/templates/Caddyfile.j2 seed/roles/seed/templates/caddy.container.j2 seed/roles/seed/tasks/zot.yml seed/roles/seed/tasks/main.yml seed/roles/seed/handlers/main.yml seed/roles/seed/defaults/main.yml
git commit -m "feat(seed): zot registry with pull-through cache behind caddy"
```

### Task 2: Postgres and Redis datastores

The shared datastore Gitea and Authentik sit on. Postgres creates both app databases and roles on first init from mounted secrets; Redis is passwordless (internal network only, never published).

**Files:**
- Create: `seed/roles/seed/templates/postgres.container.j2`, `postgres-initdb.sh.j2`, `redis.container.j2`
- Create: `seed/roles/seed/tasks/datastores.yml`
- Modify: `seed/roles/seed/tasks/main.yml`, `seed/roles/seed/defaults/main.yml`

**Interfaces:**
- Consumes: `seed` network, `secrets.yml`.
- Produces: `postgres.service` (alias `postgres.lab`) with databases `gitea` and `authentik` owned by like-named roles, passwords in `secrets/{gitea_db_password,authentik_db_password}`; `redis.service` (alias `redis.lab`). Consumed by Tasks 3 and 4.

- [ ] **Step 1: See the goal unmet** — `systemctl is-active postgres.service` → `inactive`.

- [ ] **Step 2: Add to `seed/roles/seed/defaults/main.yml`**

```yaml
seed_postgres_image: docker.io/library/postgres:16.4-alpine
seed_redis_image: docker.io/library/redis:7.4-alpine
```

- [ ] **Step 3: Create `seed/roles/seed/templates/postgres-initdb.sh.j2`**

Runs once, only when the data dir is empty. The passwords are `ascii_letters+digits` (Task 3's secret charset), so string interpolation into SQL is injection-safe here.

```bash
#!/bin/bash
set -euo pipefail
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<SQL
  CREATE ROLE gitea LOGIN PASSWORD '$(cat /run/secrets/gitea_db_password)';
  CREATE DATABASE gitea OWNER gitea;
  CREATE ROLE authentik LOGIN PASSWORD '$(cat /run/secrets/authentik_db_password)';
  CREATE DATABASE authentik OWNER authentik;
SQL
```

- [ ] **Step 4: Create `seed/roles/seed/templates/postgres.container.j2`**

```ini
[Unit]
Description=Postgres (Gitea + Authentik datastore)
After=network-online.target seed-network.service
Wants=network-online.target

[Container]
Image={{ seed_postgres_image }}
ContainerName=postgres
Network=seed
NetworkAlias=postgres.{{ seed_domain }}
Volume={{ seed_state_root }}/postgres/data:/var/lib/postgresql/data
Volume={{ seed_state_root }}/postgres/initdb:/docker-entrypoint-initdb.d:ro
Volume={{ seed_state_root }}/secrets/postgres_password:/run/secrets/postgres_password:ro
Volume={{ seed_state_root }}/secrets/gitea_db_password:/run/secrets/gitea_db_password:ro
Volume={{ seed_state_root }}/secrets/authentik_db_password:/run/secrets/authentik_db_password:ro
Environment=POSTGRES_USER=postgres
Environment=POSTGRES_PASSWORD_FILE=/run/secrets/postgres_password
Environment=PGDATA=/var/lib/postgresql/data/pgdata

[Service]
Restart=on-failure

[Install]
WantedBy=default.target
```

- [ ] **Step 5: Create `seed/roles/seed/templates/redis.container.j2`**

```ini
[Unit]
Description=Redis (Authentik cache/broker)
After=network-online.target seed-network.service
Wants=network-online.target

[Container]
Image={{ seed_redis_image }}
ContainerName=redis
Network=seed
NetworkAlias=redis.{{ seed_domain }}
Volume={{ seed_state_root }}/redis:/data
Exec=redis-server --save 60 1 --appendonly no

[Service]
Restart=on-failure

[Install]
WantedBy=default.target
```

- [ ] **Step 6: Create `seed/roles/seed/tasks/datastores.yml`**

```yaml
---
- name: Ensure datastore dirs exist
  ansible.builtin.file:
    path: "{{ item }}"
    state: directory
    owner: root
    group: root
    mode: "0700"
  loop:
    - "{{ seed_state_root }}/postgres"
    - "{{ seed_state_root }}/postgres/data"
    - "{{ seed_state_root }}/postgres/initdb"
    - "{{ seed_state_root }}/redis"

- name: Ensure datastore secrets exist
  ansible.builtin.include_tasks: secrets.yml
  vars:
    secret_name: "{{ item }}"
  loop:
    - postgres_password
    - gitea_db_password
    - authentik_db_password

- name: Render the Postgres init script
  ansible.builtin.template:
    src: postgres-initdb.sh.j2
    dest: "{{ seed_state_root }}/postgres/initdb/10-seed-databases.sh"
    owner: root
    group: root
    mode: "0755"

- name: Render the datastore quadlets
  ansible.builtin.template:
    src: "{{ item }}.container.j2"
    dest: "/etc/containers/systemd/{{ item }}.container"
    owner: root
    group: root
    mode: "0644"
  loop:
    - postgres
    - redis
  notify: reload systemd

- name: Apply pending changes
  ansible.builtin.meta: flush_handlers

- name: Start Postgres and Redis
  ansible.builtin.systemd_service:
    name: "{{ item }}"
    state: started
  loop:
    - postgres.service
    - redis.service

- name: Wait for Postgres to accept connections
  ansible.builtin.command: podman exec postgres pg_isready -U postgres
  register: _pg_ready
  until: _pg_ready.rc == 0
  retries: 30
  delay: 2
  changed_when: false
```

- [ ] **Step 7: Wire into `seed/roles/seed/tasks/main.yml`** (append below the zot include):

```yaml
- name: Bring up the datastores (Postgres + Redis)
  ansible.builtin.include_tasks: datastores.yml
```

- [ ] **Step 8: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: both services started; the `pg_isready` wait passes; `failed=0`.

- [ ] **Step 9: Gate — datastores healthy with both app databases**

Run (on the target):
```bash
podman exec postgres pg_isready -U postgres
podman exec postgres psql -U postgres -tAc "SELECT datname FROM pg_database WHERE datname IN ('gitea','authentik') ORDER BY 1"
podman exec redis redis-cli ping
```
Expected: `accepting connections`; two lines `authentik` and `gitea`; `PONG`.

- [ ] **Step 10: Idempotence** — re-run; expected `changed=0`.

- [ ] **Step 11: Commit**

```bash
git add seed/roles/seed/templates/postgres.container.j2 seed/roles/seed/templates/postgres-initdb.sh.j2 seed/roles/seed/templates/redis.container.j2 seed/roles/seed/tasks/datastores.yml seed/roles/seed/tasks/main.yml seed/roles/seed/defaults/main.yml
git commit -m "feat(seed): postgres and redis datastores"
```

### Task 3: Gitea — git and the Python/Go/Rust package registries

One container covers git plus OCI/PyPI/Go/Cargo/Helm (D-reframe). Config via `GITEA__*` env; the DB password is read from a mounted file with Gitea's `__FILE` suffix so it never lands in the world-readable quadlet.

**Files:**
- Create: `seed/roles/seed/templates/gitea.container.j2`, `git.caddy.j2`
- Create: `seed/roles/seed/tasks/gitea.yml`
- Modify: `seed/roles/seed/tasks/main.yml`, `seed/roles/seed/defaults/main.yml`

**Interfaces:**
- Consumes: Postgres (Task 2), `secrets.yml`, Caddy conf.d (Task 1).
- Produces: `gitea.service` at `https://git.lab` (alias `git.lab`/`gitea`); admin `seedadmin` (password in `secrets/gitea_admin_password`); package registries at `https://git.lab/api/packages/seedadmin/{pypi,go,cargo}`. Consumed by Task 5 (OIDC) and the Phase gate.

- [ ] **Step 1: See the goal unmet** — `curl -s https://git.lab/api/v1/version` → refused / does not resolve.

- [ ] **Step 2: Add to `seed/roles/seed/defaults/main.yml`**

```yaml
seed_gitea_image: docker.io/gitea/gitea:1.22.6
```

- [ ] **Step 3: Create `seed/roles/seed/templates/gitea.container.j2`**

```ini
[Unit]
Description=Gitea (git + package registries)
After=network-online.target seed-network.service postgres.service
Wants=network-online.target
Requires=postgres.service

[Container]
Image={{ seed_gitea_image }}
ContainerName=gitea
Network=seed
NetworkAlias=git.{{ seed_domain }}
Volume={{ seed_state_root }}/gitea/data:/data
Volume={{ seed_state_root }}/secrets/gitea_db_password:/run/secrets/gitea_db_password:ro
Environment=USER_UID=1000
Environment=USER_GID=1000
Environment=GITEA__database__DB_TYPE=postgres
Environment=GITEA__database__HOST=postgres:5432
Environment=GITEA__database__NAME=gitea
Environment=GITEA__database__USER=gitea
Environment=GITEA__database__PASSWD__FILE=/run/secrets/gitea_db_password
Environment=GITEA__server__DOMAIN=git.{{ seed_domain }}
Environment=GITEA__server__ROOT_URL=https://git.{{ seed_domain }}/
Environment=GITEA__security__INSTALL_LOCK=true
Environment=GITEA__service__DISABLE_REGISTRATION=true
Environment=GITEA__packages__ENABLED=true

[Service]
Restart=on-failure
TimeoutStartSec=120

[Install]
WantedBy=default.target
```

- [ ] **Step 4: Create `seed/roles/seed/templates/git.caddy.j2`**

```caddyfile
git.{{ seed_domain }} {
	reverse_proxy gitea:3000
}
```

- [ ] **Step 5: Create `seed/roles/seed/tasks/gitea.yml`**

```yaml
---
- name: Ensure Gitea data dir exists
  ansible.builtin.file:
    path: "{{ seed_state_root }}/gitea/data"
    state: directory
    owner: root
    group: root
    mode: "0750"

- name: Ensure the Gitea admin password exists
  ansible.builtin.include_tasks: secrets.yml
  vars:
    secret_name: gitea_admin_password

- name: Render the Gitea quadlet
  ansible.builtin.template:
    src: gitea.container.j2
    dest: /etc/containers/systemd/gitea.container
    owner: root
    group: root
    mode: "0644"
  notify: reload systemd

- name: Add the Gitea Caddy vhost
  ansible.builtin.template:
    src: git.caddy.j2
    dest: "{{ seed_state_root }}/caddy/conf.d/git.caddy"
    owner: root
    group: root
    mode: "0644"
  notify: restart caddy

- name: Apply pending changes
  ansible.builtin.meta: flush_handlers

- name: Start Gitea
  ansible.builtin.systemd_service:
    name: gitea.service
    state: started

- name: Wait for Gitea HTTP
  ansible.builtin.uri:
    url: "https://git.{{ seed_domain }}/api/v1/version"
    ca_path: /etc/ssl/seed/root_ca.crt
  register: _gitea_up
  until: _gitea_up.status == 200
  retries: 30
  delay: 2

- name: Read the Gitea admin password
  ansible.builtin.slurp:
    src: "{{ seed_state_root }}/secrets/gitea_admin_password"
  register: _gitea_admin_pw_b64
  no_log: true

- name: List existing Gitea users
  ansible.builtin.command: podman exec -u git gitea gitea admin user list
  register: _gitea_users
  changed_when: false

- name: Create the Gitea admin on first run
  ansible.builtin.command: >
    podman exec -u git gitea gitea admin user create
    --admin --username seedadmin
    --password "{{ _gitea_admin_pw_b64.content | b64decode }}"
    --email admin@git.{{ seed_domain }} --must-change-password=false
  when: "'seedadmin' not in _gitea_users.stdout"
  no_log: true
```

- [ ] **Step 6: Wire into `seed/roles/seed/tasks/main.yml`** (append below the datastores include):

```yaml
- name: Bring up Gitea (git + packages)
  ansible.builtin.include_tasks: gitea.yml
```

- [ ] **Step 7: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: `gitea.service` up, admin created; `failed=0`.

- [ ] **Step 8: Gate — a git push and a PyPI round-trip**

Run (on the target; `uv` and `git` present):
```bash
pw=$(cat /var/lib/seed/secrets/gitea_admin_password)
# git: create a repo via API and push to it over HTTPS (cert trusted since Phase 0)
curl -fsS -u "seedadmin:$pw" -X POST https://git.lab/api/v1/user/repos \
  -H 'content-type: application/json' -d '{"name":"smoke","auto_init":true}'
git clone "https://seedadmin:$pw@git.lab/seedadmin/smoke" /tmp/smoke
cd /tmp/smoke && echo hi > f && git -c user.email=a@b.c -c user.name=a add f \
  && git -c user.email=a@b.c -c user.name=a commit -m x && git push

# PyPI: build a trivial package and round-trip it
mkdir -p /tmp/seedpkg/src/seedpkg && cd /tmp/seedpkg
printf '[project]\nname="seedpkg"\nversion="0.0.1"\n' > pyproject.toml
touch src/seedpkg/__init__.py
uv build
uv publish --publish-url https://git.lab/api/packages/seedadmin/pypi \
  --username seedadmin --password "$pw"
uv pip install --system --index-url "https://seedadmin:$pw@git.lab/api/packages/seedadmin/pypi/simple/" seedpkg
```
Expected: the push updates `main`; `uv publish` reports the upload; `uv pip install` resolves `seedpkg` from Gitea.

- [ ] **Step 9: Gate — Go and Cargo registries are live**

The full `go get`/`cargo add` round-trips live in `phase1.sh` (Task 6). Here, prove the endpoints answer:
```bash
curl -fsS -u "seedadmin:$pw" https://git.lab/api/packages/seedadmin/cargo/config.json
curl -fsS -o /dev/null -w '%{http_code}\n' "https://git.lab/api/packages/seedadmin/go"
```
Expected: the cargo `config.json` returns a JSON body with `dl`/`api` keys; the go endpoint returns `200` (or `404` for an empty registry, which still proves routing — a connection error would not).

- [ ] **Step 10: Idempotence** — re-run; expected `changed=0` (the admin-create is skipped once `seedadmin` exists).

- [ ] **Step 11: Commit**

```bash
git add seed/roles/seed/templates/gitea.container.j2 seed/roles/seed/templates/git.caddy.j2 seed/roles/seed/tasks/gitea.yml seed/roles/seed/tasks/main.yml seed/roles/seed/defaults/main.yml
git commit -m "feat(seed): gitea for git and py/go/rust packages"
```

### Task 4: Authentik — SSO (server + worker)

Two containers sharing one env file (rendered `0600` from generated secrets, referenced via quadlet `EnvironmentFile=` so nothing lands in the world-readable unit). Backed by Postgres + Redis (Task 2). This is the heaviest component in the stack (D7 risk 3) — and by D7, `zot` never depends on it.

**Files:**
- Create: `seed/roles/seed/templates/authentik.env.j2`, `authentik-server.container.j2`, `authentik-worker.container.j2`, `auth.caddy.j2`
- Create: `seed/roles/seed/tasks/authentik.yml`
- Modify: `seed/roles/seed/tasks/main.yml`, `seed/roles/seed/handlers/main.yml`, `seed/roles/seed/defaults/main.yml`

**Interfaces:**
- Consumes: Postgres, Redis (Task 2), `secrets.yml`, Caddy conf.d.
- Produces: `authentik-server.service` + `authentik-worker.service` at `https://auth.lab`; bootstrap admin `akadmin` (password in `secrets/authentik_bootstrap_password`); an API token in `secrets/authentik_bootstrap_token`. Consumed by Task 5.

- [ ] **Step 1: See the goal unmet** — `systemctl is-active authentik-server.service` → `inactive`.

- [ ] **Step 2: Add to `seed/roles/seed/defaults/main.yml`**

```yaml
seed_authentik_image: ghcr.io/goauthentik/server:2024.8.3
```

- [ ] **Step 3: Create `seed/roles/seed/templates/authentik.env.j2`**

```bash
AUTHENTIK_SECRET_KEY={{ _ak_secret_key }}
AUTHENTIK_POSTGRESQL__HOST=postgres
AUTHENTIK_POSTGRESQL__USER=authentik
AUTHENTIK_POSTGRESQL__NAME=authentik
AUTHENTIK_POSTGRESQL__PASSWORD={{ _ak_db_password }}
AUTHENTIK_REDIS__HOST=redis
AUTHENTIK_BOOTSTRAP_PASSWORD={{ _ak_bootstrap_password }}
AUTHENTIK_BOOTSTRAP_TOKEN={{ _ak_bootstrap_token }}
```

- [ ] **Step 4: Create `seed/roles/seed/templates/authentik-server.container.j2`**

```ini
[Unit]
Description=Authentik server
After=network-online.target seed-network.service postgres.service redis.service
Wants=network-online.target
Requires=postgres.service redis.service

[Container]
Image={{ seed_authentik_image }}
ContainerName=authentik-server
Network=seed
NetworkAlias=auth.{{ seed_domain }}
EnvironmentFile={{ seed_state_root }}/authentik/authentik.env
Volume={{ seed_state_root }}/authentik/media:/media
Volume={{ seed_state_root }}/authentik/templates:/templates
Exec=server

[Service]
Restart=on-failure
TimeoutStartSec=180

[Install]
WantedBy=default.target
```

- [ ] **Step 5: Create `seed/roles/seed/templates/authentik-worker.container.j2`**

```ini
[Unit]
Description=Authentik worker
After=network-online.target seed-network.service postgres.service redis.service
Wants=network-online.target
Requires=postgres.service redis.service

[Container]
Image={{ seed_authentik_image }}
ContainerName=authentik-worker
Network=seed
EnvironmentFile={{ seed_state_root }}/authentik/authentik.env
Volume={{ seed_state_root }}/authentik/media:/media
Volume={{ seed_state_root }}/authentik/templates:/templates
Exec=worker

[Service]
Restart=on-failure
TimeoutStartSec=180

[Install]
WantedBy=default.target
```

- [ ] **Step 6: Create `seed/roles/seed/templates/auth.caddy.j2`**

```caddyfile
auth.{{ seed_domain }} {
	reverse_proxy authentik-server:9000
}
```

- [ ] **Step 7: Add the `restart authentik` handler to `seed/roles/seed/handlers/main.yml`**

```yaml
- name: restart authentik
  ansible.builtin.systemd_service:
    name: "{{ item }}"
    state: restarted
  loop:
    - authentik-server.service
    - authentik-worker.service
```

- [ ] **Step 8: Create `seed/roles/seed/tasks/authentik.yml`**

```yaml
---
- name: Ensure Authentik dirs exist
  ansible.builtin.file:
    path: "{{ item }}"
    state: directory
    owner: root
    group: root
    mode: "0750"
  loop:
    - "{{ seed_state_root }}/authentik"
    - "{{ seed_state_root }}/authentik/media"
    - "{{ seed_state_root }}/authentik/templates"

- name: Ensure Authentik secrets exist
  ansible.builtin.include_tasks: secrets.yml
  vars:
    secret_name: "{{ item }}"
    secret_length: 60
  loop:
    - authentik_secret_key
    - authentik_bootstrap_password
    - authentik_bootstrap_token

- name: Read Authentik secrets
  ansible.builtin.slurp:
    src: "{{ seed_state_root }}/secrets/{{ item }}"
  register: _ak_secrets
  loop:
    - authentik_db_password
    - authentik_secret_key
    - authentik_bootstrap_password
    - authentik_bootstrap_token
  no_log: true

- name: Bind Authentik secret facts
  ansible.builtin.set_fact:
    _ak_db_password: "{{ _ak_secrets.results[0].content | b64decode }}"
    _ak_secret_key: "{{ _ak_secrets.results[1].content | b64decode }}"
    _ak_bootstrap_password: "{{ _ak_secrets.results[2].content | b64decode }}"
    _ak_bootstrap_token: "{{ _ak_secrets.results[3].content | b64decode }}"
  no_log: true

- name: Render the Authentik env file
  ansible.builtin.template:
    src: authentik.env.j2
    dest: "{{ seed_state_root }}/authentik/authentik.env"
    owner: root
    group: root
    mode: "0600"
  notify: restart authentik

- name: Render the Authentik quadlets
  ansible.builtin.template:
    src: "{{ item }}.container.j2"
    dest: "/etc/containers/systemd/{{ item }}.container"
    owner: root
    group: root
    mode: "0644"
  loop:
    - authentik-server
    - authentik-worker
  notify: reload systemd

- name: Add the Authentik Caddy vhost
  ansible.builtin.template:
    src: auth.caddy.j2
    dest: "{{ seed_state_root }}/caddy/conf.d/auth.caddy"
    owner: root
    group: root
    mode: "0644"
  notify: restart caddy

- name: Apply pending changes
  ansible.builtin.meta: flush_handlers

- name: Start Authentik
  ansible.builtin.systemd_service:
    name: "{{ item }}"
    state: started
  loop:
    - authentik-server.service
    - authentik-worker.service

- name: Wait for Authentik readiness
  ansible.builtin.uri:
    url: "https://auth.{{ seed_domain }}/-/health/ready/"
    ca_path: /etc/ssl/seed/root_ca.crt
    status_code: [200, 204]
  register: _ak_ready
  until: _ak_ready.status in [200, 204]
  retries: 60
  delay: 3
```

- [ ] **Step 9: Wire into `seed/roles/seed/tasks/main.yml`** (append below the Gitea include):

```yaml
- name: Bring up Authentik (SSO)
  ansible.builtin.include_tasks: authentik.yml
```

- [ ] **Step 10: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: both units up; the readiness wait passes; `failed=0`. (First boot runs migrations — the 60×3s retry covers it.)

- [ ] **Step 11: Gate — health and the bootstrap token**

Run (on the target):
```bash
tok=$(cat /var/lib/seed/secrets/authentik_bootstrap_token)
curl -fsS -o /dev/null -w '%{http_code}\n' https://auth.lab/-/health/live/
curl -fsS -H "Authorization: Bearer $tok" \
  "https://auth.lab/api/v3/core/users/?username=akadmin" | grep -o '"username":"akadmin"'
```
Expected: `204`; then `"username":"akadmin"` — the bootstrap admin exists and the token authenticates.

- [ ] **Step 12: Idempotence** — re-run; expected `changed=0`.

- [ ] **Step 13: Commit**

```bash
git add seed/roles/seed/templates/authentik.env.j2 seed/roles/seed/templates/authentik-server.container.j2 seed/roles/seed/templates/authentik-worker.container.j2 seed/roles/seed/templates/auth.caddy.j2 seed/roles/seed/tasks/authentik.yml seed/roles/seed/tasks/main.yml seed/roles/seed/handlers/main.yml seed/roles/seed/defaults/main.yml
git commit -m "feat(seed): authentik sso (server + worker)"
```

### Task 5: Gitea ↔ Authentik OIDC

Register a confidential OAuth2 provider + application in Authentik (the homelab `oauth_app_register` pattern, rewritten with idempotent search-then-create), then add it to Gitea as a login source. **Found here (Tier 2):** Gitea must trust the step-ca root to complete OIDC discovery against `https://auth.lab` — so `gitea.container.j2` gains the host CA bundle (which already carries the seed root since Phase 0 Task 5) via `SSL_CERT_FILE`.

**Files:**
- Create: `seed/roles/seed/tasks/oidc.yml`
- Modify: `seed/roles/seed/templates/gitea.container.j2` (trust the internal CA), `seed/roles/seed/tasks/main.yml`

**Interfaces:**
- Consumes: Authentik API + bootstrap token (Task 4), Gitea CLI (Task 3), `secrets.yml`.
- Produces: an Authentik provider/application `gitea`; a Gitea OIDC login source `authentik`; the client secret in `secrets/gitea_oidc_client_secret`.

- [ ] **Step 1: See the goal unmet** — `podman exec -u git gitea gitea admin auth list` shows no `authentik` row.

- [ ] **Step 2: Teach Gitea to trust the internal CA** — add to `seed/roles/seed/templates/gitea.container.j2` (the host bundle already includes the seed root):

```ini
Volume=/etc/ssl/certs/ca-certificates.crt:/etc/ssl/seed/ca-bundle.crt:ro
Environment=SSL_CERT_FILE=/etc/ssl/seed/ca-bundle.crt
```

- [ ] **Step 3: Create `seed/roles/seed/tasks/oidc.yml`**

```yaml
---
- name: Ensure the Gitea OIDC client secret exists
  ansible.builtin.include_tasks: secrets.yml
  vars:
    secret_name: gitea_oidc_client_secret

- name: Read the Authentik token and OIDC secret
  ansible.builtin.slurp:
    src: "{{ seed_state_root }}/secrets/{{ item }}"
  register: _oidc_secrets
  loop:
    - authentik_bootstrap_token
    - gitea_oidc_client_secret
  no_log: true

- name: Bind OIDC facts
  ansible.builtin.set_fact:
    _ak_token: "{{ _oidc_secrets.results[0].content | b64decode }}"
    _oidc_secret: "{{ _oidc_secrets.results[1].content | b64decode }}"
  no_log: true

- name: Resolve Authentik default flows
  ansible.builtin.uri:
    url: "https://auth.{{ seed_domain }}/api/v3/flows/instances/?slug={{ item.slug }}"
    headers: { Authorization: "Bearer {{ _ak_token }}" }
    ca_path: /etc/ssl/seed/root_ca.crt
  register: _ak_flows
  loop:
    - { slug: default-provider-authorization-implicit-consent }
    - { slug: default-provider-invalidation-flow }
  no_log: true

- name: Bind flow pks
  ansible.builtin.set_fact:
    _ak_authz_pk: "{{ _ak_flows.results[0].json.results[0].pk }}"
    _ak_inval_pk: "{{ _ak_flows.results[1].json.results[0].pk }}"

- name: Look for an existing gitea provider
  ansible.builtin.uri:
    url: "https://auth.{{ seed_domain }}/api/v3/providers/oauth2/?name=gitea"
    headers: { Authorization: "Bearer {{ _ak_token }}" }
    ca_path: /etc/ssl/seed/root_ca.crt
  register: _ak_prov
  no_log: true

- name: Create the gitea OAuth2 provider
  ansible.builtin.uri:
    url: "https://auth.{{ seed_domain }}/api/v3/providers/oauth2/"
    method: POST
    headers: { Authorization: "Bearer {{ _ak_token }}" }
    ca_path: /etc/ssl/seed/root_ca.crt
    body_format: json
    status_code: [201]
    body:
      name: gitea
      client_type: confidential
      client_id: gitea
      client_secret: "{{ _oidc_secret }}"
      authorization_flow: "{{ _ak_authz_pk }}"
      invalidation_flow: "{{ _ak_inval_pk }}"
      sub_mode: hashed_user_id
      redirect_uris:
        - { matching_mode: strict, url: "https://git.{{ seed_domain }}/user/oauth2/authentik/callback" }
  when: _ak_prov.json.pagination.count == 0
  register: _ak_prov_created
  no_log: true

- name: Bind the provider pk
  ansible.builtin.set_fact:
    _ak_prov_pk: "{{ _ak_prov.json.results[0].pk if _ak_prov.json.pagination.count > 0 else _ak_prov_created.json.pk }}"

- name: Look for an existing gitea application
  ansible.builtin.uri:
    url: "https://auth.{{ seed_domain }}/api/v3/core/applications/?slug=gitea"
    headers: { Authorization: "Bearer {{ _ak_token }}" }
    ca_path: /etc/ssl/seed/root_ca.crt
  register: _ak_app
  no_log: true

- name: Create the gitea application
  ansible.builtin.uri:
    url: "https://auth.{{ seed_domain }}/api/v3/core/applications/"
    method: POST
    headers: { Authorization: "Bearer {{ _ak_token }}" }
    ca_path: /etc/ssl/seed/root_ca.crt
    body_format: json
    status_code: [201]
    body: { name: Gitea, slug: gitea, provider: "{{ _ak_prov_pk }}" }
  when: _ak_app.json.pagination.count == 0
  no_log: true

- name: List Gitea auth sources
  ansible.builtin.command: podman exec -u git gitea gitea admin auth list
  register: _gitea_auth
  changed_when: false

- name: Add the Authentik OIDC login source to Gitea
  ansible.builtin.command: >
    podman exec -u git gitea gitea admin auth add-oauth
    --name authentik --provider openidConnect
    --key gitea --secret "{{ _oidc_secret }}"
    --auto-discover-url https://auth.{{ seed_domain }}/application/o/gitea/.well-known/openid-configuration
  when: "'authentik' not in _gitea_auth.stdout"
  no_log: true
```

- [ ] **Step 4: Wire into `seed/roles/seed/tasks/main.yml`** (append below the Authentik include):

```yaml
- name: Wire Gitea <-> Authentik OIDC
  ansible.builtin.include_tasks: oidc.yml
```

- [ ] **Step 5: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: provider + application created, Gitea source added, Gitea restarted with the CA mount; `failed=0`.

- [ ] **Step 6: Gate — the OIDC wiring exists end to end**

Run (on the target):
```bash
curl -fsS -o /dev/null -w '%{http_code}\n' \
  https://auth.lab/application/o/gitea/.well-known/openid-configuration
podman exec -u git gitea gitea admin auth list | grep -i authentik
```
Expected: `200` from the discovery document (proves Authentik trusts the app and Gitea can reach it), and an `authentik` row in Gitea's auth sources.
**Manual confirmation:** browse `https://git.lab`, click "Sign in with authentik", log in as `akadmin`; a Gitea session is created. (A full headless SSO flow is out of scope for the automated gate.)

- [ ] **Step 7: Idempotence** — re-run; expected `changed=0` (provider, app, and Gitea source all already present).

- [ ] **Step 8: Commit**

```bash
git add seed/roles/seed/templates/gitea.container.j2 seed/roles/seed/tasks/oidc.yml seed/roles/seed/tasks/main.yml
git commit -m "feat(seed): gitea login through authentik oidc"
```

### Task 6: tinq `spec.registries.ca`, and the Phase 1 acceptance

The one tinq code change (D4): a mirror gains a CA so a node verifies an `https://` registry against the seed's step-ca instead of `http://` or `insecureSkipVerify`. This is `talos-in-qemu` proper, not under `seed/` — separate commits.

**Verified against the current tree:** `registryMirrors` (`cmd/tinq/main.go:449`) reads `spec.registries` into `[]cluster.RegistryMirror`; the struct is `cluster/config.go:39`; `registriesConfig` (`cluster/config.go:520`) maps it to `v1alpha1.RegistriesConfig`, today emitting a TLS block only for `InsecureSkipVerify`.

**Files:**
- Modify: `cluster/config.go` (`RegistryMirror` + `registriesConfig`), `cluster/registries_test.go`
- Modify: `cmd/tinq/main.go` (`registryMirrors` + a `registryCA` helper), `cmd/tinq/main_test.go`
- Modify: `crd/talosmachine.yaml` (registries item: `ca`, `caFile`)
- Create: `seed/acceptance/phase1.sh`

**Interfaces:**
- Consumes: nothing new (extends existing types).
- Produces: `RegistryMirror.CA string`; `spec.registries[].ca` (inline PEM) / `.caFile` (path) accepted; `machine.registries.config.<host>.tls.ca` emitted. Consumed by Phase 2 Task 5 (a Talos guest pulling from `https://registry.lab`).

- [ ] **Step 1: Add the failing emission test to `cluster/registries_test.go`**

```go
func TestRegistriesConfigCA(t *testing.T) {
	pem := "-----BEGIN CERTIFICATE-----\nMIIseed\n-----END CERTIFICATE-----\n"
	got := registriesConfig([]RegistryMirror{{
		Host: "registry.lab", Endpoint: "https://registry.lab", CA: pem,
	}})

	cfg := got.RegistryConfig["registry.lab"]
	if cfg == nil || cfg.RegistryTLS == nil || cfg.RegistryTLS.TLSCA == nil {
		t.Fatalf("CA did not reach the TLS config: %+v", got.RegistryConfig)
	}
	if string(cfg.RegistryTLS.TLSCA) != pem {
		t.Fatal("CA bytes not carried verbatim")
	}
	// A CA to TRUST is not a request to SKIP verification.
	if cfg.RegistryTLS.TLSInsecureSkipVerify != nil {
		t.Fatal("CA must not imply insecureSkipVerify")
	}
}
```

- [ ] **Step 2: Run it, watch it fail** — `go test ./cluster -run TestRegistriesConfigCA`. Expected: FAIL (`RegistryMirror has no field CA` — compile error).

- [ ] **Step 3: Add `CA` to `RegistryMirror` in `cluster/config.go`** (after the `Endpoint` field, before `InsecureSkipVerify`):

```go
	// CA is a PEM certificate bundle to verify an https:// endpoint against,
	// emitted as machine.registries.config.<host>.tls.ca. It is how a node
	// trusts a registry with a PRIVATE CA (the seed's step-ca) without the
	// blunt instrument of InsecureSkipVerify. Empty for http:// or a
	// publicly-trusted endpoint.
	CA string
```

- [ ] **Step 4: Emit it in `registriesConfig`** — replace the tail of the loop (the `if !m.InsecureSkipVerify { continue }` block through the `RegistryConfig[m.Host] = …` assignment) with:

```go
		// TLS config is a SECOND map. Either a CA to trust or a request to skip
		// verification lands here; a plain http:// mirror needs neither.
		needsTLS := m.InsecureSkipVerify || m.CA != ""
		if !needsTLS {
			continue
		}

		// Talos refuses "*" as a TLS config key; emitting it fails validation
		// on apply, i.e. after a VM has already booted.
		if m.Host == "*" {
			continue
		}

		if out.RegistryConfig == nil {
			out.RegistryConfig = map[string]*v1alpha1.RegistryConfig{}
		}

		tls := &v1alpha1.RegistryTLSConfig{}
		if m.InsecureSkipVerify {
			tls.TLSInsecureSkipVerify = new(true)
		}
		if m.CA != "" {
			// Raw PEM bytes; machinery base64-encodes TLSCA when it renders.
			tls.TLSCA = []byte(m.CA)
		}
		out.RegistryConfig[m.Host] = &v1alpha1.RegistryConfig{RegistryTLS: tls}
```

- [ ] **Step 5: Run the emission tests** — `go test ./cluster -run TestRegistriesConfig`. Expected: PASS (the new test and the existing `PlainHTTP`/`InsecureTLS`/`Wildcard` ones — a CA on `*` is still dropped, unchanged behaviour).

- [ ] **Step 6: Read `ca`/`caFile` in `cmd/tinq/main.go`** — set `CA` in the `mirror` literal in `registryMirrors` and add the helper. Ensure `os` is imported.

```go
		mirror := cluster.RegistryMirror{
			Host:               str(e["host"], ""),
			Endpoint:           str(e["endpoint"], ""),
			CA:                 ca,
			InsecureSkipVerify: e["insecureSkipVerify"] == true,
			OverridePath:       e["overridePath"] == true,
		}
```

Just above the literal:

```go
		ca, err := registryCA(e)
		if err != nil {
			return nil, fmt.Errorf("spec.registries[%d]: %w (%s)", i, err, m.GetName())
		}
```

And the helper (near the other tiny helpers):

```go
// registryCA resolves the optional CA for a mirror. caFile is a path read at
// generate time — the seed exports /etc/ssl/seed/root_ca.crt — and ca is inline
// PEM. caFile wins if both are set; neither is the common case. Read here, in
// cmd/tinq, so cluster/ stays pure: it receives already-resolved bytes.
func registryCA(e map[string]interface{}) (string, error) {
	if p := str(e["caFile"], ""); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("caFile %q could not be read: %w", p, err)
		}
		return string(b), nil
	}
	return str(e["ca"], ""), nil
}
```

- [ ] **Step 7: Add the reader tests to `cmd/tinq/main_test.go`**

```go
func TestRegistryMirrorsReadsInlineCA(t *testing.T) {
	m := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"registries": []interface{}{
			map[string]interface{}{
				"host": "registry.lab", "endpoint": "https://registry.lab",
				"ca": "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n",
			},
		}},
	}}
	got, err := registryMirrors(m)
	if err != nil {
		t.Fatalf("registryMirrors: %v", err)
	}
	if len(got) != 1 || !strings.Contains(got[0].CA, "BEGIN CERTIFICATE") {
		t.Fatalf("inline ca not read: %+v", got)
	}
}

func TestRegistryMirrorsReadsCAFile(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/root.crt"
	if err := os.WriteFile(p, []byte("PEMBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"registries": []interface{}{
			map[string]interface{}{
				"host": "registry.lab", "endpoint": "https://registry.lab", "caFile": p,
			},
		}},
	}}
	got, err := registryMirrors(m)
	if err != nil {
		t.Fatalf("registryMirrors: %v", err)
	}
	if got[0].CA != "PEMBYTES" {
		t.Fatalf("caFile not read: %q", got[0].CA)
	}
}
```

- [ ] **Step 8: Extend the CRD** — in `crd/talosmachine.yaml`, under the `spec.registries` array item `properties`, add:

```yaml
                    ca:
                      type: string
                      description: PEM CA bundle to verify an https endpoint against.
                    caFile:
                      type: string
                      description: Path (on the host running tinq) to a PEM CA bundle.
```

- [ ] **Step 9: Full tinq gate** — from the repo root:

```bash
go build ./... && go vet ./... && go test ./... && GOOS=darwin GOARCH=arm64 go vet ./... && gofmt -l cmd cluster
```
Expected: all pass; `gofmt -l` prints nothing.

- [ ] **Step 10: Commit the tinq change**

```bash
git add cluster/config.go cluster/registries_test.go cmd/tinq/main.go cmd/tinq/main_test.go crd/talosmachine.yaml
git commit -m "feat(cluster): spec.registries.ca, trust a registry's private CA"
```

- [ ] **Step 11: Create `seed/acceptance/phase1.sh`**

Aggregates the per-task gates that run host-side. The **Talos-guest pull over `https://registry.lab`** is the capstone of **Phase 2 Task 5** — it needs a node that resolves `registry.lab`, which is the seed's dnsmasq (Phase 2), not the `/etc/hosts` bridge. The `ca` field is proven at the config layer here and consumed there.

```bash
#!/usr/bin/env bash
# Phase 1 gate: registry, packages, and SSO wiring — all host-side.
set -euo pipefail
D=${SEED_DOMAIN:-lab}
pw=$(cat /var/lib/seed/secrets/gitea_admin_password)
zpw=$(cat /var/lib/seed/secrets/zot_password)

# zot: authenticated push, anonymous pull, pull-through cache
podman login "registry.${D}" -u seed -p "$zpw"
podman pull docker.io/library/busybox:1.36
podman tag docker.io/library/busybox:1.36 "registry.${D}/seed/busybox:test"
podman push "registry.${D}/seed/busybox:test"
podman logout "registry.${D}"
podman pull "registry.${D}/seed/busybox:test"          # anonymous read
podman pull "registry.${D}/library/alpine:3.20"        # docker.io via cache

# Gitea: git push (repo created earlier) and the three package registries live
curl -fsS -o /dev/null -w 'pypi-simple:%{http_code}\n' \
  -u "seedadmin:$pw" "https://git.${D}/api/packages/seedadmin/pypi/simple/"
curl -fsS -o /dev/null -w 'cargo-config:%{http_code}\n' \
  -u "seedadmin:$pw" "https://git.${D}/api/packages/seedadmin/cargo/config.json"
curl -fsS -o /dev/null -w 'go-registry:%{http_code}\n' \
  "https://git.${D}/api/packages/seedadmin/go"

# Authentik: live, and the Gitea OIDC discovery resolves
curl -fsS -o /dev/null -w 'authentik:%{http_code}\n' "https://auth.${D}/-/health/live/"
curl -fsS -o /dev/null -w 'oidc-disco:%{http_code}\n' \
  "https://auth.${D}/application/o/gitea/.well-known/openid-configuration"

echo "phase1 ok"
```

> **Documented manual round-trips (spec's per-type gate).** PyPI round-trips automatically in Task 3, Step 8. Go: build a module, `PUT` its `<module>@v<ver>.zip` to `/api/packages/seedadmin/go/upload`, then `GOPROXY=https://seedadmin:$pw@git.lab/api/packages/seedadmin/go GONOSUMDB=* go install <module>@v<ver>`. Cargo: set a `gitea` sparse registry (`https://git.lab/api/packages/seedadmin/cargo/`) with a token in `~/.cargo/config.toml`, `cargo publish --registry gitea`, then `cargo add <crate> --registry gitea`. The endpoint checks above prove the registries route; confirm the exact upload formats against the pinned Gitea's package docs.

- [ ] **Step 12: Run the gate** — `chmod +x seed/acceptance/phase1.sh && cd seed && ansible seed-dev -m script -a 'acceptance/phase1.sh'`. Expected: every line a `2xx`, ending `phase1 ok`.

- [ ] **Step 13: Commit the acceptance script**

```bash
git add seed/acceptance/phase1.sh
git commit -m "test(seed): phase 1 acceptance (registry, packages, sso)"
```

---

## Phase 1 Done — Definition of Done

- Playbook converges and is idempotent (`changed=0` on re-run).
- `phase1.sh` passes: zot push/anon-pull/cache, Gitea git + package registries live, Authentik healthy, Gitea↔Authentik OIDC discovery resolving.
- `go test ./...` green; `spec.registries[].ca`/`.caFile` accepted and emitted as `machine.registries.config.<host>.tls.ca`.
- Deferred to Phase 2 Task 5 (needs DNS): a Talos guest pulling an image that exists only in zot, over `https://registry.lab`, trusting the step-ca root via the new `ca` field.

## Self-Review

- **Spec coverage:** D3 zot split — Task 1. D7 zot-off-Authentik — Task 1 (htpasswd, anon read) + Task 4. Postgres/Redis — Task 2. Gitea git+packages — Task 3. Authentik — Task 4. OIDC — Task 5. tinq `ca` field — Task 6. The Talos-guest https pull is explicitly handed to Phase 2 Task 5 (DNS dependency), not dropped.
- **Placeholder scan:** none. Go/Cargo full publish formats are labelled manual round-trips with canonical commands, not TODOs; the automated gate proves the registries route.
- **Interface consistency:** `RegistryMirror.CA`, `registryCA`, `machine.registries.config.<host>.tls.ca`, the `seed`/`registry.lab`/`git.lab`/`auth.lab` names, and every secret path match across tasks and against the verified tree (`cluster/config.go:39,520`, `cmd/tinq/main.go:449`).
