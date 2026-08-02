#!/usr/bin/env bash
# Verify that the SHIPPED guest kernel actually carries the config we intended —
# not what the fragment says, but what survived `make olddefconfig`. This exists
# because merge_config.sh + olddefconfig silently revert fragment lines whose
# symbol is not user-settable (e.g. KPTI/BHB when EXPERT is off) or whose choice
# sibling wasn't disabled (e.g. PREEMPT_LAZY vs PREEMPT_NONE). Trust the binary.
#
# It extracts the embedded IKCONFIG from the built Image (CONFIG_IKCONFIG=y) and
# asserts each perf/threat-model decision.
#
# usage: verify-config.sh [kernel-arm64]       verify (default: build/kernel-arm64)
#        verify-config.sh --extract [kernel]    dump the embedded .config to stdout
#        verify-config.sh --golden  [kernel]    emit the normalized golden symbol set
set -uo pipefail

HERE="$(cd "$(dirname "$0")/.." && pwd)"     # repo root (script lives in kernel/)

# Extract the gzipped IKCONFIG blob that follows the IKCFG_ST marker. The stock
# scripts/extract-ikconfig relies on GNU `tr` and fails on macOS ("Illegal byte
# sequence"), so we decompress directly with python's zlib.
extract() {  # <kernel-image> -> .config on stdout
  python3 - "$1" <<'PY'
import zlib, sys
data = open(sys.argv[1], 'rb').read()
i = data.find(b'IKCFG_ST')
if i < 0:
    sys.stderr.write("no IKCFG_ST marker (CONFIG_IKCONFIG not set?)\n"); sys.exit(2)
sys.stdout.write(zlib.decompressobj(16 + zlib.MAX_WBITS).decompress(data[i+8:]).decode('utf-8', 'replace'))
PY
}

# golden_filter normalizes an extracted .config down to the golden set: every decided
# symbol — =y/=m AND numerics/hex/strings (see the golden-guard note below for why
# non-bools matter). This is the SINGLE owner of that definition: the golden diff
# below and `just kernel-golden` (via --golden) both come through here, so the
# committed snapshot and the drift check can never normalize differently.
golden_filter() {
  LC_ALL=C grep -E '^CONFIG_[A-Z0-9_]+=(y|m|[0-9]+|0x[0-9a-fA-F]+|\".*\")$' | LC_ALL=C sort
}

# Mode dispatch — the plumbing halves of this script, exposed for `just kernel-golden`
# (and debugging) so extraction/normalization are never re-implemented elsewhere.
# The default, flag-less invocation verifies.
MODE=verify
case "${1:-}" in
  --extract) MODE=extract; shift ;;
  --golden)  MODE=golden;  shift ;;
esac

K="${1:-$HERE/build/kernel-arm64}"

[ -f "$K" ] || { echo "no kernel at $K (build it first: just kernel)" >&2; exit 1; }

case "$MODE" in
  extract) extract "$K"; exit ;;
  golden)  extract "$K" | golden_filter; exit ;;
esac

CFG="$(mktemp "$(dirname "$K")/.verify-config.XXXXXX")"; trap 'rm -f "$CFG"' EXIT
extract "$K" > "$CFG" || exit 1
[ -s "$CFG" ] || { echo "extracted empty config from $K" >&2; exit 1; }

# state <SYMBOL>: prints y | m | n | absent  (n = explicit "is not set")
state() {
  local s="$1"
  if   grep -q "^CONFIG_${s}=y" "$CFG";            then echo y
  elif grep -q "^CONFIG_${s}=m" "$CFG";            then echo m
  elif grep -q "^# CONFIG_${s} is not set" "$CFG"; then echo n
  else echo absent; fi
}

fail=0
# want <SYMBOL> <expected: y|not-y> [why]
want() {
  local s="$1" exp="$2" why="${3:-}" got; got="$(state "$s")"
  local ok=0
  case "$exp" in
    y)      [ "$got" = y ] && ok=1 ;;
    not-y)  [ "$got" != y ] && ok=1 ;;   # n or absent both fine
  esac
  if [ "$ok" = 1 ]; then
    printf "  \033[32mok\033[0m   %-38s = %-6s  %s\n" "CONFIG_$s" "$got" "$why"
  else
    printf "  \033[31mFAIL\033[0m %-38s = %-6s  (want %s) %s\n" "CONFIG_$s" "$got" "$exp" "$why"
    fail=1
  fi
}

