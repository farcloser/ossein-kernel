//go:build linux

// Command ossein-init is the build microVM's PID 1: one static linux/arm64 binary
// (CGO_ENABLED=0) embedded in the Debian rootfs at /ossein-init:
//  1. mounts the base pseudo-filesystems + a tmpfs /kbuild scratch,
//  2. mounts the virtio-fs shares (/kernel rw always; /opt/llvm ro when ossein.llvm=1),
//  3. brings up the NAT NIC via DHCP when ossein.net=1 is on the cmdline,
//  4. runs `/bin/bash /kernel/build.sh` with the env from /kernel/build.env,
//  5. writes the child's exit code to /kernel/status and powers off.
//
// The host learns pass/fail by reading /kernel/status after the VM stops — the
// status file is this design's replacement for vsock's Wait() exit code.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// version is stamped at build time (-X main.version) to match the shared go build task.
var version = "dev"

// errNoEthernet reports that the virtio NIC never appeared. A package-level sentinel so
// the boot path can match on it rather than on message text.
var errNoEthernet = errors.New("no ethernet interface found")

// initFailedStatus is written to /kernel/status when init itself fails before the
// build's own exit code is available — distinguishes "init broke" from "build failed".
const initFailedStatus = 254

const (
	kernelShare = "/kernel"
	statusPath  = "/kernel/status"
	buildScript = "/kernel/build.sh"
	buildLog    = "/kernel/build.log"
	envFile     = "/kernel/build.env"

	// overlayMountpoint is the baked-in dir (see internal/rootfs) where the init mounts the
	// tmpfs that backs the writable overlay root.
	overlayMountpoint = "/ram"

	// fsTmpfs is both the source and the fstype of every tmpfs in the base mount table.
	fsTmpfs = "tmpfs"

	// The two error shapes the mount and file paths repeat.
	errFmtMkdir = "mkdir %s: %w"
	errFmtOpen  = "open %s: %w"

	// Modes for everything PID 1 creates. Deliberately world-readable: the guest's system
	// directories must stay traversable by unprivileged processes (apt drops to _apt for
	// downloads), and the host reads the log and status files back off the virtio-fs
	// share. Note main sets umask 0, so these land verbatim.
	dirMode  = 0o755
	fileMode = 0o644
)

func main() {
	// PID 1 must survive a broken stdio pipe, and file modes must land verbatim.
	signal.Ignore(syscall.SIGPIPE)
	unix.Umask(0)

	// Boot banner on the console (hvc0) — the kernel has already probed it by the time PID 1
	// runs, so this reaches the host. version is build-stamped (-X main.version).
	fmt.Fprintf(os.Stderr, "ossein-init %s: booting\n", version)

	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ossein-init: %v\n", err)

		if code == 0 {
			code = initFailedStatus
		}
	}

	// Record the outcome durably BEFORE powering off: writeStatus fsyncs the file (and the
	// share dir) through virtio-fs, which blocks until the host copy is flushed — so the
	// hand-off is deterministic, no "sleep and hope the cache drains". panic=-1 covers a
	// reboot that somehow fails.
	if werr := writeStatus(code); werr != nil {
		fmt.Fprintf(os.Stderr, "ossein-init: writing status: %v\n", werr)
	}

	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)

	// Unreachable on success; if reboot returns, spin so we don't exec anything else.
	select {}
}

// run performs PID-1 setup and runs the build, returning its exit code.
func run() (int, error) {
	// FIRST: pivot from the read-only ext4 root into a tmpfs-backed overlay, so every root
	// write (apt install, /var, dpkg) lands in RAM and the disk image stays pristine.
	if err := setupOverlayRoot(); err != nil {
		return initFailedStatus, fmt.Errorf("overlay root: %w", err)
	}

	if err := mountBase(); err != nil {
		return initFailedStatus, fmt.Errorf("base mounts: %w", err)
	}

	if err := mountShares(); err != nil {
		return initFailedStatus, fmt.Errorf("virtiofs shares: %w", err)
	}

	if cmdlineHas("ossein.net=1") {
		if err := bringUpNetwork(); err != nil {
			return initFailedStatus, fmt.Errorf("network: %w", err)
		}
	}

	code, err := runBuild()
	if err != nil {
		return code, fmt.Errorf("build: %w", err)
	}

	return code, nil
}

