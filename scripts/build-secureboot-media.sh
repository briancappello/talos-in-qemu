#!/usr/bin/env bash
#
# Build SecureBoot UKI Talos media (ISO + installer image) from an upstream
# BASE plus an ordered list of PRs and patches layered on top.
#
# WHY THIS EXISTS: TPM-sealed disk encryption needs
# /.extra/tpm2-pcr-public-key.pem, and systemd-stub only materialises that from
# a UKI's .pcrpkey section. Those sections are emitted ONLY by
# uki.BuildSigned(), which the imager reaches only when the profile sets
# secureboot: true. A stock `iso`/`installer` build cannot seal a TPM key.
#
# WHAT THE ISO CONTAINS: a signed sd-boot, a signed UKI, and
# loader/keys/auto/{PK,KEK,db}.auth. NOTHING about disk layout -- no partition
# sizes, no RAID, no LUKS. All of that is machine config applied afterwards over
# the maintenance API. The same ISO installs any layout.
#
# Only PUBLIC key material reaches the ISO. The private signing keys stay in
# --keys and never leave this host.
#
# Usage:
#   scripts/build-secureboot-media.sh --out ~/media
#   scripts/build-secureboot-media.sh --out ~/media --base v1.14.0-rc.3 --pr 14145
#   scripts/build-secureboot-media.sh --out ~/media --base 9fceb21 --talos-version v1.15.0-alpha.0
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
PATCH_DIR="$REPO_ROOT/patches/talos"

TALOS_REPO="https://github.com/siderolabs/talos.git"

OUT_DIR=""
KEYS_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/tinq/secureboot"
TALOS_SRC=""
REGISTRY="127.0.0.1:5000"
NAMESPACE="talos"
IMAGE_TAG="secureboot"
EXTENSIONS=()

# BASE is the upstream ref the build starts from: a tag, a branch, or a SHA.
# Fetched by ref rather than `clone --branch`, which cannot take a SHA.
BASE="v1.14.0-rc.2"

# SOURCES is the ORDERED list of things layered on BASE, each "pr:<n>" or
# "patch:<path>". Order is the order given on the command line, because a patch
# routinely extends the PR it sits on. Empty means "read patches/talos/recipe",
# which is where the repo's own recipe lives as data rather than as a glob.
SOURCES=()
RECIPE="$PATCH_DIR/recipe"

# Materialise the patched checkout and stop, so a recipe can be checked without
# paying for an image build.
DRY_RUN=0

# The version pinned into the imager profile, which becomes the ISO9660 volume
# id. SEPARATE from BASE on purpose: BASE may be a SHA, and a volume id has to
# read as TALOS_V<major>_<minor>_<patch> or tinq refuses the image outright
# (cluster/up.go:374). Derived from BASE when BASE looks like a release tag,
# otherwise you must say what to call it.
TALOS_VERSION=""

REBUILD_IMAGER=0

die() { echo "error: $*" >&2; exit 1; }
note() { echo "==> $*"; }

usage() {
	sed -n '2,23p' "$0" | sed 's|^# \{0,1\}||'
	cat <<EOF

Options:
  --out DIR             where the ISO is written (REQUIRED)
  --keys DIR            SecureBoot signing keys   [$KEYS_DIR]
  --talos-src DIR       Talos checkout; cloned+patched if absent
                        [<repo>/.build/talos-src-<base>]
  --registry HOST       OCI registry              [$REGISTRY]
  --namespace NS        registry namespace        [$NAMESPACE]
  --tag TAG             image tag for imager/installer   [$IMAGE_TAG]
  --base REF            upstream tag, branch or SHA to build from [$BASE]
  --pr N                layer siderolabs/talos PR #N on top (repeatable)
  --patch FILE          layer a local patch on top (repeatable)
                        --pr/--patch apply in the order given; default is
                        the recipe in patches/talos/recipe
  --talos-version VER   version pinned into the ISO volume id
                        [derived from --base when it is a release tag]
  --system-extension REF
                        OCI ref of a Talos system extension to bake into the
                        INSTALLER (repeatable). Extensions live in the installer
                        image, never in the boot media, so adding one does NOT
                        invalidate an ISO already written to a USB.
                        A kmod extension MUST match the target Talos version
                        exactly -- it ships a module built against that kernel.
  --rebuild-imager      rebuild the imager from source (slow, ~8 min)
  --dry-run             materialise the patched checkout and stop
  -h, --help
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--out) OUT_DIR="${2:?}"; shift 2 ;;
		--keys) KEYS_DIR="${2:?}"; shift 2 ;;
		--talos-src) TALOS_SRC="${2:?}"; shift 2 ;;
		--registry) REGISTRY="${2:?}"; shift 2 ;;
		--namespace) NAMESPACE="${2:?}"; shift 2 ;;
		--tag) IMAGE_TAG="${2:?}"; shift 2 ;;
		--talos-version) TALOS_VERSION="${2:?}"; shift 2 ;;
		--base) BASE="${2:?}"; shift 2 ;;
		--pr) SOURCES+=("pr:${2:?}"); shift 2 ;;
		--patch) SOURCES+=("patch:${2:?}"); shift 2 ;;
		--rebuild-imager) REBUILD_IMAGER=1; shift ;;
		--system-extension) EXTENSIONS+=("${2:?}"); shift 2 ;;
		--dry-run) DRY_RUN=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown argument: $1 (try --help)" ;;
	esac
