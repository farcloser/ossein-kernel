# This file is the project's own.
# Add recipes leveraging provided `do` ready-made recipes, or create your own.
# The import must be kept: it mounts every shared limen task under `just do ...`.
import '.limen/just/main.just'

# ossein-kernel builds the ossein guest kernel STANDALONE — it links
# Virtualization.framework directly (Code-Hex/vz) and provides its own microVM boot
# (internal/vm), Debian root disk (internal/rootfs), and guest init
# (cmd/ossein-kernel/init), with no dependency on the ossein runtime. CGO is mandatory
# for VZ and the host binary is darwin/arm64-only; the init is static linux/arm64.
export CGO_ENABLED := '1'

# go-licenses false-positives our own GPL-2.0 module once first-party code spans
# sub-packages (internal/vm, internal/rootfs) — see go-licenses#186. Our license is
# intentional (this builds a Linux kernel); ignore the self-module in the dep scan.
export LINT_GO_LICENSES_FLAGS := '--ignore=github.com/farcloser/ossein-kernel'

# The FIRST recipe defined here becomes `just`'s default.
lint: do::lint::default
fix: do::fix::default
test:
    go test ./...

# The guest PID-1 init: one static linux/arm64 binary, embedded in the Debian root disk
# by internal/rootfs. Mirrors `do build go`'s release flags (trimpath, netgo/osusergo,
# stripped, git-describe version stamp) EXCEPT -buildmode=pie: with CGO=0 that yields a
# DYNAMIC pie needing /lib/ld-linux-aarch64.so.1, but PID 1 must be self-contained, so the
# init stays STATICALLY linked (as ossein's own vminitd was). Cross-compiled to the guest arch.
build-init:
    #!/usr/bin/env bash
    set -euo pipefail
    version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -tags='netgo,osusergo' \
        -ldflags "-s -w -X main.version=${version}" -o build/ossein-init ./cmd/ossein-kernel/init

build: build-init
    CGO_ENABLED=1 just do build go   # shared reproducible build (trimpath, stamped, PIE, stripped); CGO for VZ
    codesign --force --sign - --timestamp=none --entitlements vz.entitlements build/ossein-kernel
all: kernel kernel-debug kernel-nopatch

# ---------------------------------------------------------------------------
# Guest kernel factory. This project OWNS everything it needs: source pins, config fragment,
# patches (kernel/patches/), the build/verify recipes, AND the whole build environment —
# microVM boot (internal/vm), the Debian root disk built from the pinned image
# (internal/rootfs), and the guest init (cmd/ossein-kernel/init). No dependency on ossein.
# Everything WE build (kernel, perf, init, root disk, scratch) lands in ./build.
# ---------------------------------------------------------------------------

# Pinned guest kernel source (passed explicitly to ossein-kernel). Latest stable.
kernel_source_url := "https://cdn.kernel.org/pub/linux/kernel/v7.x/linux-7.1.3.tar.xz"
kernel_source_sha256 := "be41c068e88f5242a19bccdbffbe077b18c47b45f627e2325504b4fab79dd1dc"

# Kernel build container (throwaway build VM only; nothing guest-facing), PINNED for
# reproducibility on FOUR axes, ALL passed down to build.sh: the base image by DIGEST,
# the Debian SUITE (codename), every apt package to an immutable Debian SNAPSHOT
# (build.sh rewrites sources to snapshot.debian.org @ the timestamp), and the CLANG
# toolchain to an exact release tarball (see kernel_llvm_* below). image/suite/snapshot
# must agree — kernel_debian_suite must match the image's codename (trixie-slim → trixie).
# Pulled via mirror.gcr.io (Google's transparent Docker Hub mirror): anonymous, NOT
# Hub-rate-limited, digest-preserving — so this pinned public base needs no credential
# helper (ossein's general `run`/`pull` keeps full Hub auth). Re-pin for newer:
# `crane digest debian:trixie-slim` for the digest; bump kernel_apt_snapshot.
kernel_build_image := "mirror.gcr.io/library/debian@sha256:020c0d20b9880058cbe785a9db107156c3c75c2ac944a6aa7ab59f2add76a7bd"
kernel_debian_suite := "trixie" # apt suite/codename; MUST match the image
kernel_apt_snapshot := "20260701T025158Z" # snapshot.debian.org archive timestamp

