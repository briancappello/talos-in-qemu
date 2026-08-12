# Seed Phase 2 — Day-0 Boot Plane Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a bare-metal node boot Talos into maintenance mode over the network — no USB — so `tinq adopt` takes it to Ready, with the installer pulled from `zot` over verified HTTPS. dnsmasq (proxy-DHCP + TFTP + DNS), an HTTP asset server, iPXE, and chrony.

**Architecture:** dnsmasq and chrony are the network-core services and run as **host packages** (not quadlets) because they need host networking and privileged/raw UDP — the same reasoning that made chunk-0's registry a host binary. Everything application-level stays a quadlet. dnsmasq proxy-DHCP appends boot info to the LAN's existing DHCP; the mode is a single knob (`proxy` → `authoritative`) so a VLAN is a later config change (spec D5, Risk 1). The maintenance boot pulls kernel/initramfs from the asset server; the on-disk install pulls the `installer` image from `zot` (Phase 1).

**Tech Stack:** Everything from Phases 0–1, plus host `dnsmasq` and `chrony`, iPXE binaries from `boot.ipxe.org`, and Talos `metal` kernel/initramfs + `installer` for the pinned version.

## Global Constraints

Phases 0–1 constraints hold. Additional:

- **Netboot targets amd64 bare metal only.** Dev VMs boot through tinq/qemu and never PXE.
- **Network-core services (dnsmasq, chrony) are host packages, not quadlets** — host net + privileged UDP (53/67/69/123). They bind the seed's LAN interface explicitly (`bind-interfaces`) so they never collide with `systemd-resolved` on loopback.
- **proxy-DHCP is the default and MUST NOT assign addresses.** The LAN router keeps leasing; the seed only adds boot info. `seed_dhcp_mode` is the knob; `authoritative` is opt-in for a dedicated segment.
- **dnsmasq serves `*.lab` to the network (the nodes).** The seed host itself keeps the Phase-0/1 `/etc/hosts` entries — harmless, and avoids resolv.conf surgery.
- **Assets are mirrored for a pinned Talos version**, not manufactured; `image-factory` and `matchbox` stay out of scope.
- **Phase gate:** `seed/acceptance/phase2.sh` — a qemu PXE guest reaches Talos maintenance off the seed and `tinq adopt` drives it to Ready, pulling the installer from `zot`; then one 5900X hardware run.

## File Structure

Extends the Phase 0–1 role. New files:

```
seed/roles/seed/
  templates/
    dnsmasq.conf.j2       # /etc/dnsmasq.d/seed.conf: DNS + proxy-DHCP(knob) + TFTP + NTP opt
    boot.caddy.j2         # boot.lab -> file_server over the asset dir
    boot.ipxe.j2          # chainloaded iPXE script: boot Talos maintenance
    chrony.conf.j2
  tasks/
    dnsmasq.yml           # host package
    assets.yml            # fetch vmlinuz/initramfs; push installer into zot
    ipxe.yml              # fetch undionly.kpxe + ipxe.efi into the tftp root
    chrony.yml            # host package
  files/
    fetch-talos-assets.sh # pinned-version fetcher (idempotent, checksum-guarded)
seed/acceptance/phase2.sh
```

