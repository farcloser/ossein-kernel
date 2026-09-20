package ikconfig

// Policy is every perf and threat-model decision the shipped kernel must carry,
// checked against the config the binary embeds rather than the fragment that
// asked for it: merge_config.sh + olddefconfig silently revert fragment lines
// whose symbol is not user-settable (KPTI/BHB when EXPERT is off) or whose
// choice sibling was not disabled (PREEMPT_LAZY vs PREEMPT_NONE). Trust the
// binary. The reasons are the record of each choice; change them with it.
//
//nolint:gochecknoglobals // immutable table.
var Policy = []Section{
	{Title: "root enabler", Decisions: []Decision{
		{"EXPERT", WantY, "gates the KPTI/BHB prompts below"},
	}},
	{Title: "preemption (7.x arm64: NONE/VOLUNTARY unavailable; LAZY is lightest)", Decisions: []Decision{
		{"PREEMPT_LAZY", WantY, "lightest model selectable on 7.x arm64"},
		{"PREEMPT", WantNotY, "full preemption = heaviest non-RT (measured: no sched-pipe gain)"},
		{"PREEMPT_DYNAMIC", WantNotY, "no runtime-switchable preempt indirection"},
		// PREEMPTION=y + PREEMPT_COUNT=y are unavoidable on stock 7.x arm64 (upstream
		// deprecated the non-preemptible build): a deliberate ~1-2ns vs Apple's 6.18
		// PREEMPT_NONE, accepted rather than fought (see the fragment step 2 note).
	}},
	{Title: "arch hardening (VM boundary is the containment)", Decisions: []Decision{
		{"ARM64_PTR_AUTH", WantNotY, "user PAC key mgmt"},
		{"ARM64_PTR_AUTH_KERNEL", WantNotY, "in-kernel signing codegen"},
		{"ARM64_BTI", WantNotY, ""},
		{"ARM64_POE", WantNotY, ""},
		{"ARM64_GCS", WantNotY, ""},
		{"UNMAP_KERNEL_AT_EL0", WantNotY, "KPTI (also runtime-off via mitigations=off)"},
		{"MITIGATE_SPECTRE_BRANCH_HISTORY", WantNotY, "BHB (also runtime-off via mitigations=off)"},
		{"INIT_STACK_NONE", WantY, "no per-call stack-var zeroing (Apple's choice)"},
		{"INIT_STACK_ALL_ZERO", WantNotY, "must lose the choice to NONE"},
		{"LTO_CLANG_THIN", WantY, "clang ThinLTO (build.sh builds with LLVM=1)"},
		{"ARM64_4K_PAGES", WantY, "Rosetta (amd64 containers) requires 4K guest pages"},
	}},
	{Title: "core plumbing (tinyconfig strips these; the allowlist MUST restore them)", Decisions: []Decision{
		{"CC_OPTIMIZE_FOR_PERFORMANCE", WantY, "-O2 not tinyconfig's -Os"},
		{"SMP", WantY, "2+ vCPUs"},
		{"PRINTK", WantY, "console output at all"},
		{"TTY", WantY, "console + PTYs"},
		{"BLOCK", WantY, "virtio-blk / ext4"},
		{"NET", WantY, "networking core"},
		{"INET", WantY, "IPv4"},
		{"UNIX", WantY, "AF_UNIX (dockerd/containerd/gRPC)"},
		{"FUTEX", WantY, "glibc/pthreads/Go runtime"},
		{"BLK_DEV_INITRD", WantY, "initramfs root (ossein boots a cpio via rdinit=, no root device)"},
		{"BINFMT_ELF", WantY, "run ELF binaries"},
		{"BINFMT_SCRIPT", WantY, "run #! scripts"},
		{"FILE_LOCKING", WantY, "fcntl/flock — apt/dpkg/git/DBs; EXPERT-trap, every container needs it"},
		{"ADVISE_SYSCALLS", WantY, "posix_fadvise/madvise; EXPERT-trap"},
		{"INOTIFY_USER", WantY, "file watching — countless container tools need it"},
		{"SHMEM", WantY, "tmpfs backing (EXPERT-hidden; traps easily)"},
		{"NETDEVICES", WantY, "menu gate over virtio-net/veth/tun"},
		{"VIRTIO_MENU", WantY, "menu gate over virtio-pci transport"},
		{"PCI", WantY, "VZ virtio-over-PCI"},
		{"ARM_GIC_V3", WantY, "interrupt controller"},
		{"ARM_ARCH_TIMER", WantY, "arch timer"},
		{"ARM_PSCI_FW", WantY, "SMP boot + power-off"},
	}},
	{Title: "boot contract (must always be on)", Decisions: []Decision{
		{"VIRTIO_VSOCKETS", WantY, "agent lifeline"},
		{"IKCONFIG", WantY, "this check depends on it"},
		{"VIRTIO_PCI", WantY, "virtio transport"},
		{"VIRTIO_BLK", WantY, "root/cache disk"},
		{"VIRTIO_NET", WantY, "guest networking"},
		{"VIRTIO_CONSOLE", WantY, "console"},
		{"VIRTIO_FS", WantY, "host<->guest shares"},
		{"FUSE_FS", WantY, "virtio-fs needs fuse"},
		{"EXT4_FS", WantY, "root disk fs"},
		{"OVERLAY_FS", WantY, "container rootfs"},
		{"TMPFS", WantY, "rootfs backing"},
		{"DEVTMPFS", WantY, "/dev"},
		{
			"EROFS_FS",
			WantY,
			"rootfs lower (mount-instead-of-extract materialization; erofs blob + no driver = boot ENODEV)",
		},
		{"EROFS_FS_XATTR", WantY, "image xattrs"},
		{"EROFS_FS_SECURITY", WantY, "security.capability on image files"},
		{
			"EROFS_FS_ZIP",
			WantNotY,
			"uncompressed images only — ZIP drags LZ4/XZ decompressors into the kernel for nothing",
		},
		{"POSIX_MQUEUE", WantY, "OCI default mount /dev/mqueue (stage2 mounts it)"},
		{"UNIX98_PTYS", WantY, "OCI default mount /dev/pts (devpts)"},
		{"VETH", WantY, "container net"},
		{"BRIDGE", WantNotY, "cut 2026-07-16 — proven inert by a buildkit network-RUN test (B8)"},
		{"CGROUP_BPF", WantY, "runc cgroup2 device policy (buildkit RUN); else bpf_prog_query EINVAL"},
		{"STACKPROTECTOR_STRONG", WantY, "kernel stack canary (correctness net; +192KB, <0.5% runtime)"},
		{"SERIAL_AMBA_PL011_CONSOLE", WantY, "VZ serial console"},
	}},
	{Title: "allowlist sanity (never named → must be OFF; proves the empty base held)", Decisions: []Decision{
		{"USB_SUPPORT", WantNotY, "no USB in VZ"},
		{"SOUND", WantNotY, "no audio"},
		{"DRM", WantNotY, "headless"},
		{"WLAN", WantNotY, "no wifi"},
		{"WIRELESS", WantNotY, "no wifi subsystem"},
		{"ETHERNET", WantNotY, "physical-NIC driver menu (~69 NET_VENDOR_* stubs); virtio-net doesn't need it"},
		{"NETWORK_FILESYSTEMS", WantNotY, "no NFS/CIFS; ext4/overlay/tmpfs/virtio-fs only"},
		{"ATA", WantNotY, "virtio-blk only"},
		{"ARCH_APPLE", WantNotY, "VZ guest, not bare-metal SoC"},
		{"KALLSYMS_ALL", WantNotY, "full symbol table = Image fat (was on under defconfig)"},
	}},
	{Title: "deliberate cuts (must STAY off; if any reverts, something selects it)", Decisions: []Decision{
		{"SWAP", WantNotY, "no swap device ever attached"},
		{
			"DEBUG_INFO",
			WantNotY,
			"no debug symbols / Image bloat (DEBUG_KERNEL itself is force-selected by EXPERT, so guard the real feature)",
		},
		{"CFS_BANDWIDTH", WantNotY, "OCI spec sets no cpu limits (cgroup cpu ctrl cut)"},
		{"BLK_CGROUP", WantNotY, "OCI spec sets no io limits (cgroup io ctrl cut)"},
		{"KALLSYMS", WantNotY, "cut: ~1ns getpid (layout); debug-only, re-add locally to chase a panic"},
		{"COMPACTION", WantNotY, "cut: part of the ~1ns getpid layout regression"},
		{"MIGRATION", WantNotY, "cut: part of the ~1ns getpid layout regression"},
	}},
}