# clang/LLVM toolchain, pinned to an EXACT official release tarball (url + sha256), NOT
# apt's clang-NN. apt.llvm.org is a rolling repo (only the newest build per major is kept,
# no snapshot archive) — it pins the major only. The github.com/llvm/llvm-project release
# asset pins the full version forever, so the compiler is as reproducible as every other
# input. ossein-kernel downloads + verifies it host-side, stages it in, build.sh extracts
# it and builds with LLVM=1. Re-pin for a newer clang: pick a newer LLVM-<ver>-Linux-ARM64
# asset and update both URL + sha256 (the GitHub API publishes the asset "digest").
kernel_llvm_url := "https://github.com/llvm/llvm-project/releases/download/llvmorg-22.1.8/LLVM-22.1.8-Linux-ARM64.tar.xz"
kernel_llvm_sha256 := "805efad2bb91cb4967fa569e0881d10c0f69c04461cf671cccbae19f547acc34"

# Guest kernel Kconfig allowlist fragment — the "which kernel" input (ossein-kernel merges
# it onto `make tinyconfig`). Point this at another fragment to build a different kernel.
kernel_config := "kernel/config/kernel-fragment"

# Kernel patches applied to the PRISTINE tarball, in lexical order (0001-, 0002-, ...). We do
# NOT fork: the source stays the pinned url+sha256 upstream release and every delta is an
# explicit, reviewable file with its rationale in the header. build.sh applies them with
# `patch --forward` under `set -e`, so a patch that stops applying FAILS the build on a kernel
# bump instead of silently disappearing. Both current patches fix upstream bugs that only bite
# VZ guests (see each patch header); revisit them when bumping kernel_source_url.
kernel_patches := "kernel/patches"

# Kata bootstrap kernel: the COLD-START seed only — used once, to boot the very first build
# VM before the factory can self-host on its own output. Pinned by URL + sha256 like every
# other download (GitHub publishes the asset digest: `gh api …/releases/tags/<v>`). After
# the first successful build, --out exists and this is never fetched again.
kernel_kata_url := "https://github.com/kata-containers/kata-containers/releases/download/3.32.0/kata-static-3.32.0-arm64.tar.zst"
kernel_kata_sha256 := "8736c054d9223974735394f822000823baef509e1c33405ec798240fa9b6e4b5"

# Kernel version tag appended to the release string (→ 7.1.3-ossein in uname -r + boot
# banner). Pass the BARE tag (no leading dash) — ossein-kernel prepends the "-" (a leading
# dash here would be parsed as a flag by kong). REQUIRED all the way down (ossein-kernel +
# build.sh reject empty) so a build is never silently unlabeled — always identifiable.
kernel_localversion := "ossein"

# Guest init for the build VM: WE build it (`just build-init` → build/ossein-init), a static
# linux/arm64 binary internal/rootfs embeds in the Debian root disk. No ossein artifact needed.
ossein_init := "build/ossein-init"

# Build the guest kernel standalone: boot a microVM (internal/vm) on the seed/self-host kernel,
# root it on a Debian ext4 built from the pinned image (internal/rootfs), run build.sh via our
# own init. Links Virtualization.framework and MUST be codesigned with the vz entitlement.
# Source in kernel/ + the fragment; output + scratch in build/. The init is built by `build`.
kernel: build
    build/ossein-kernel \
        --init "{{ ossein_init }}" \
        --image "{{ kernel_build_image }}" \
        --debian-suite "{{ kernel_debian_suite }}" \
        --apt-snapshot "{{ kernel_apt_snapshot }}" \
        --llvm-url "{{ kernel_llvm_url }}" \
        --llvm-sha256 "{{ kernel_llvm_sha256 }}" \
        --localversion "{{ kernel_localversion }}" \
        --kernel-config "{{ kernel_config }}" \
        --kernel-patches "{{ kernel_patches }}" \
        --source-url "{{ kernel_source_url }}" \
        --source-sha256 "{{ kernel_source_sha256 }}" \
        --kata-url "{{ kernel_kata_url }}" \
        --kata-sha256 "{{ kernel_kata_sha256 }}"
    @just kernel-verify-config

# Build the SAME kernel with kernel/patches/ NOT applied → build/kernel-arm64.nopatch.
# The A/B baseline for ossein's `just bench-perf`: identical source + config + toolchain pins,
# the patch set is the ONLY variable, so any delta in the bench matrix is ours and nothing
# else. Implemented by pointing --kernel-patches at an empty dir (the flag is required, and an
# empty dir is the honest way to say "none" — build.sh's glob then applies nothing).
# NOTE: --out doubles as the self-host bootstrap, so the first run of this (no .nopatch
# kernel yet) falls back to the Kata seed; it's cached in build/kernel-scratch afterwards.
kernel-nopatch: build
    mkdir -p build/no-patches
    build/ossein-kernel \
        --init "{{ ossein_init }}" \
        --out "build/kernel-arm64.nopatch" \
        --kernel-patches "build/no-patches" \
        --image "{{ kernel_build_image }}" \
        --debian-suite "{{ kernel_debian_suite }}" \
        --apt-snapshot "{{ kernel_apt_snapshot }}" \
        --llvm-url "{{ kernel_llvm_url }}" \
        --llvm-sha256 "{{ kernel_llvm_sha256 }}" \
        --localversion "{{ kernel_localversion }}-nopatch" \
        --kernel-config "{{ kernel_config }}" \
        --source-url "{{ kernel_source_url }}" \
        --source-sha256 "{{ kernel_source_sha256 }}" \
        --kata-url "{{ kernel_kata_url }}" \
        --kata-sha256 "{{ kernel_kata_sha256 }}"

