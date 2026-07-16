#!/usr/bin/env bash
# Merge an overlay Kconfig fragment onto a base one, OVERRIDE-style: any symbol the overlay
# decides is removed from the base first, so the result never contradicts itself.
#
# Why not `cat base overlay`: merge_config.sh would resolve the conflict (last line wins), but
# build.sh's post-olddefconfig drift check walks EVERY line of the fragment and asserts it
# survived. A fragment containing both "# CONFIG_KALLSYMS is not set" and "CONFIG_KALLSYMS=y"
# fails that check by construction — correctly, since it is not a coherent statement of intent.
#
# usage: merge-fragments.sh <base> <overlay> > merged
set -uo pipefail
BASE="${1:?usage: merge-fragments.sh <base> <overlay>}"
OVERLAY="${2:?usage: merge-fragments.sh <base> <overlay>}"

# Symbols the overlay decides (either "CONFIG_X=..." or "# CONFIG_X is not set").
# Space-separated: `awk -v` cannot carry embedded newlines ("newline in string") and fails
# silently-ish, emitting garbage instead of a fragment.
syms=$(sed -nE 's/^(CONFIG_[A-Z0-9_]+)=.*/\1/p; s/^# (CONFIG_[A-Z0-9_]+) is not set$/\1/p' "$OVERLAY" | sort -u | tr '\n' ' ')

awk -v syms="$syms" '
  BEGIN { n = split(syms, a, " "); for (i = 1; i <= n; i++) if (a[i] != "") drop[a[i]] = 1 }
  {
    sym = ""
    if (match($0, /^CONFIG_[A-Z0-9_]+=/))            { sym = substr($0, RSTART, RLENGTH - 1) }
    else if (match($0, /^# CONFIG_[A-Z0-9_]+ is not set$/)) { sym = $2 }
    if (sym != "" && (sym in drop)) next   # overlay decides this one; drop the base line
    print
  }
' "$BASE"
echo
echo "# ---- overlay: $(basename "$OVERLAY") ----"
cat "$OVERLAY"