// setupOverlayRoot turns the read-only ext4 root into a writable one WITHOUT touching the
// disk: mount a tmpfs, build an overlay (lowerdir = the ro root, upper+work on the tmpfs),
// and pivot_root into it. Every subsequent write lands in the tmpfs (RAM); the ext4 image is
// never mutated, so all boots share one pristine disk with no per-build clone.
func setupOverlayRoot() error {
	if err := unix.Mount("tmpfs", overlayMountpoint, "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount tmpfs on %s: %w", overlayMountpoint, err)
	}

	lower := overlayMountpoint + "/lower"
	upper := overlayMountpoint + "/upper"
	work := overlayMountpoint + "/work"
	newRoot := overlayMountpoint + "/root"

	for _, d := range []string{lower, upper, work, newRoot} {
		if err := os.Mkdir(d, dirMode); err != nil {
			return fmt.Errorf(errFmtMkdir, d, err)
		}
	}

	// Bind / to a stable path for the overlay lowerdir (the ro ext4 root).
	if err := unix.Mount("/", lower, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind / -> %s: %w", lower, err)
	}

	opts := "lowerdir=" + lower + ",upperdir=" + upper + ",workdir=" + work
	if err := unix.Mount("overlay", newRoot, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay (%s): %w", opts, err)
	}

	oldRoot := newRoot + "/oldroot"
	if err := os.Mkdir(oldRoot, dirMode); err != nil { // writable: lands in the overlay upper
		return fmt.Errorf(errFmtMkdir, oldRoot, err)
	}

	if err := unix.PivotRoot(newRoot, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}

	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}

	// Tidy the mount tree: detach the old ro-root subtree. overlayfs keeps the lower/upper
	// references it needs, so this is best-effort and safe if it fails.
	_ = unix.Unmount("/oldroot", unix.MNT_DETACH)

	return nil
}

type mountSpec struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

func mountBase() error {
	// Order matters: parents before children. /dev is usually already mounted by the
	// kernel (DEVTMPFS_MOUNT), so its mount tolerates EBUSY.
	for _, spec := range []mountSpec{
		{"proc", "/proc", "proc", 0, ""},
		{"sysfs", "/sys", "sysfs", 0, ""},
		{"devtmpfs", "/dev", "devtmpfs", 0, ""},
		{"devpts", "/dev/pts", "devpts", 0, "gid=5,mode=620"},
		{fsTmpfs, "/dev/shm", fsTmpfs, 0, ""},
		{fsTmpfs, "/run", fsTmpfs, 0, ""},
		{fsTmpfs, "/tmp", fsTmpfs, 0, ""},
		// /kbuild is the build scratch: a real fs the guest kernel owns (NOT virtiofs —
		// virtiofsd can't honour the guest root's DAC override). Sized by VM RAM.
		{fsTmpfs, "/kbuild", fsTmpfs, 0, ""},
	} {
		if err := os.MkdirAll(spec.target, dirMode); err != nil {
			return fmt.Errorf(errFmtMkdir, spec.target, err)
		}

		if err := unix.Mount(spec.source, spec.target, spec.fstype, spec.flags, spec.data); err != nil {
			if spec.target == "/dev" && errors.Is(err, unix.EBUSY) {
				continue // kernel already mounted devtmpfs
			}

			return fmt.Errorf("mount %s on %s: %w", spec.fstype, spec.target, err)
		}
	}

	return nil
}

func mountShares() error {
	// /kernel is always present (build AND smoke test). Required.
	if err := mountVirtiofs("kernel", kernelShare); err != nil {
		return err
	}

	// /opt/llvm exists only on the build VM, so the host DECLARES it (ossein.llvm=1 on
	// the cmdline, same idiom as ossein.net=1) rather than the init probing for it.
	// Declared → the mount is hard-required: a real failure (missing host dir, virtiofsd
	// trouble) fails the boot here, not minutes later as a baffling "clang: not found"
	// mid-build. Undeclared (the smoke-test VM) → not attempted at all.
	if cmdlineHas("ossein.llvm=1") {
		if err := mountVirtiofs("llvm", "/opt/llvm"); err != nil {
			return err
		}
	}

	return nil
}

// mountVirtiofs mounts a virtio-fs share (addressed by tag, set host-side in internal/vm).
func mountVirtiofs(tag, target string) error {
	if err := os.MkdirAll(target, dirMode); err != nil {
		return fmt.Errorf(errFmtMkdir, target, err)
	}

	if err := unix.Mount(tag, target, "virtiofs", 0, ""); err != nil {
		return fmt.Errorf("mount virtiofs %s on %s: %w", tag, target, err)
	}

	return nil
}

// cmdlineHas reports whether token is present on the kernel cmdline — the host's
// channel for declaring what this boot needs (ossein.net=1, ossein.llvm=1).
func cmdlineHas(token string) bool {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}

	return slices.Contains(strings.Fields(string(data)), token)
}