echo "=== verifying shipped config: $(grep -m1 'Kernel Configuration' "$CFG" | sed 's/^# //') ==="
echo "--- root enabler ---"
want EXPERT                          y     "gates the KPTI/BHB prompts below"
echo "--- preemption (7.x arm64: NONE/VOLUNTARY unavailable; LAZY is lightest) ---"
want PREEMPT_LAZY                    y     "lightest model selectable on 7.x arm64"
want PREEMPT                         not-y "full preemption = heaviest non-RT (measured: no sched-pipe gain)"
want PREEMPT_DYNAMIC                 not-y "no runtime-switchable preempt indirection"
# PREEMPTION=y + PREEMPT_COUNT=y are unavoidable on stock 7.x arm64 (upstream
# deprecated the non-preemptible build) — a deliberate ~1-2ns vs Apple's 6.18
# PREEMPT_NONE that we accept rather than fight (see the fragment step 2 note).
echo "--- arch hardening (VM boundary is the containment) ---"
want ARM64_PTR_AUTH                  not-y "user PAC key mgmt"
want ARM64_PTR_AUTH_KERNEL           not-y "in-kernel signing codegen"
want ARM64_BTI                       not-y
want ARM64_POE                       not-y
want ARM64_GCS                       not-y
want UNMAP_KERNEL_AT_EL0             not-y "KPTI (also runtime-off via mitigations=off)"
want MITIGATE_SPECTRE_BRANCH_HISTORY not-y "BHB (also runtime-off via mitigations=off)"
want INIT_STACK_NONE                 y     "no per-call stack-var zeroing (Apple's choice)"
want INIT_STACK_ALL_ZERO             not-y "must lose the choice to NONE"
want LTO_CLANG_THIN                  y     "clang ThinLTO (build.sh builds with LLVM=1)"
want ARM64_4K_PAGES                  y     "Rosetta (amd64 containers) requires 4K guest pages"
echo "--- core plumbing (tinyconfig strips these; the allowlist MUST restore them) ---"
want CC_OPTIMIZE_FOR_PERFORMANCE     y     "-O2 not tinyconfig's -Os"
want SMP                             y     "2+ vCPUs"
want PRINTK                          y     "console output at all"
want TTY                             y     "console + PTYs"
want BLOCK                           y     "virtio-blk / ext4"
want NET                             y     "networking core"
want INET                            y     "IPv4"
want UNIX                            y     "AF_UNIX (dockerd/containerd/gRPC)"
want FUTEX                           y     "glibc/pthreads/Go runtime"
want BLK_DEV_INITRD                  y     "initramfs root (ossein boots a cpio via rdinit=, no root device)"
want BINFMT_ELF                      y     "run ELF binaries"
want BINFMT_SCRIPT                   y     "run #! scripts"
want FILE_LOCKING                    y     "fcntl/flock — apt/dpkg/git/DBs; EXPERT-trap, every container needs it"
want ADVISE_SYSCALLS                 y     "posix_fadvise/madvise; EXPERT-trap"
want INOTIFY_USER                    y     "file watching — countless container tools need it"
want SHMEM                           y     "tmpfs backing (EXPERT-hidden; traps easily)"
want NETDEVICES                      y     "menu gate over virtio-net/veth/tun"
want VIRTIO_MENU                     y     "menu gate over virtio-pci transport"
want PCI                             y     "VZ virtio-over-PCI"
want ARM_GIC_V3                      y     "interrupt controller"
want ARM_ARCH_TIMER                  y     "arch timer"
want ARM_PSCI_FW                     y     "SMP boot + power-off"
echo "--- boot contract (must always be on) ---"
want VIRTIO_VSOCKETS                 y     "agent lifeline"
want IKCONFIG                        y     "this check depends on it"
want VIRTIO_PCI                      y     "virtio transport"
want VIRTIO_BLK                      y     "root/cache disk"
want VIRTIO_NET                      y     "guest networking"
want VIRTIO_CONSOLE                  y     "console"
want VIRTIO_FS                       y     "host<->guest shares"
want FUSE_FS                         y     "virtio-fs needs fuse"
want EXT4_FS                         y     "root disk fs"
want OVERLAY_FS                      y     "container rootfs"
want TMPFS                           y     "rootfs backing"
want DEVTMPFS                        y     "/dev"
want POSIX_MQUEUE                     y     "OCI default mount /dev/mqueue (stage2 mounts it)"
want UNIX98_PTYS                     y     "OCI default mount /dev/pts (devpts)"
want VETH                            y     "container net"
want BRIDGE                          not-y "cut 2026-07-16 — proven inert by a buildkit network-RUN test (B8)"
want CGROUP_BPF                      y     "runc cgroup2 device policy (buildkit RUN); else bpf_prog_query EINVAL"
want STACKPROTECTOR_STRONG           y     "kernel stack canary (correctness net; +192KB, <0.5% runtime)"
want SERIAL_AMBA_PL011_CONSOLE       y     "VZ serial console"
echo "--- allowlist sanity (never named → must be OFF; proves the empty base held) ---"
want USB_SUPPORT                     not-y "no USB in VZ"
want SOUND                           not-y "no audio"
want DRM                             not-y "headless"
want WLAN                            not-y "no wifi"
want WIRELESS                        not-y "no wifi subsystem"
want ETHERNET                        not-y "physical-NIC driver menu (~69 NET_VENDOR_* stubs); virtio-net doesn't need it"
want NETWORK_FILESYSTEMS             not-y "no NFS/CIFS; ext4/overlay/tmpfs/virtio-fs only"
want ATA                             not-y "virtio-blk only"
want ARCH_APPLE                      not-y "VZ guest, not bare-metal SoC"
want KALLSYMS_ALL                    not-y "full symbol table = Image fat (was on under defconfig)"
echo "--- deliberate cuts (must STAY off; if any reverts, something selects it) ---"
want SWAP                            not-y "no swap device ever attached"
want DEBUG_INFO                      not-y "no debug symbols / Image bloat (DEBUG_KERNEL itself is force-selected by EXPERT, so guard the real feature)"
want CFS_BANDWIDTH                   not-y "OCI spec sets no cpu limits (cgroup cpu ctrl cut)"
want BLK_CGROUP                      not-y "OCI spec sets no io limits (cgroup io ctrl cut)"
want KALLSYMS                        not-y "cut: ~1ns getpid (layout); debug-only, re-add locally to chase a panic"
want COMPACTION                      not-y "cut: part of the ~1ns getpid layout regression"
want MIGRATION                       not-y "cut: part of the ~1ns getpid layout regression"

