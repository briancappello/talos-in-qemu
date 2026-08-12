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
