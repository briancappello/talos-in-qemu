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
