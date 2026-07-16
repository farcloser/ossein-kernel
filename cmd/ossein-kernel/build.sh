#!/bin/bash
# Guest kernel build: runs inside the ossein build container by cmd/ossein-kernel: build the
# arm64 Image from the source + config staged at /kernel, drop it back there.
#
# TOOLCHAIN: clang + ld.lld + llvm-* with ThinLTO (CONFIG_LTO_CLANG_THIN), native arm64
# (no CROSS_COMPILE) → LLVM=1. A pinned LLVM release tarball, not apt's clang — see the
# compiler block below for why and how.
#
# CONFIG: allowlist model (Apple's approach). `make tinyconfig` (empty base, everything
# off) + one fragment turning ON only what a VZ virtio microVM + containers need
# (kernel/config/kernel-fragment). verify-config.sh guards the boot contract.
#
# Inputs — env: OSSEIN_DEBIAN_SUITE (REQUIRED, apt codename, must match the image),
# OSSEIN_APT_SNAPSHOT (REQUIRED, snapshot.debian.org timestamp), LOCALVERSION (REQUIRED,
# kernel version suffix, e.g. -ossein).
# Mounts from ossein-kernel: /kernel (source.tar.xz, build.sh, the fragment — vmlinux +
# perf out), /opt/llvm (the host-extracted, trimmed clang toolchain, read-only).

set -euo pipefail

LOCALVERSION="${LOCALVERSION:?required: kernel version suffix}"
SUITE="${OSSEIN_DEBIAN_SUITE:?required (e.g. trixie) — must match the base image codename}"
SNAPSHOT="${OSSEIN_APT_SNAPSHOT:?required — snapshot.debian.org archive timestamp, e.g. 20260701T025158Z}"

# arm64 ONLY
case "$(uname -m)" in
  aarch64|arm64) ;;
  *) echo "ossein builds its arm64 guest kernel natively; build VM is $(uname -m), not arm64" >&2; exit 1 ;;
esac

KARCH=arm64
IMAGE_PATH=arch/arm64/boot/Image
OUTPUT_NAME=kernel-arm64
PERF_OUTPUT_NAME=perf-arm64

# Base build deps (compiler is the tarball below, not apt). Dropped where the only
# consumer is disabled:
#   openssl/libssl-dev — module signing / cert extraction; MODULES=n
#   dwarves (pahole)   — DEBUG_INFO_BTF only; off
#   libelf-dev         — objtool + BTF only; both off (modpost has its own ELF parser)
#   kmod (depmod)      — module install; MODULES=n
#   cpio               — initramfs; BLK_DEV_INITRD off
#   wget/ca-certs      — nothing is fetched in-container (tarballs staged in host-side)
# Kept: bc/bison/flex/make (kconfig + build), xz-utils (extract source.tar.xz), python3
# (a few build sub-steps shell to it), libc6-dev (glibc headers: HOSTCC/clang compiles the
# kernel's own host tools — fixdep, kconfig, modpost — against <sys/types.h> etc.; the slim
# base ships none, and it must be named explicitly under --no-install-recommends, which
# otherwise dragged it in transitively alongside an unwanted gcc).
# libxml2 is required by the llvm toolchain.
# patch applies kernel/patches/*.patch onto the pristine tarball (see the patch step below).
PACKAGES="bc bison flex make xz-utils python3 libc6-dev libxml2 patch"

export DEBIAN_FRONTEND=noninteractive

rm -f /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources
cat > /etc/apt/sources.list.d/debian.sources <<EOF
Types: deb
URIs: http://snapshot.debian.org/archive/debian/${SNAPSHOT}
Suites: ${SUITE}
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
EOF
# snapshot Release files carry a past Valid-Until; apt rejects them without this.
cat > /etc/apt/apt.conf.d/00snapshot <<EOF
Acquire::Check-Valid-Until "false";
Acquire::Retries "5";
EOF

# The Signed-By keyring above is assumed present (true for library/debian); check it, so a
# base-image flavor change fails here with a clear message, not a generic apt signature error.
[ -f /usr/share/keyrings/debian-archive-keyring.gpg ] || {
  echo "missing /usr/share/keyrings/debian-archive-keyring.gpg — base image lacks debian-archive-keyring" >&2
  exit 1
}

apt-get update -qq
apt-get install -y -qq --no-install-recommends ${PACKAGES}