# Build the ship kernel + in-guest observability -> build/kernel-arm64.debug. NOT shipped:
# use it to investigate, then throw it away. merge_config takes both fragments in order, so
# the debug one only ADDS (see kernel/config/kernel-debug-fragment for what and why).
#   OSSEIN_KERNEL=ossein-kernel/build/kernel-arm64.debug ossein run --cpus 4 ...  (+ schedstats=enable)
# Skips verify-config: the golden describes the SHIP kernel, and this one deliberately differs.
kernel-debug: build
    bash kernel/merge-fragments.sh {{ kernel_config }} kernel/config/kernel-debug-fragment > build/kernel-debug.merged
    build/ossein-kernel \
        --init "{{ ossein_init }}" \
        --out "build/kernel-arm64.debug" \
        --kernel-config "build/kernel-debug.merged" \
        --kernel-patches "{{ kernel_patches }}" \
        --image "{{ kernel_build_image }}" \
        --debian-suite "{{ kernel_debian_suite }}" \
        --apt-snapshot "{{ kernel_apt_snapshot }}" \
        --llvm-url "{{ kernel_llvm_url }}" \
        --llvm-sha256 "{{ kernel_llvm_sha256 }}" \
        --localversion "{{ kernel_localversion }}-debug" \
        --source-url "{{ kernel_source_url }}" \
        --source-sha256 "{{ kernel_source_sha256 }}" \
        --kata-url "{{ kernel_kata_url }}" \
        --kata-sha256 "{{ kernel_kata_sha256 }}"

# Assert the SHIPPED kernel carries our intended config: extract the embedded IKCONFIG
# and fail on drift (merge_config + olddefconfig silently revert EXPERT-gated or
# arch-unreachable fragment lines).
kernel-verify-config kernel="build/kernel-arm64":
    bash kernel/verify-config.sh {{ kernel }}

# Regenerate kernel/config/kernel-golden — the committed snapshot of every DECIDED symbol that
# verify-config diffs against to catch silent default-y drift, e.g. a new feature
# riding in on a kernel-version bump. Run ONLY when you intentionally change the config, then
# review + commit the diff (that diff IS the record of what the change turned on/off).
# Delegates to verify-config.sh --golden — extraction AND normalization live there, so this
# snapshot and the drift diff can never normalize differently. Written via temp + rename so a
# failed extraction cannot truncate the committed golden.
kernel-golden kernel="build/kernel-arm64":
    #!/usr/bin/env bash
    set -euo pipefail
    tmp="$(mktemp kernel/config/.kernel-golden.XXXXXX)"; trap 'rm -f "$tmp"' EXIT
    bash kernel/verify-config.sh --golden "{{ kernel }}" > "$tmp"
    [ -s "$tmp" ] || { echo "ERROR: empty golden — no IKCONFIG in {{ kernel }}?" >&2; exit 1; }
    mv "$tmp" kernel/config/kernel-golden
    trap - EXIT
    echo "wrote kernel/config/kernel-golden ($(wc -l < kernel/config/kernel-golden | tr -d ' ') symbols)"

# ---------------------------------------------------------------------------
# Release. We publish the BUILT KERNEL, not the tooling: the builder (ossein-kernel) and the
# guest init are repo-internal build scaffolding, wired up by `just build`, and have no
# standalone consumer. The kernel is the product — ossein (and any VZ user) points
# OSSEIN_KERNEL= at the blob. This runs LOCALLY: the build boots a real VM on
# Virtualization.framework (arm64 + the vz entitlement) and cannot run on stock CI, so
# goreleaser's build-in-CI model doesn't fit — a thin recipe that signs + publishes an
# already-built, boot-verified artifact does.
# ---------------------------------------------------------------------------

# The maintainer's keyless-signing identity (sigstore/Fulcio SAN) and OIDC issuer. Consumers
# verify a released kernel against these EXACT values — a build signed by any other identity or
# issuer is not ours. Read from the signing cert; update both if the signer/provider changes.
maintainer_identity := "apostasie@farcloser.world"
maintainer_oidc_issuer := "https://github.com/login/oauth"