Modified: `tasks/main.yml`, `defaults/main.yml`, `inventory/group_vars/seed.yml` (the seed's LAN facts + DHCP knob).

---

### Task 1: dnsmasq — DNS, proxy-DHCP (knob), and TFTP

A host package, bound to the seed's LAN interface so it coexists with `systemd-resolved` and the LAN router. It resolves `*.lab` for the nodes and, in proxy mode, hands PXE clients an arch-matched boot file and chainloads iPXE to the HTTP script.

**Files:**
- Create: `seed/roles/seed/templates/dnsmasq.conf.j2`, `seed/roles/seed/tasks/dnsmasq.yml`
- Modify: `seed/roles/seed/handlers/main.yml` (`restart dnsmasq`), `seed/roles/seed/tasks/main.yml`, `seed/inventory/group_vars/seed.yml`

**Interfaces:**
- Consumes: the seed's LAN facts.
- Produces: `dnsmasq` resolving `*.lab` → `seed_lan_address`; proxy-DHCP offering `undionly.kpxe`/`ipxe.efi` (by DHCP option 93) and chainloading `https://boot.lab/boot.ipxe`; DHCP option 42 → the seed as NTP. The `/var/lib/seed/tftp` root (filled by Task 3).

- [ ] **Step 1: See the goal unmet** — `dig +short @{{ seed_lan_address }} registry.lab` → empty / connection refused.

- [ ] **Step 2: Add the seed's LAN facts to `seed/inventory/group_vars/seed.yml`** (per-environment; comments say what to set):

```yaml
seed_lan_iface: eth0            # the seed's LAN interface
seed_lan_address: 192.168.1.10  # the seed's LAN IP (what *.lab resolves to)
seed_lan_subnet: 192.168.1.0    # the LAN network address (/24 assumed below)
seed_dhcp_mode: proxy           # proxy (coexist w/ router) | authoritative (seed owns a segment)
seed_dhcp_range: 192.168.50.50,192.168.50.150,12h   # only used when mode=authoritative
```

- [ ] **Step 3: Create `seed/roles/seed/templates/dnsmasq.conf.j2`**

```jinja
interface={{ seed_lan_iface }}
listen-address={{ seed_lan_address }}
bind-interfaces
domain={{ seed_domain }}
local=/{{ seed_domain }}/
{% for n in ['ca', 'registry', 'git', 'auth', 'boot', 'time', 's3'] %}
address=/{{ n }}.{{ seed_domain }}/{{ seed_lan_address }}
{% endfor %}
server=1.1.1.1

enable-tftp
tftp-root=/var/lib/seed/tftp

{% if seed_dhcp_mode == 'proxy' %}
dhcp-range={{ seed_lan_subnet }},proxy
{% else %}
dhcp-range={{ seed_dhcp_range }}
{% endif %}

# arch-matched boot file (DHCP option 93): BIOS vs UEFI x86-64
dhcp-match=set:bios,option:client-arch,0
dhcp-match=set:efi64,option:client-arch,7
dhcp-match=set:efi64,option:client-arch,9
dhcp-boot=tag:bios,undionly.kpxe
dhcp-boot=tag:efi64,ipxe.efi

# once iPXE itself is running (it sends option 175), chainload the HTTP script.
# HTTP, not HTTPS: stock iPXE does not trust the seed's private CA (that would
# need a custom iPXE build), and the maintenance boot is on the trusted
# provisioning wire — the same trust window adopt already documents. The pull
# that matters, the installer image, is HTTPS via zot (Phase 1 `ca` field).
dhcp-match=set:ipxe,175
dhcp-boot=tag:ipxe,http://boot.{{ seed_domain }}/boot.ipxe

# NTP (option 42) -> the seed
dhcp-option=option:ntp-server,{{ seed_lan_address }}
```

- [ ] **Step 4: Add the `restart dnsmasq` handler to `seed/roles/seed/handlers/main.yml`**

```yaml
- name: restart dnsmasq
  ansible.builtin.systemd_service:
    name: dnsmasq
    state: restarted
```

- [ ] **Step 5: Create `seed/roles/seed/tasks/dnsmasq.yml`**

```yaml
---
- name: Install dnsmasq
  ansible.builtin.package:
    name: dnsmasq
    state: present

- name: Ensure the TFTP root exists
  ansible.builtin.file:
    path: /var/lib/seed/tftp
    state: directory
    owner: root
    group: root
    mode: "0755"

- name: Render the seed dnsmasq config
  ansible.builtin.template:
    src: dnsmasq.conf.j2
    dest: /etc/dnsmasq.d/seed.conf
    owner: root
    group: root
    mode: "0644"
    validate: dnsmasq --test --conf-file=%s
  notify: restart dnsmasq

- name: Enable and start dnsmasq
  ansible.builtin.systemd_service:
    name: dnsmasq
    enabled: true
    state: started
```

- [ ] **Step 6: Wire into `seed/roles/seed/tasks/main.yml`** (append at the end):

```yaml
- name: Bring up dnsmasq (DNS + proxy-DHCP + TFTP)
  ansible.builtin.include_tasks: dnsmasq.yml
```

- [ ] **Step 7: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: dnsmasq installed; config passes `--test`; service active.

- [ ] **Step 8: Gate — DNS answers for the seed names**

Run (on the target):
```bash
systemctl is-active dnsmasq
for n in registry git auth boot; do dig +short @{{ seed_lan_address }} "$n.lab"; done
```
Expected: `active`; four lines each printing `{{ seed_lan_address }}`.

- [ ] **Step 9: Idempotence** — re-run; expected `changed=0`.

- [ ] **Step 10: Commit**

```bash
git add seed/roles/seed/templates/dnsmasq.conf.j2 seed/roles/seed/tasks/dnsmasq.yml seed/roles/seed/handlers/main.yml seed/roles/seed/tasks/main.yml seed/inventory/group_vars/seed.yml
git commit -m "feat(seed): dnsmasq dns, proxy-dhcp and tftp for netboot"
```

### Task 2: Talos assets and the HTTP boot server

Fetch the pinned Talos `vmlinuz`/`initramfs` (the maintenance boot) into the asset dir served by Caddy over HTTP at `boot.lab`, and **own** the `installer` image in `zot` (a copy, not merely a cache) so the on-disk install pulls from the seed.

**Files:**
- Create: `seed/roles/seed/templates/boot.caddy.j2`, `seed/roles/seed/tasks/assets.yml`
- Modify: `seed/roles/seed/templates/caddy.container.j2` (mount the asset dir), `seed/roles/seed/tasks/main.yml`, `seed/roles/seed/defaults/main.yml`

**Interfaces:**
- Consumes: `zot` + its push creds (Phase 1), Caddy.
- Produces: `http://boot.lab/{vmlinuz-amd64,initramfs-amd64.xz}`; `registry.lab/siderolabs/installer:{{ seed_talos_version }}` in zot. Consumed by Task 3 (`boot.ipxe`) and Task 5 (install).

- [ ] **Step 1: See the goal unmet** — `curl -sf http://boot.lab/vmlinuz-amd64` → refused.

- [ ] **Step 2: Add to `seed/roles/seed/defaults/main.yml`**

```yaml
seed_talos_version: v1.13.7
seed_talos_arch: amd64
```

- [ ] **Step 3: Create `seed/roles/seed/templates/boot.caddy.j2`** (HTTP — see the Task 1 note):

```caddyfile
http://boot.{{ seed_domain }} {
	root * /srv/boot
	file_server browse
}
```

- [ ] **Step 4: Mount the asset dir into Caddy** — add to `seed/roles/seed/templates/caddy.container.j2`:

```ini
Volume={{ seed_state_root }}/assets:/srv/boot:ro
```

- [ ] **Step 5: Create `seed/roles/seed/tasks/assets.yml`**

```yaml
---
- name: Install skopeo
  ansible.builtin.package:
    name: skopeo
    state: present

- name: Ensure the asset dir exists
  ansible.builtin.file:
    path: "{{ seed_state_root }}/assets"
    state: directory
    owner: root
    group: root
    mode: "0755"

- name: Fetch the Talos maintenance kernel and initramfs
  ansible.builtin.get_url:
    url: "https://github.com/siderolabs/talos/releases/download/{{ seed_talos_version }}/{{ item }}"
    dest: "{{ seed_state_root }}/assets/{{ item }}"
    mode: "0644"
  loop:
    - "vmlinuz-{{ seed_talos_arch }}"
    - "initramfs-{{ seed_talos_arch }}.xz"

- name: Read the zot push password
  ansible.builtin.slurp:
    src: "{{ seed_state_root }}/secrets/zot_password"
  register: _zot_pw_b64
  no_log: true

- name: Is the installer already in zot?
  ansible.builtin.command: >
    skopeo inspect docker://registry.{{ seed_domain }}/siderolabs/installer:{{ seed_talos_version }}
  register: _installer_present
  failed_when: false
  changed_when: false

- name: Own the Talos installer image in zot
  ansible.builtin.command: >
    skopeo copy --retry-times 3
    --dest-creds "{{ seed_registry_user }}:{{ _zot_pw_b64.content | b64decode }}"
    docker://ghcr.io/siderolabs/installer:{{ seed_talos_version }}
    docker://registry.{{ seed_domain }}/siderolabs/installer:{{ seed_talos_version }}
  when: _installer_present.rc != 0
  no_log: true

- name: Add the boot asset vhost
  ansible.builtin.template:
    src: boot.caddy.j2
    dest: "{{ seed_state_root }}/caddy/conf.d/boot.caddy"
    owner: root
    group: root
    mode: "0644"
  notify: restart caddy

- name: Re-render the Caddy quadlet with the asset mount
  ansible.builtin.template:
    src: caddy.container.j2
    dest: /etc/containers/systemd/caddy.container
    owner: root
    group: root
    mode: "0644"
  notify: reload systemd

- name: Apply pending changes
  ansible.builtin.meta: flush_handlers
```

- [ ] **Step 6: Wire into `seed/roles/seed/tasks/main.yml`** (append; before or after dnsmasq is fine — no ordering dependency):

```yaml
- name: Fetch Talos assets and the boot server
  ansible.builtin.include_tasks: assets.yml
```

- [ ] **Step 7: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: assets downloaded; installer copied into zot; Caddy re-rendered.

- [ ] **Step 8: Gate — assets and installer served**

Run (on the target):
```bash
curl -sf -o /dev/null -w 'vmlinuz:%{http_code}\n'   http://boot.lab/vmlinuz-amd64
curl -sf -o /dev/null -w 'initramfs:%{http_code}\n' http://boot.lab/initramfs-amd64.xz
skopeo inspect docker://registry.lab/siderolabs/installer:{{ seed_talos_version }} >/dev/null && echo installer-in-zot
```
Expected: `vmlinuz:200`, `initramfs:200`, `installer-in-zot`.

- [ ] **Step 9: Idempotence** — re-run; expected `changed=0` (get_url skips present files, the installer copy is guarded by the inspect).

- [ ] **Step 10: Commit**

```bash
git add seed/roles/seed/templates/boot.caddy.j2 seed/roles/seed/templates/caddy.container.j2 seed/roles/seed/tasks/assets.yml seed/roles/seed/tasks/main.yml seed/roles/seed/defaults/main.yml
git commit -m "feat(seed): talos assets and the http boot server"
```

### Task 3: iPXE binaries and the boot script

TFTP serves the arch-matched iPXE binary; iPXE chainloads `boot.ipxe` over HTTP, which boots the Talos maintenance kernel with **no** `talos.config` — so the node lands in maintenance mode and waits for `tinq adopt`.

**Files:**
- Create: `seed/roles/seed/templates/boot.ipxe.j2`, `seed/roles/seed/tasks/ipxe.yml`
- Modify: `seed/roles/seed/tasks/main.yml`, `seed/roles/seed/defaults/main.yml`

**Interfaces:**
- Consumes: the TFTP root (Task 1), the asset server + assets (Task 2).
- Produces: `undionly.kpxe` + `ipxe.efi` in TFTP; `http://boot.lab/boot.ipxe` booting Talos maintenance. Consumed by Task 5.

- [ ] **Step 1: See the goal unmet** — `curl -sf http://boot.lab/boot.ipxe` → 404.

- [ ] **Step 2: Add to `seed/roles/seed/defaults/main.yml`**

```yaml
seed_ipxe_efi_url: https://boot.ipxe.org/ipxe.efi
seed_ipxe_bios_url: https://boot.ipxe.org/undionly.kpxe
```

- [ ] **Step 3: Create `seed/roles/seed/templates/boot.ipxe.j2`**

```jinja
#!ipxe
# Boot Talos into MAINTENANCE mode: talos.platform=metal and NO talos.config,
# so the node comes up on its DHCP lease and waits for `tinq adopt`.
set base http://boot.{{ seed_domain }}
kernel ${base}/vmlinuz-{{ seed_talos_arch }} talos.platform=metal console=tty0 console=ttyS0
initrd ${base}/initramfs-{{ seed_talos_arch }}.xz
boot
```

- [ ] **Step 4: Create `seed/roles/seed/tasks/ipxe.yml`**

```yaml
---
- name: Fetch iPXE binaries into the TFTP root
  ansible.builtin.get_url:
    url: "{{ item.url }}"
    dest: "/var/lib/seed/tftp/{{ item.name }}"
    mode: "0644"
  loop:
    - { url: "{{ seed_ipxe_efi_url }}", name: ipxe.efi }
    - { url: "{{ seed_ipxe_bios_url }}", name: undionly.kpxe }

- name: Render the iPXE boot script
  ansible.builtin.template:
    src: boot.ipxe.j2
    dest: "{{ seed_state_root }}/assets/boot.ipxe"
    owner: root
    group: root
    mode: "0644"
```

- [ ] **Step 5: Wire into `seed/roles/seed/tasks/main.yml`** (append after the assets include):

```yaml
- name: Install iPXE binaries and the boot script
  ansible.builtin.include_tasks: ipxe.yml
```

- [ ] **Step 6: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: two binaries fetched; `boot.ipxe` rendered.

- [ ] **Step 7: Gate — TFTP and the boot script**

Run (on the target):
```bash
curl -sf "tftp://{{ seed_lan_address }}/ipxe.efi" -o /tmp/ipxe.efi && test -s /tmp/ipxe.efi && echo tftp-ok
curl -sf http://boot.lab/boot.ipxe | head -1
```
Expected: `tftp-ok`; the first line of the script is `#!ipxe`.

- [ ] **Step 8: Idempotence** — re-run; expected `changed=0`.

- [ ] **Step 9: Commit**

```bash
git add seed/roles/seed/templates/boot.ipxe.j2 seed/roles/seed/tasks/ipxe.yml seed/roles/seed/tasks/main.yml seed/roles/seed/defaults/main.yml
git commit -m "feat(seed): ipxe binaries and the talos maintenance boot script"
```

### Task 4: chrony — NTP for internet-less nodes

A host package serving time to the LAN, so a node with no upstream reaches correct time before etcd and TLS need it. dnsmasq already hands the seed out as NTP (Task 1, option 42); this makes it answer, and keep answering at a low stratum even if the upstream pool is unreachable (air-gap).

**Files:**
- Create: `seed/roles/seed/templates/chrony.conf.j2`, `seed/roles/seed/tasks/chrony.yml`
- Modify: `seed/roles/seed/handlers/main.yml` (`restart chrony`), `seed/roles/seed/tasks/main.yml`

**Interfaces:**
- Consumes: `seed_lan_subnet`.
- Produces: a chrony NTP server answering on the LAN.

- [ ] **Step 1: See the goal unmet** — `chronyc tracking` → `506 Cannot talk to daemon` / not installed.

- [ ] **Step 2: Create `seed/roles/seed/templates/chrony.conf.j2`**

```jinja
pool 2.pool.ntp.org iburst
driftfile /var/lib/chrony/chrony.drift
makestep 1.0 3
rtcsync
# serve time to the LAN (nodes with no internet reach the seed)
allow {{ seed_lan_subnet }}/24
# keep answering as a low-stratum server even if the upstream pool is unreachable
local stratum 10
```

- [ ] **Step 3: Add the `restart chrony` handler to `seed/roles/seed/handlers/main.yml`**

```yaml
- name: restart chrony
  ansible.builtin.systemd_service:
    name: "{{ 'chrony' if ansible_os_family == 'Debian' else 'chronyd' }}"
    state: restarted
```

- [ ] **Step 4: Create `seed/roles/seed/tasks/chrony.yml`**

```yaml
---
- name: Install chrony
  ansible.builtin.package:
    name: chrony
    state: present

- name: Render the chrony config
  ansible.builtin.template:
    src: chrony.conf.j2
    dest: "{{ '/etc/chrony/chrony.conf' if ansible_os_family == 'Debian' else '/etc/chrony.conf' }}"
    owner: root
    group: root
    mode: "0644"
  notify: restart chrony

- name: Enable and start chrony
  ansible.builtin.systemd_service:
    name: "{{ 'chrony' if ansible_os_family == 'Debian' else 'chronyd' }}"
    enabled: true
    state: started
```

- [ ] **Step 5: Wire into `seed/roles/seed/tasks/main.yml`** (append after the iPXE include):

```yaml
- name: Bring up chrony (NTP)
  ansible.builtin.include_tasks: chrony.yml
```

- [ ] **Step 6: Converge** — `cd seed && ansible-playbook site.yml --limit seed-dev`. Expected: chrony installed, config applied, service active.

- [ ] **Step 7: Gate — chrony is tracking and serving**

Run (on the target):
```bash
chronyc tracking | grep -E 'Reference ID|Stratum'
chronyc sources | tail -n +1
```
Expected: a non-zero `Reference ID` (or `Stratum : 10` while the pool warms up), and the pool in `sources`. Optional client check from another LAN host: `sntp {{ seed_lan_address }}` returns an offset.

- [ ] **Step 8: Idempotence** — re-run; expected `changed=0`.

- [ ] **Step 9: Commit**

```bash
git add seed/roles/seed/templates/chrony.conf.j2 seed/roles/seed/tasks/chrony.yml seed/roles/seed/handlers/main.yml seed/roles/seed/tasks/main.yml
git commit -m "feat(seed): chrony ntp for internet-less nodes"
```

### Task 5: The rehearsal — PXE guest → `tinq adopt` → Ready, pulling from zot over HTTPS

The capstone. A qemu guest set to netboot comes up in Talos maintenance off the seed, `tinq adopt` drives it to Ready, and a workload whose image exists **only in zot** runs — pulled over `https://registry.lab` trusting the step-ca root (the Phase 1 `ca` field, now resolvable because the seed is the guest's DNS). Then one 5900X hardware run.

**Files:**
- Create: `seed/examples/seed-adopt.yaml`, `seed/acceptance/phase2.sh`

**Interfaces:**
- Consumes: the whole boot plane (Tasks 1–4), zot (Phase 1), `tinq adopt`.
- Produces: the acceptance gate and a worked adopt example.

- [ ] **Step 1: Create `seed/acceptance/phase2.sh` (server-side readiness)**

Everything a PXE client will need, assertable on the seed without a VM:

```bash
#!/usr/bin/env bash
set -euo pipefail
D=${SEED_DOMAIN:-lab}
V=${SEED_TALOS_VERSION:-v1.13.7}
LAN=$(awk -F= '/^listen-address=/{print $2}' /etc/dnsmasq.d/seed.conf)
systemctl is-active dnsmasq >/dev/null && echo "dnsmasq: active"
(systemctl is-active chronyd 2>/dev/null || systemctl is-active chrony) >/dev/null && echo "chrony: active"
dig +short "@${LAN}" "registry.${D}" | grep -q . && echo "dns: registry.${D}"
curl -sf "tftp://${LAN}/ipxe.efi" -o /dev/null && echo "tftp: ipxe.efi"
curl -sf "http://boot.${D}/boot.ipxe" | head -1 | grep -q '#!ipxe' && echo "http: boot.ipxe"
curl -sf -o /dev/null "http://boot.${D}/vmlinuz-amd64" && echo "http: vmlinuz"
skopeo inspect "docker://registry.${D}/siderolabs/installer:${V}" >/dev/null && echo "zot: installer ${V}"
echo "phase2 server-side ok"
```

Run: `chmod +x seed/acceptance/phase2.sh && cd seed && ansible seed-dev -m script -a 'acceptance/phase2.sh'`. Expected: every line present, ending `phase2 server-side ok`.

- [ ] **Step 2: Create `seed/examples/seed-adopt.yaml`**

The registries mirror `ghcr.io`, `docker.io`, and `registry.lab` all through zot over HTTPS with the seed CA — so the installer and every workload image pull from the seed, verified. `nameservers` points at the seed, which is why `registry.lab` resolves inside the node.

```yaml
apiVersion: machine.hvf.fleet.io/v1alpha1
kind: TalosMachine
metadata:
  name: seed-rehearsal
spec:
  site: seed-lab
  role: talos-cp
  baremetal:
    maintenanceEndpoint: 192.168.50.60          # the guest's DHCP lease (MAC-reserved)
    systemDiskSerial: ""                         # first adopt run prints the table; copy the serial in
    network:
      address: 192.168.50.60/24
      gateway: 192.168.50.1                      # the seed on the rehearsal segment
      nameservers: [192.168.50.1]                # the seed = DNS, so registry.lab resolves in the node
      hardwareAddr: "52:54:00:5e:ed:60"
  registries:
    - { host: ghcr.io,      endpoint: https://registry.lab, caFile: /etc/ssl/seed/root_ca.crt }
    - { host: docker.io,    endpoint: https://registry.lab, caFile: /etc/ssl/seed/root_ca.crt }
    - { host: registry.lab, endpoint: https://registry.lab, caFile: /etc/ssl/seed/root_ca.crt }
```

- [ ] **Step 3: Put the seed in authoritative mode on an isolated rehearsal segment**

The rehearsal uses a libvirt network where the seed is the sole DHCP + DNS, so the guest gets boot info and name resolution from the seed. In `seed-dev`'s inventory vars set `seed_dhcp_mode: authoritative`, `seed_lan_iface`/`seed_lan_address` to the seed's interface on that network (e.g. `192.168.50.1`), `seed_dhcp_range: 192.168.50.50,192.168.50.150,12h`, and add a MAC reservation for the guest by appending to `dnsmasq.conf.j2`:

```jinja
dhcp-host=52:54:00:5e:ed:60,192.168.50.60
```

Re-converge: `cd seed && ansible-playbook site.yml --limit seed-dev`.

- [ ] **Step 4: Boot a diskful qemu guest set to netboot on that segment**

```bash
qemu-img create -f qcow2 /tmp/seed-rehearsal.qcow2 20G
qemu-system-x86_64 -accel kvm -m 4096 -smp 2 \
  -machine q35 -bios /usr/share/OVMF/OVMF_CODE.fd \
  -drive file=/tmp/seed-rehearsal.qcow2,if=virtio,serial=seedtest \
  -netdev bridge,id=n0,br=virbr-seedboot -device virtio-net-pci,netdev=n0,mac=52:54:00:5e:ed:60 \
  -boot order=n -serial stdio
```
Expected: DHCP → TFTP `ipxe.efi` → `http://boot.lab/boot.ipxe` → Talos kernel/initramfs → the console reaches Talos maintenance and the node answers at `192.168.50.60:50000`. (`br=virbr-seedboot` is the libvirt bridge for the rehearsal segment; substitute yours.)

- [ ] **Step 5: Adopt it**

First run prints the disk table (no serial yet); copy the `seedtest` disk's serial into `systemDiskSerial` and re-run:

```bash
tinq adopt seed/examples/seed-adopt.yaml     # refuses, prints disks
# edit systemDiskSerial, then:
tinq adopt seed/examples/seed-adopt.yaml
```
Expected: the ten steps to Ready; step 7 installs from `registry.lab/siderolabs/installer` (watch zot's access log show the pull over HTTPS); node `Ready`.

- [ ] **Step 6: Capstone — a workload image that exists ONLY in zot, pulled over HTTPS**

```bash
export KUBECONFIG=~/.hvf/seed-lab/*/kubeconfig
kubectl run zotproof --image=registry.lab/seed/busybox:test --restart=Never --command -- sleep 3600
kubectl wait --for=condition=Ready pod/zotproof --timeout=120s
kubectl get pod zotproof -o jsonpath='{.status.containerStatuses[0].image}{"\n"}'
```
Expected: the pod reaches `Ready`, having pulled `registry.lab/seed/busybox:test` (pushed in Phase 1, present nowhere else) over verified HTTPS — the end-to-end proof the `ca` field exists for.

- [ ] **Step 7: The hardware run (5900X)**

Rack the amd64 node, set it to netboot in firmware, and repeat Steps 4–6 in **proxy** mode on the flat LAN — except the node resolves `registry.lab` via `spec.baremetal.network.nameservers: [{{ seed_lan_address }}]` (the seed), since the LAN router's DHCP hands out the router as DNS. This is the only step a VM cannot stand in for: a real NIC PXE ROM and a real disk serial.

- [ ] **Step 8: Commit**

```bash
git add seed/examples/seed-adopt.yaml seed/acceptance/phase2.sh
git commit -m "test(seed): phase 2 rehearsal (pxe -> adopt -> ready over https)"
```

---

## Phase 2 Done — Definition of Done

- `phase2.sh` passes: dnsmasq, chrony, DNS, TFTP, HTTP assets, and the installer in zot all ready.
- A qemu PXE guest reaches Talos maintenance off the seed with no USB, and `tinq adopt` drives it to Ready with the installer pulled from `registry.lab` over HTTPS.
- A pod whose image exists only in zot runs — the deferred Phase 1 proof, closed.
- One 5900X hardware run confirms a real NIC and disk serial (proxy mode + `nameservers` at the seed).

## Self-Review

- **Spec coverage:** D5 dnsmasq proxy-DHCP + mode knob — Task 1. Talos asset server + installer in zot — Task 2. iPXE + boot.ipxe maintenance boot — Task 3. NTP — Task 4. The rehearsal + hardware run + the deferred https pull — Task 5.
- **Placeholder scan:** none. Environment-specific values (LAN iface/IP/subnet, disk serial, bridge name) are inventory/host facts with comments, not TODOs — the same shape as tinq's own `adopt` examples.
- **Cross-task consistency:** the boot URL is HTTP in both the dnsmasq template (Task 1) and `boot.ipxe`/`boot.caddy` (Tasks 2–3); `seed_talos_version`/`seed_talos_arch`, the `registry.lab` names, and the `caFile` path match across tasks and into the Phase 1 `ca` field.