# Compiler: the pinned LLVM release tarball (github.com/llvm/llvm-project/releases),
# downloaded + extracted + trimmed ON THE HOST by ossein-kernel and mounted read-only at
# /opt/llvm. The tarball pins the exact version forever.
#
# LLVM=1 makes the whole build clang/ld.lld (target link is LD=ld.lld, self-contained —
# the kernel supplies its own runtime, no libgcc). The KERNEL'S HOST TOOLS (fixdep,
# kconfig, modpost) are different: they link ordinary userland executables through the
# clang DRIVER, and the release clang defaults to the GNU toolchain — GNU `ld`, crtbeginS.o,
# libgcc, libgcc_s — none of which we ship (no binutils, no gcc, on purpose). So point every
# host link at LLVM's own runtime instead (all present in the trimmed toolchain):
#   -fuse-ld=lld        → /opt/llvm/bin/ld.lld, not GNU ld
#   --rtlib=compiler-rt → clang_rt.crtbegin.o + libclang_rt.builtins.a, not crt*S.o + libgcc
#   --unwindlib=none    → host tools are plain C (no exceptions) → skip libgcc_s/libunwind
# Passed as a quoted make command-line var (not MAKEARGS — it has spaces) so it beats the
# env; on every make call, since `make tinyconfig` already builds fixdep.
LLVM_DIR=/opt/llvm
export PATH="${LLVM_DIR}/bin:${PATH}"
MAKEARGS="ARCH=${KARCH} LLVM=1"
HOST_LDFLAGS="-fuse-ld=lld --rtlib=compiler-rt --unwindlib=none"
echo ">> toolchain: $("${LLVM_DIR}/bin/clang" --version | head -1) (pinned LLVM release tarball)"

# Deterministic build stamp: the kernel otherwise bakes a wall-clock timestamp into its
# version string, so identical inputs would differ. Derive it from the apt snapshot pin
# (YYYYMMDDThhmmssZ → ISO-8601 that GNU date parses), and fix USER/HOST. (Note: ThinLTO is
# still not bit-deterministic across builds — this removes the trivial nondeterminism only.)
export KBUILD_BUILD_TIMESTAMP="${SNAPSHOT:0:4}-${SNAPSHOT:4:2}-${SNAPSHOT:6:2}T${SNAPSHOT:9:2}:${SNAPSHOT:11:2}:${SNAPSHOT:13:2}Z"
export KBUILD_BUILD_USER=ossein
export KBUILD_BUILD_HOST=ossein

mkdir -p /kbuild
tar -xf /kernel/source.tar.xz -C /kbuild --strip-components=1

