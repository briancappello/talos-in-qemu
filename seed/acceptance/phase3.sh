#!/usr/bin/env bash
# Phase 3 gate: a RESTORE, not a backup. Wipe zot + gitea on-disk state,
# restore from restic, and prove the data is back.
set -euo pipefail
set -a; . /var/lib/seed/backup/seed-backup.env; set +a
D=${SEED_DOMAIN:-lab}

# Restore + restart is one proven routine, used for both the main restore and
# the recovery trap. --host pins to THIS host so a shared/off-host repo never
# restores another host's snapshot over our zot/gitea state.
restore_and_start() {
  restic restore latest --host "$(hostname)" --target / \
    --include /var/lib/seed/zot/registry --include /var/lib/seed/gitea/data
  systemctl start zot.service gitea.service
}

# FIX 1: never leave the host wiped-and-down. If anything below fails, re-run
# the (proven) restore+restart. The drill still exits non-zero -- the trap
# recovers the HOST, it does not mask the failure.
trap 'rc=$?; if [ $rc -ne 0 ]; then echo "!!! RESTORE DRILL FAILED (rc=$rc) — RE-RESTORING so the host is not left broken"; restore_and_start && echo "recovered: zot+gitea restored and started" || echo "!!! RECOVERY ALSO FAILED — MANUAL RESTORE NEEDED (data is in the restic snapshot)"; fi' EXIT

# 1. fresh snapshot of current state
systemctl start seed-backup.service

# 2. simulate loss: stop the services and wipe their on-disk state
systemctl stop zot.service gitea.service
rm -rf /var/lib/seed/zot/registry /var/lib/seed/gitea/data

# 3. restore exactly those paths from the latest snapshot, and bring services back
restore_and_start
sleep 5

# 4. prove it: the image pulls again (restored blobs), and a real git clone
# retrieves the commit (restored on-disk git objects, not just the PG record)
podman rmi "registry.${D}/seed/busybox:test" >/dev/null 2>&1 || true
podman pull "registry.${D}/seed/busybox:test" >/dev/null && echo "zot: image restored"

pw=$(cat /var/lib/seed/secrets/gitea_admin_password)
rm -rf /tmp/smoke-restore-check
git clone "https://seedadmin:$pw@git.${D}/seedadmin/smoke" /tmp/smoke-restore-check
test -n "$(git -C /tmp/smoke-restore-check log --oneline)" && echo "gitea: repo restored"

echo "phase3 ok: restore verified"