// bringUpNetwork finds the virtio NIC, DHCPs against VZ's NAT, and installs the
// address, default route, and resolv.conf so apt can reach snapshot.debian.org.
func bringUpNetwork() error {
	link, err := findEthernet()
	if err != nil {
		return err
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link up %s: %w", link.Attrs().Name, err)
	}

	client, err := nclient4.New(link.Attrs().Name)
	if err != nil {
		return fmt.Errorf("dhcp client on %s: %w", link.Attrs().Name, err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	lease, err := client.Request(ctx)
	if err != nil {
		return fmt.Errorf("dhcp request: %w", err)
	}

	ack := lease.ACK

	mask := ack.SubnetMask()
	if mask == nil {
		mask = net.CIDRMask(24, 32) // VZ NAT is a /24; fall back if the option is absent
	}

	addr := &netlink.Addr{IPNet: &net.IPNet{IP: ack.YourIPAddr, Mask: mask}}
	if err := netlink.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("assign %s: %w", addr, err)
	}

	if routers := ack.Router(); len(routers) > 0 {
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Gw: routers[0]}
		if err := netlink.RouteAdd(route); err != nil {
			return fmt.Errorf("default route via %s: %w", routers[0], err)
		}
	}

	if err := writeResolvConf(ack.DNS()); err != nil {
		return fmt.Errorf("resolv.conf: %w", err)
	}

	return nil
}

// findEthernet returns the first non-loopback link (the single virtio-net NIC).
func findEthernet() (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}

	for _, l := range links {
		if l.Attrs().Name != "lo" && l.Type() != "" {
			return l, nil
		}
	}

	return nil, errNoEthernet
}

func writeResolvConf(servers []net.IP) error {
	var builder strings.Builder
	for _, s := range servers {
		// strings.Builder.WriteString is documented never to fail.
		_, _ = builder.WriteString("nameserver " + s.String() + "\n")
	}

	if builder.Len() == 0 {
		return nil
	}

	if err := os.MkdirAll("/etc", dirMode); err != nil {
		return fmt.Errorf(errFmtMkdir, "/etc", err)
	}

	if err := os.WriteFile("/etc/resolv.conf", []byte(builder.String()), fileMode); err != nil {
		return fmt.Errorf("write /etc/resolv.conf: %w", err)
	}

	return nil
}

// runBuild execs the build under bash with the env staged at /kernel/build.env,
// streaming its output to the console, and returns its exit code.
func runBuild() (int, error) {
	env, err := readEnvFile(envFile)
	if err != nil {
		return initFailedStatus, err
	}

	// Tee the build's output to a durable log on the /kernel share, on top of the live
	// console. The console (hvc0) is best-effort — its tail can be lost on poweroff — so the
	// fsync'd log is the authoritative record the host reads to debug a failed build.
	logFile, err := os.OpenFile(buildLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return initFailedStatus, fmt.Errorf(errFmtOpen, buildLog, err)
	}

	defer func() {
		_ = logFile.Sync() // flush through virtio-fs to the host before we power off
		_ = logFile.Close()
	}()

	// One writer value for both streams: os/exec guarantees at most one Write at a time when
	// Stdout == Stderr, which avoids interleaved concurrent writes to the shared log file.
	// Both guest fds already share the console, so routing stderr via os.Stdout loses nothing.
	out := io.MultiWriter(os.Stdout, logFile)

	// noctx: PID 1 has no cancellation story — the build runs to completion and the HOST
	// cancels by stopping the VM. A context.Background() here would be pure ceremony.
	//nolint:noctx
	cmd := exec.Command("/bin/bash", buildScript)
	cmd.Env = env
	cmd.Dir = kernelShare
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil // build ran and failed; surface its code
		}

		return initFailedStatus, fmt.Errorf("exec %s: %w", buildScript, err)
	}

	return 0, nil
}

func readEnvFile(path string) ([]string, error) {
	// Always start from a minimal PATH so bash and the toolchain resolve; build.sh prepends
	// /opt/llvm/bin itself. Seeded unconditionally: the smoke test stages no build.env, and
	// PID 1's own environment carries no PATH, so the file-missing path needs this too.
	env := []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}

	file, err := os.Open(path) //nolint:gosec // G304: path is the envFile constant, host-staged.
	if err != nil {
		if os.IsNotExist(err) {
			return env, nil
		}

		return nil, fmt.Errorf(errFmtOpen, path, err)
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		env = append(env, line)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return env, nil
}

// writeStatus records the exit code where the host reads it after poweroff, and makes the
// write durable on the host BEFORE returning: fsync on a virtio-fs file blocks until
// virtiofsd has flushed the backing host file, and fsync on the share dir does the same
// for the new directory entry. That replaces the old unix.Sync()+sleep with a hard
// guarantee — when this returns, the host is certain to see the status.
func writeStatus(code int) error {
	file, err := os.OpenFile(statusPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return fmt.Errorf(errFmtOpen, statusPath, err)
	}

	if _, err := file.WriteString(strconv.Itoa(code) + "\n"); err != nil {
		_ = file.Close()

		return fmt.Errorf("write %s: %w", statusPath, err)
	}

	if err := file.Sync(); err != nil {
		_ = file.Close()

		return fmt.Errorf("fsync %s: %w", statusPath, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", statusPath, err)
	}

	// Best-effort dir fsync for the new dirent's durability. The file's own fsync above is
	// the guarantee the host relies on (virtio-fs already created the host dirent on open),
	// so a virtio-fs that rejects directory fsync must not fail the status write.
	if dir, err := os.Open(kernelShare); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}

	return nil
}