(
  cd /kbuild

  # Patches — kernel/patches/*.patch from the repo, staged at /kernel/patches. The source
  # tarball stays PRISTINE (pinned url + sha256); our deltas live as explicit, reviewable patch
  # files rather than a fork. Applied in lexical order, before anything is configured or built.
  # --forward + set -e means a patch that no longer applies FAILS THE BUILD on a kernel bump,
  # rather than silently vanishing — the same "fail on drift" contract as the config check.
  if [ -d /kernel/patches ]; then
    for p in /kernel/patches/*.patch; do
      [ -e "$p" ] || break   # no nullglob: an empty dir yields the literal glob
      echo ">> applying patch: $(basename "$p")"
      patch -p1 --batch --forward -i "$p"
    done
  fi

  # perf (cross-runtime bench tooling) — built FIRST so a perf problem fails fast, before the
  # long kernel compile. tools/perf ships in the kernel source and builds independently of the
  # kernel .config; build it with the same pinned clang and disable EVERY optional library
  # (NO_LIB*/NO_JEVENTS). `perf bench` (sched/syscall/futex/epoll/mem — the cross-runtime
  # latency probes) is pure libc, so this pulls in no extra apt -dev packages and the binary
  # runs in any plain debian container (just mount + exec it). NO_JEVENTS drops the pmu-events
  # JSON generation (needs a `python` interpreter + is only for `perf stat`/`record`, not bench).
  # Host tools (fixdep) link through the clang driver, and in the tools/ build that link reads
  # KBUILD_HOSTLDFLAGS — NOT HOSTLDFLAGS — so -fuse-ld=lld must go there or clang hunts for a GNU
  # `ld` we don't ship (posix_spawn: No such file). LDFLAGS covers perf's own (native) link.
  # set -e fails the build on any error (unknown NO_* vars are harmless no-ops to make).
  PERF_NO="NO_JEVENTS=1 NO_LIBELF=1 NO_LIBDW=1 NO_LIBUNWIND=1 NO_LIBTRACEEVENT=1 NO_LIBTRACEFS=1 \
NO_SLANG=1 NO_LIBPYTHON=1 NO_LIBPERL=1 NO_LIBNUMA=1 NO_LIBCAP=1 NO_LIBBPF=1 NO_BPF_SKEL=1 \
NO_LIBCRYPTO=1 NO_JVMTI=1 NO_LZMA=1 NO_ZSTD=1 NO_LIBZSTD=1 NO_DEMANGLE=1 NO_AUXTRACE=1 \
NO_CAPSTONE=1 NO_LIBPFM4=1 NO_LIBDEBUGINFOD=1"
  echo ">> building perf (bench tooling; all optional libs off) ..."
  # -f Makefile.perf skips tools/perf/Makefile, the -j wrapper — it does `unexport MAKEFLAGS`,
  # which drops our -s before the recursion (command-line vars still get through, so the NO_*
  # above are unaffected — only the flags are lost) and the build echoes every CC/LD. Calling
  # Makefile.perf directly is the wrapper's own documented escape hatch; all the wrapper adds
  # is -j, which we already pass. -s then reaches tools/scripts/Makefile.include, which sets
  # silent=1 (same quiet contract as the kernel's make -s below) and adds --no-print-directory
  # on its own recursions. Warnings and errors still print.
  # shellcheck disable=SC2086  # intentional word-split of MAKEARGS / PERF_NO
  make -s -C tools/perf -f Makefile.perf ${MAKEARGS} WERROR=0 ${PERF_NO} \
    KBUILD_HOSTLDFLAGS="${HOST_LDFLAGS}" HOSTLDFLAGS="${HOST_LDFLAGS}" LDFLAGS="${HOST_LDFLAGS}" \
    -j"$(nproc)" perf
  cp tools/perf/perf "/kernel/${PERF_OUTPUT_NAME}"
  echo ">> perf built: $(tools/perf/perf --version 2>/dev/null || echo '?')"

  # Allowlist model — start from tinyconfig (empty) and merge our single fragment. The make
  # steps carry LLVM=1 so kconfig sees CC_IS_CLANG/LD_IS_LLD and the ThinLTO choice is
  # selectable; merge_config.sh runs with only ARCH= (its -m mode just concatenates text and
  # never invokes kconfig — the real evaluation is olddefconfig, which has LLVM=1).
  make ${MAKEARGS} HOSTLDFLAGS="${HOST_LDFLAGS}" tinyconfig

  # THE RECURRING TRAP, made loud. kernel/configs/tiny.config is our BASE and it is five
  # lines — every one a speed-for-size trade (-Os, SLUB_TINY, the NR_CPUS numeric, ...).
  # `EXPERT=y` + an allnoconfig base answers "n" to every prompt nobody names and takes the
  # default for every numeric nobody names — silently. So: every symbol tiny.config sets MUST
  # appear in our fragment. Agreeing or disagreeing is fine; inheriting it without a decision
  # is not. A kernel bump that adds a sixth line fails here instead of shipping unnamed.
  unnamed=0
  while IFS= read -r line; do
    case "$line" in
      CONFIG_*=*) sym=${line%%=*} ;;
      *) continue ;;
    esac
    if ! grep -q "\b${sym}\b" /kernel/kernel-fragment; then
      echo "UNNAMED tinyconfig default: ${sym} (tiny.config sets '${line}')" >&2
      echo "  -> name it in kernel/config/kernel-fragment: keep it, or turn it off, but DECIDE." >&2
      unnamed=1
    fi
  done < kernel/configs/tiny.config
  [ "$unnamed" -eq 0 ] || { echo "tinyconfig base has unnamed defaults — see above" >&2; exit 1; }
  ARCH="${KARCH}" scripts/kconfig/merge_config.sh -m .config \
    /kernel/kernel-fragment
  make ${MAKEARGS} HOSTLDFLAGS="${HOST_LDFLAGS}" olddefconfig

  # Assert every fragment line survived olddefconfig — it can silently revert a symbol whose
  # deps aren't met, and `merge_config -m` skips that check. Fail the BUILD (before the long
  # compile + promote) rather than relying on the external, post-promotion verify step.
  drift=0
  while IFS= read -r line; do
    case "$line" in
      CONFIG_*=*)
        if ! grep -qxF "$line" .config; then
          echo "CONFIG DRIFT: '$line' reverted (.config: $(grep "^${line%%=*}=" .config || echo unset))" >&2
          drift=1
        fi ;;
      "# CONFIG_"*" is not set")
        sym=${line#\# }; sym=${sym% is not set}
        if grep -q "^${sym}=" .config; then
          echo "CONFIG DRIFT: '$line' but .config has $(grep "^${sym}=" .config)" >&2
          drift=1
        fi ;;
    esac
  done < /kernel/kernel-fragment
  [ "$drift" -eq 0 ] || { echo "config drift after olddefconfig — aborting before promote" >&2; exit 1; }

  # Plain lines (set -e aborts on any failure) — clearer in CI than a chained &&.
  make -s ${MAKEARGS} HOSTLDFLAGS="${HOST_LDFLAGS}" -j"$(nproc)" LOCALVERSION="${LOCALVERSION}"
  cp "${IMAGE_PATH}" "/kernel/${OUTPUT_NAME}"
)