# Golden-config guard: the curated `want` list above only checks named symbols; this catches
# the UNnamed drift — a new default-y symbol riding in through an opened gate on a kernel bump,
# or an expected one silently dropping. Fail on ANY change vs the committed snapshot.
# Regenerate deliberately: just kernel-golden.
#
# NUMERICS AND STRINGS ARE INCLUDED, not just =y/=m: a =y-only golden is blind to an entire
# class of trap. arm64 defaults NR_CPUS=512, which force-selects CPUMASK_OFFSTACK (every
# cpumask_var_t heap-allocated) — invisible to a =y-only guard because `CONFIG_NR_CPUS=512`
# is not a bool.
GOLDEN="$HERE/kernel/config/kernel-golden"
if [ -f "$GOLDEN" ]; then
  got="$(mktemp)"; golden_filter < "$CFG" > "$got"
  added="$(LC_ALL=C comm -13 "$GOLDEN" "$got")"    # in shipped only → newly ON
  removed="$(LC_ALL=C comm -23 "$GOLDEN" "$got")"  # in golden only → now OFF
  rm -f "$got"
  if [ -n "$added$removed" ]; then
    echo "--- golden-config DRIFT vs kernel/config/kernel-golden ---"
    [ -n "$added" ]   && { echo "  turned ON (unexpected — new default-y or drift):"; printf '%s\n' "$added"   | sed 's/^/    + /'; }
    [ -n "$removed" ] && { echo "  now OFF (expected symbol lost):";                  printf '%s\n' "$removed" | sed 's/^/    - /'; }
    echo "  → review the delta; if intentional, run 'just kernel-golden' and commit kernel/config/kernel-golden" >&2
    fail=1
  else
    echo "--- golden-config: no drift ($(wc -l < "$GOLDEN" | tr -d ' ') enabled symbols match kernel/config/kernel-golden) ---"
  fi
else
  echo "--- golden-config: no kernel/config/kernel-golden committed (run 'just kernel-golden' to create it) ---"
fi

echo
if [ "$fail" = 0 ]; then echo "config OK: every intended decision survived, and no unexpected symbol drift."; else
  echo "CONFIG DRIFT: a fragment line reverted or the enabled-symbol set changed — see FAIL/DRIFT above." >&2; fi
exit "$fail"
