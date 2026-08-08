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