done

[[ -n "$OUT_DIR" || "$DRY_RUN" -eq 1 ]] || { usage; die "--out is required"; }

# A release tag doubles as the version label; anything else (a branch, a SHA)
# cannot, because the volume id must parse.
if [[ -z "$TALOS_VERSION" ]]; then
	if [[ "$BASE" =~ ^v[0-9]+\.[0-9]+\.[0-9]+ ]]; then
		TALOS_VERSION="$BASE"
	else
		die "--base $BASE is not a release tag, so it cannot name the image.
  Pass --talos-version vX.Y.Z (it becomes the ISO volume id, which tinq parses)."
	fi
fi

# Default recipe: patches/talos/recipe, in file order. Comments and blank lines
# ignored; patch: paths resolve relative to the recipe itself.
if [[ ${#SOURCES[@]} -eq 0 && -f "$RECIPE" ]]; then
	while IFS= read -r line; do
		line="${line%%#*}"
		line="${line//[[:space:]]/}"
		[[ -z "$line" ]] && continue
		case "$line" in
			patch:*) SOURCES+=("patch:$PATCH_DIR/${line#patch:}") ;;
			*)       SOURCES+=("$line") ;;
		esac
	done < "$RECIPE"
fi

# Per-BASE checkout: switching --base must never silently reuse a tree built
# from a different ref.
[[ -n "$TALOS_SRC" ]] || TALOS_SRC="$REPO_ROOT/.build/talos-src-${BASE//\//-}"

for cmd in podman crane git curl; do
	command -v "$cmd" >/dev/null || die "missing required command: $cmd"
done

TALOSCTL="$(command -v talosctl || true)"
[[ -n "$TALOSCTL" ]] || TALOSCTL="$HOME/.local/share/mise/installs/talosctl/latest/talosctl"
[[ -x "$TALOSCTL" ]] || die "talosctl not found (needed for 'gen secureboot')"

mkdir -p ${OUT_DIR:+"$OUT_DIR"} "$KEYS_DIR"
[[ -n "$OUT_DIR" ]] && OUT_DIR="$(cd "$OUT_DIR" && pwd)"
KEYS_DIR="$(cd "$KEYS_DIR" && pwd)"

# ensure_talos_src materialises the patched Talos tree, and is called ONLY from
# the branches that actually compile something. A run that reuses an existing
# imager and installer-base needs no source at all, and cloning Talos to do
# nothing with it is a slow way to achieve that.
# fetch_source materialises one entry of SOURCES as a patch file on stdout's
# path, printing the path. A PR is taken from GitHub's .patch endpoint rather
# than by fetching refs/pull/N/head: the endpoint yields a mailbox that applies
# to a SHALLOW checkout, whereas cherry-picking a PR head needs history across
# a merge base the shallow clone does not have.
fetch_source() {
	local entry="$1" dest="$2"
	case "$entry" in
		pr:*)
			local n="${entry#pr:}"
			note "fetching siderolabs/talos PR #$n"
			curl -fsSL "https://github.com/siderolabs/talos/pull/$n.patch" -o "$dest" \
				|| die "could not fetch PR #$n -- is the number right, and is it still open?"
			;;
		patch:*)
			local f="${entry#patch:}"
			[[ -f "$f" ]] || die "patch not found: $f"
			cp "$f" "$dest"
			;;
		*) die "unrecognised source $entry (want pr:<n> or patch:<path>)" ;;
	esac
}