# Cut a kernel release: (re)build + boot-verify, then publish build/kernel-arm64 + perf-arm64
# to a GitHub release with checksums, a keyless cosign (sigstore) signature over the checksums,
# and notes pinning every build input so the blob is reproducible + auditable from the release
# alone. Tag shape: v<upstream-version>-{{ kernel_localversion }}.<N> (e.g. v7.1.3-ossein.1);
# the version MUST match the pinned kernel source. Signing opens a browser for the OIDC flow.
# just release-kernel v7.1.3-ossein.1
release-kernel tag:
    #!/usr/bin/env bash
    set -euo pipefail
    tag="{{ tag }}"
    upstream="$(basename "{{ kernel_source_url }}" | sed -E 's/^linux-(.+)\.tar\..*$/\1/')"

    # --- pre-flight (cheap; fail before the long build and before any tag/push) ---
    # Tag is vX.Y.Z-<localversion>.N and X.Y.Z equals the pinned upstream source version:
    # a release can never disagree with the kernel it ships.
    if ! printf '%s' "$tag" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+-{{ kernel_localversion }}\.[0-9]+$'; then
        echo "tag '$tag' must be vX.Y.Z-{{ kernel_localversion }}.N (e.g. v${upstream}-{{ kernel_localversion }}.1)" >&2
        exit 2
    fi
    if [ "$(printf '%s' "$tag" | sed -E 's/^v([0-9]+\.[0-9]+\.[0-9]+)-.*/\1/')" != "$upstream" ]; then
        echo "tag version != pinned kernel source $upstream — bump kernel_source_url or fix the tag" >&2
        exit 2
    fi
    # A dirty tree means the pins/commit recorded in the notes would not match what shipped.
    if [ -n "$(git status --porcelain)" ]; then
        echo "working tree is dirty — commit or stash before releasing:" >&2
        git status --short >&2
        exit 1
    fi
    if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
        echo "tag $tag already exists — pick the next .N (or: git tag -d $tag)" >&2
        exit 1
    fi
    gh auth status >/dev/null 2>&1 || { echo "gh is not authenticated — run 'gh auth login'" >&2; exit 1; }

    # --- build + boot-verify (kernel recipe: build, smoke-test, verify-config) ---
    just kernel

    # --- stage, checksum, sign (keyless cosign over the checksum file covers both blobs) ---
    rel=build/release
    rm -rf "$rel"; mkdir -p "$rel"
    cp build/kernel-arm64 build/perf-arm64 "$rel/"
    ( cd "$rel"
      shasum -a 256 kernel-arm64 perf-arm64 > SHA256SUMS
      cosign sign-blob --yes --bundle SHA256SUMS.cosign.bundle SHA256SUMS
    )

    # --- release notes: commit pins every in-repo input (config, golden, patches); the sha256s
    # below pin only the EXTERNAL downloads the commit can't vouch for. Clean tree is enforced
    # above, so the commit == what shipped. ---
    {
        echo "## Verify"
        echo '```'
        echo "cosign verify-blob --bundle SHA256SUMS.cosign.bundle \\"
        echo "  --certificate-identity {{ maintainer_identity }} \\"
        echo "  --certificate-oidc-issuer {{ maintainer_oidc_issuer }} SHA256SUMS"
        echo "shasum -a 256 -c SHA256SUMS"
        echo '```'
        echo
        echo "## Checksums"
        echo '```'
        cat "$rel/SHA256SUMS"
        echo '```'
        echo
        echo "## Build inputs"
        echo "- kernel source: {{ kernel_source_url }}"
        echo "  - sha256: {{ kernel_source_sha256 }}"
        echo "- LLVM toolchain: {{ kernel_llvm_url }}"
        echo "  - sha256: {{ kernel_llvm_sha256 }}"
        echo "- build image: {{ kernel_build_image }}"
        echo "- debian suite: {{ kernel_debian_suite }}"
        echo "- apt snapshot: {{ kernel_apt_snapshot }}"
        echo
        echo "## Patches"
        for p in {{ kernel_patches }}/*.patch; do
            [ -e "$p" ] || { echo "- (none)"; break; }
            echo "- $(basename "$p")"
        done
    } > "$rel/NOTES.md"

    # --- tag, push, publish (only now, after everything above succeeded) ---
    git tag -a "$tag" -m "$tag"
    git push origin HEAD
    git push origin "refs/tags/$tag"
    gh release create "$tag" --verify-tag --title "$tag" --notes-file "$rel/NOTES.md" \
        "$rel/kernel-arm64" "$rel/perf-arm64" "$rel/SHA256SUMS" "$rel/SHA256SUMS.cosign.bundle"
    echo ">> released $tag"