# ensure_build_env makes `docker buildx` work before any make target that needs
# it. The Talos Makefile shells out to docker unconditionally; on a rootless
# podman host that is a shim on PATH plus DOCKER_HOST pointing at the podman
# socket. Neither is exported by default, and discovering that from
#
#   /bin/sh: line 1: docker: command not found
#
# is a terrible thing to hit after the checkout has already been fetched and
# patched. Set it up here, or say exactly what is missing.
ensure_build_env() {
	if ! command -v docker >/dev/null 2>&1 && [[ -x "$HOME/bin/docker" ]]; then
		export PATH="$HOME/bin:$PATH"
	fi

	command -v docker >/dev/null 2>&1 || die \
"the Talos build needs a \`docker\` CLI and none is on PATH

  This host builds with rootless podman, which needs the docker-compat shim:
    ~/bin/docker  and  ~/.docker/cli-plugins/docker-buildx"

	if [[ -z "${DOCKER_HOST:-}" ]]; then
		local sock="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/podman/podman.sock"
		if [[ -S "$sock" ]]; then
			export DOCKER_HOST="unix://$sock"
			note "using podman socket: $DOCKER_HOST"
		fi
	fi

	# Proves the daemon answers, not merely that the binary exists -- a stopped
	# podman socket fails identically to a missing one, several minutes later.
	docker buildx version >/dev/null 2>&1 || die \
"\`docker buildx\` is not usable

  DOCKER_HOST=${DOCKER_HOST:-<unset>}
  If this host uses rootless podman, start its socket:
    systemctl --user start podman.socket"
}

ensure_talos_src() {
	if [[ -d "$TALOS_SRC/.git" ]]; then
		note "using Talos checkout: $TALOS_SRC"
		return
	fi

	note "fetching Talos $BASE"
	mkdir -p "$TALOS_SRC"
	git -C "$TALOS_SRC" init -q
	git -C "$TALOS_SRC" remote add origin "$TALOS_REPO"
	# Fetch BY REF so a tag, a branch and a SHA are all handled the same way.
	# Shallow: sources are applied as diffs, so no history is needed.
	git -C "$TALOS_SRC" fetch -q --depth 1 origin "$BASE" \
		|| die "could not fetch $BASE from $TALOS_REPO"
	git -C "$TALOS_SRC" checkout -q FETCH_HEAD

	# Fetching BY REF creates no local tag, so the Makefile's
	# `TAG ?= $(git describe --tag --always ...)` degrades to a bare SHA and
	# bakes that in as the Talos version. Talos then cannot parse its own
	# version to derive the kube-apiserver/kubelet image tags, and the install
	# dies with `Invalid character(s) found in major number "3270fec"` -- long
	# after the build looked fine. Every make invocation passes TAG explicitly;
	# the tag here additionally keeps `git describe` honest inside the tree.
	if [[ "$BASE" =~ ^v[0-9] ]]; then
		git -C "$TALOS_SRC" tag -f "$BASE" FETCH_HEAD >/dev/null 2>&1 || true
	fi

	local entry pf
	for entry in "${SOURCES[@]}"; do
		pf="$(mktemp)"
		fetch_source "$entry" "$pf"

		# Already upstream? Reverse-applying cleanly is a precise test, and it
		# is the expected outcome as these land: a newer BASE simply carries
		# them. Skip, do not fail, but say so -- that line is the cue to drop
		# the source from the recipe.
		if git -C "$TALOS_SRC" apply --check --reverse "$pf" 2>/dev/null; then
			note "skipping $entry -- already present in $BASE (drop it from the recipe)"
			rm -f "$pf"
			continue
		fi

		note "applying $entry"
		git -C "$TALOS_SRC" apply "$pf" || { rm -f "$pf"; die \
"failed to apply $entry to $BASE

  It neither applies nor is already present, so it and the base have diverged.
  Rebase it onto $BASE, or drop it if the change landed in a different form."; }
		rm -f "$pf"
	done

	# Commit so the tree is not dirty: `git describe` feeds the version baked
	# into the imager, and a -dirty suffix there is noise in every image tag.
	git -C "$TALOS_SRC" add -A
	git -C "$TALOS_SRC" -c user.name=tinq -c user.email=tinq@localhost \
		commit -qm "tinq: $BASE + ${SOURCES[*]}"
}

if [[ "$DRY_RUN" -eq 1 ]]; then
	ensure_talos_src
	note "checkout ready: $TALOS_SRC"
	note "recipe applied: ${SOURCES[*]}"
	exit 0
fi

IMAGER_REF="$REGISTRY/$NAMESPACE/imager:$IMAGE_TAG"
BASE_REF="$REGISTRY/$NAMESPACE/installer-base:$IMAGE_TAG"
INSTALLER_REF="$REGISTRY/$NAMESPACE/installer:$IMAGE_TAG"

# Scratch dir for imager output, so a half-finished build never lands in --out.
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ── 1. signing keys ──────────────────────────────────────────────────────────
#
# THESE KEYS ARE THE ROOT OF TRUST FOR EVERY MACHINE YOU ENROL. Once PK/KEK/db
# are written into a machine's firmware, ONLY binaries signed by
# uki-signing-key.pem will boot it. Lose the key and you cannot ship that
# machine another kernel -- recovery means clearing PK in firmware setup and
# re-enrolling from scratch.
if [[ -f "$KEYS_DIR/uki-signing-key.pem" ]]; then
	note "reusing SecureBoot keys in $KEYS_DIR"
else
	note "generating SecureBoot keys in $KEYS_DIR"
	"$TALOSCTL" gen secureboot uki --common-name "Talos SecureBoot" -o "$KEYS_DIR"
	"$TALOSCTL" gen secureboot pcr -o "$KEYS_DIR"
	# -o controls only WHERE THE .auth FILES ARE WRITTEN. The certificate and key
	# it signs them with default to ./_out regardless, so without these three
	# flags this fails with "open _out/uki-signing-key.pem: no such file" for any
	# --keys outside the Talos checkout.
	"$TALOSCTL" gen secureboot database -o "$KEYS_DIR" \
		--enrolled-certificate "$KEYS_DIR/uki-signing-cert.pem" \
		--signing-certificate "$KEYS_DIR/uki-signing-cert.pem" \
		--signing-key "$KEYS_DIR/uki-signing-key.pem"
	cat >&2 <<EOF

  !! BACK UP $KEYS_DIR NOW, AND KEEP IT SECRET.
  !! uki-signing-key.pem and pcr-signing-key.pem are unrecoverable. Any machine
  !! enrolled with these keys boots nothing else. The homelab repo has SOPS
  !! (.sops.yaml, secrets/) -- that is the right home for them.

EOF
fi

for f in uki-signing-key.pem uki-signing-cert.pem pcr-signing-key.pem; do
	[[ -f "$KEYS_DIR/$f" ]] || die "missing key material: $KEYS_DIR/$f"
done

# ── 2. installer-base ────────────────────────────────────────────────────────
#
# Just the installer binary; unaffected by the machined patches, so it is built
# once and reused. Rebuilt only when absent from the registry.
if crane manifest --insecure "$BASE_REF" >/dev/null 2>&1; then
	note "installer-base present: $BASE_REF"
else
	note "building installer-base (missing from registry)"
	ensure_talos_src
	ensure_build_env
	make -C "$TALOS_SRC" docker-installer-base DEST=_out TAG="$TALOS_VERSION" \
		REGISTRY="$REGISTRY" USERNAME="$NAMESPACE" PLATFORM=linux/amd64
	crane push --insecure "$TALOS_SRC/_out/installer-base.tar" "$BASE_REF"
fi

# ── 3. imager ────────────────────────────────────────────────────────────────
#
# This is the expensive one (~8 min): it compiles Talos and bakes the initramfs,
# so it MUST be rebuilt after any change to machined -- which includes the
# LVMDeactivationController patch.
if [[ "$REBUILD_IMAGER" -eq 1 ]] || ! crane manifest --insecure "$IMAGER_REF" >/dev/null 2>&1; then
	note "building imager (this takes several minutes)"
	ensure_talos_src
	ensure_build_env
	make -C "$TALOS_SRC" docker-imager DEST=_out TAG="$TALOS_VERSION" \
		REGISTRY="$REGISTRY" USERNAME="$NAMESPACE" PLATFORM=linux/amd64
	crane push --insecure "$TALOS_SRC/_out/imager.tar" "$IMAGER_REF"
else
	note "reusing imager: $IMAGER_REF (pass --rebuild-imager to force)"
fi

# ── imager profiles ──────────────────────────────────────────────────────────
#
# Generated rather than checked in, so registry/tag/version can never drift out
# of step with the arguments actually used.
#
# The key paths are the imager's own defaults under /secureboot; they are spelled
# out because a partial set is a hard error in prepareEnrollmentDBs.
secureboot_input() {
	cat <<EOF
  baseInstaller:
    imageRef: $BASE_REF
    forceInsecure: true
  secureboot:
    secureBootSigner:
      keyPath: /secureboot/uki-signing-key.pem
      certPath: /secureboot/uki-signing-cert.pem
    pcrSigner:
      keyPath: /secureboot/pcr-signing-key.pem
    platformKeyPath: /secureboot/PK.auth
    keyExchangeKeyPath: /secureboot/KEK.auth
    signatureKeyPath: /secureboot/db.auth
EOF
}

# system_extensions emits the imager's systemExtensions block, or nothing.
#
# INSTALLER ONLY, deliberately. An extension is installed onto the disk, so it
# belongs to the installer image; putting the same list in the ISO profile would
# bake it into the boot media instead, changing the UKI and therefore its PCR 11
# measurement -- which would invalidate the ISO already written to a USB and the
# TPM policy sealed against it, to no purpose. The boot media only has to reach
# maintenance mode.
system_extensions() {
	[[ ${#EXTENSIONS[@]} -eq 0 ]] && return 0

	echo "  systemExtensions:"

	for ref in "${EXTENSIONS[@]}"; do
		echo "    - imageRef: $ref"
	done
}

run_imager() {
	podman run --rm -i --network=host --userns=keep-id \
		-v "$KEYS_DIR:/secureboot:ro" \
		-v "$WORK:/out" \
		-e SOURCE_DATE_EPOCH=1700000000 \
		-e DETERMINISTIC_SEED=1 \
		"$IMAGER_REF" -
}

# ── 4. installer image ───────────────────────────────────────────────────────
note "building SecureBoot installer"
run_imager <<EOF
arch: amd64
platform: metal
secureboot: true
version: $TALOS_VERSION
input:
$(secureboot_input)
$(system_extensions)
output:
  kind: installer
  outFormat: raw
EOF
crane push --insecure "$WORK/installer-amd64-secureboot.tar" "$INSTALLER_REF"

# ── 5. ISO ───────────────────────────────────────────────────────────────────
#
# sdBootEnrollKeys: force -- enrol whenever the firmware is in setup mode. The
# `if-safe` default only auto-enrols inside a VM, which is exactly wrong for the
# bare-metal case this media exists for.
note "building SecureBoot ISO"
run_imager <<EOF
arch: amd64
platform: metal
secureboot: true
version: $TALOS_VERSION
input:
$(secureboot_input)
output:
  kind: iso
  isoOptions:
    sdBootEnrollKeys: force
  outFormat: raw
EOF

ISO_NAME="talos-${TALOS_VERSION}-amd64-secureboot.iso"
install -m 0644 "$WORK/metal-amd64-secureboot.iso" "$OUT_DIR/$ISO_NAME"

# ── 6. verify ────────────────────────────────────────────────────────────────
#
# The volume id is how tinq identifies the image, and an unparseable one is a
# refusal rather than a warning. Catch it here, not four steps into an install.
VOLID="$(python3 - "$OUT_DIR/$ISO_NAME" <<'PY'
import sys
with open(sys.argv[1], 'rb') as f:
    f.seek(16 * 2048)
    print(f.read(2048)[40:72].decode('ascii', 'replace').strip())
PY
)"

echo
note "ISO:       $OUT_DIR/$ISO_NAME"
note "volume id: $VOLID"
note "installer: $INSTALLER_REF"
echo

case "$VOLID" in
	TALOS_V*) ;;
	*) die "volume id $VOLID is not parseable; tinq will refuse this image" ;;
esac

cat <<EOF
Next:
  1. sudo <homelab>/scripts/write-usb.sh $OUT_DIR/$ISO_NAME
  2. Mirror the installer somewhere the node can reach, e.g.
       crane copy $INSTALLER_REF registry.lab/$NAMESPACE/installer:$IMAGE_TAG
     and reference that in the machine config's install.image.
  3. On the target: enable TPM, clear the Platform Key (setup mode), boot the USB.
     First boot enrols PK/KEK/db and reboots; second boot lands in maintenance.
  4. tinq adopt <machine>.yaml
EOF
