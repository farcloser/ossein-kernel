//go:build darwin && arm64

// Command ossein-kernel builds the guest Linux kernel for ossein, WITHOUT depending
// on ossein itself: it boots a microVM directly on Virtualization.framework
// (internal/vm), roots it on a Debian ext4 built host-side from the pinned OCI image
// (internal/rootfs), runs the kernel's make via our own PID-1 init
// (cmd/ossein-kernel/init), and collects kernel-arm64.
package main

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alecthomas/kong"

	"github.com/mycophonic/primordium/app/logger"
	"github.com/mycophonic/primordium/compress"
	"github.com/mycophonic/primordium/filesystem"
	"github.com/mycophonic/primordium/filesystem/dirs"
	"github.com/mycophonic/primordium/network/transporter"
	uxz "github.com/mycophonic/xz"

	"github.com/farcloser/ossein-kernel/internal/rootfs"
	"github.com/farcloser/ossein-kernel/internal/vm"
)

//go:embed build.sh
var buildScript []byte

const scratchDir = "build/kernel-scratch"

// initGuestPath is where internal/rootfs embeds the init and what the kernel init=
// arg points at (kept in sync with internal/vm's cfg.Init).
const initGuestPath = "/ossein-init"

// statusFile is the exit-code file the guest init writes into the /kernel share; the
// host reads it after the VM powers off (the replacement for vsock's Wait() code).
const statusFile = "status"

// buildLogFile is the durable, fsync'd copy of the guest build output the init tees into
// the /kernel share — the authoritative record when the live console loses its tail.
const buildLogFile = "build.log"

// rootfsSizeBytes is the nominal (sparse) size of the Debian root disk: the base is
// ~150 MB and apt-installed build deps add a few hundred more; 2 GiB is ample.
const rootfsSizeBytes = 2 << 30

// smokeTestMemoryMiB sizes the throwaway VM that boot-tests a freshly built kernel;
// 1 GiB is ample to boot the kernel and run a trivial userland command.
const smokeTestMemoryMiB = 1024

// minKernelBytes is the floor below which an existing --out is treated as truncated
// garbage rather than a real kernel (a real vmlinux is ~10 MB) and ignored for self-hosting.
const minKernelBytes = 1 << 20

// partialSuffix marks an in-progress download/build artifact; every cacheable artifact
// (downloads, the seed kernel, the root disk) is written under this suffix and renamed
// into place only on success, so its cache check can never accept a truncated file.
const partialSuffix = ".part"

// Static build errors — the conditions the build fails on, wrapped with the specifics
// at each return so the message keeps its context without a fresh dynamic error type.
var (
	errNoVmlinux     = errors.New("build produced no kernel-arm64")
	errNoPerf        = errors.New("build.sh succeeded but produced no perf-arm64")
	errSmokeExit     = errors.New("boot smoke-test exited non-zero")
	errBuildExit     = errors.New("kernel build.sh exited non-zero")
	errNoStatus      = errors.New("guest wrote no status file")
	errDownloadHTTP  = errors.New("download failed")
	errDownloadStall = errors.New("download stalled (no data within idle window)")
	errTarEscape     = errors.New("llvm tar entry escapes destination")
	errNoSeedVmlinux = errors.New("no vmlinux found in seed tarball")
	errSHAMismatch   = errors.New("sha256 mismatch")
)

// Download resilience is LAYERED, because no single mechanism covers a multi-GB fetch:
//   - transporter (newDownloadClient) gives request-level retry — backoff + jitter + Retry-After
//     — for connection/header failures;
//   - the file-level loop in fetchFile re-GETs on a failure DURING the body copy, which the
//     client's request-scoped retry does not cover;
//   - the stallReader idle-watchdog aborts a body that goes quiet, which neither the client nor
//     its base transport detects (transporter's progressBody only logs; DefaultTransport has no
//     body timeout).
const (
	downloadAttempts    = 3                // file-level re-GET attempts on a body-copy failure
	downloadRetryWait   = 3 * time.Second  // fixed backoff between those attempts
	downloadIdleTimeout = 60 * time.Second // abort a body that receives no bytes for this long
)

// buildVMMemoryMiB sizes the build VM. Everything writable lives in RAM: the init pivots the
// read-only ext4 root into a tmpfs overlay (apt installs land there, ~300 MB), and /kbuild is
// a tmpfs holding the extracted source (~1.5 GB) + objects (~2 GB for the lean allowlist
// kernel). So memory must fit both. The toolchain is a read-only /opt/llvm virtio-fs mount,
// not tmpfs. 8 GiB is comfortable and leaves headroom on a 16 GB host; bump if it ever ENOSPCs.
const buildVMMemoryMiB = 8192

type config struct {
	LogLevel string `default:"info" enum:"debug,info,warn,error" help:"log verbosity" name:"log-level"`

	KernelConfig  string `help:"Kconfig allowlist fragment merged onto tinyconfig (the 'which kernel' input)"     name:"kernel-config"                                                            required:"true"`
	KernelPatches string `help:"directory of *.patch files applied (lexical order) to the pristine kernel source" name:"kernel-patches"                                                           required:"true"`
	Out           string `default:"build/kernel-arm64"                                                            help:"output kernel path (only promoted here if it passes the boot smoke-test)" name:"out"`
	LocalVer      string `help:"kernel version tag"                                                               name:"localversion"                                                             required:"true"`

	Init        string `help:"path to the cross-compiled guest init binary (linux/arm64)"                                   name:"init"          required:"true"`
	KataURL     string `help:"URL of the Kata bootstrap-kernel tarball (.tar.zst), for cold-start seed"                     name:"kata-url"      required:"true"`
	KataSHA     string `help:"sha256 of the Kata bootstrap-kernel tarball"                                                  name:"kata-sha256"   required:"true"`
	Image       string `help:"debian build container image"                                                                 name:"image"         required:"true"`
	DebianSuite string `help:"apt suite/codename"                                                                           name:"debian-suite"  required:"true"`
	AptSnapshot string `help:"snapshot.debian.org archive timestamp all apt packages are pinned to (e.g. 20260701T025158Z)" name:"apt-snapshot"  required:"true"`
	SourceURL   string `help:"URL of the kernel source tarball"                                                             name:"source-url"    required:"true"`
	SourceSHA   string `help:"sha256 of the kernel source tarball"                                                          name:"source-sha256" required:"true"`
	LLVMURL     string `help:"URL of the official LLVM release tarball"                                                     name:"llvm-url"      required:"true"`
	LLVMSHA     string `help:"sha256 of the LLVM tarball"                                                                   name:"llvm-sha256"   required:"true"`
}

func main() {
	// Name our user-dirs namespace (~/Library/Caches/ossein-kernel/…) for any
	// primordium helper that resolves a cache/temp dir.
	dirs.SetAppName("ossein-kernel")

	var cli config

	kong.Parse(&cli,
		kong.Name("ossein-kernel"),
		kong.Description("Build the ossein guest kernel (standalone; no ossein runtime)."),
		kong.UsageOnError(),
	)

	logger.SetDefaultsForLogger(context.Background(), parseLevel(cli.LogLevel))

	if err := run(cli); err != nil {
		slog.Error("build failed", "err", err)
		os.Exit(1)
	}
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func run(cfg config) error {
	// One signal context for the whole run: Ctrl-C/SIGTERM cancels in-flight downloads (ctx-aware
	// http, below) AND, once a VM is up, drives its graceful teardown (osseinBuild/smokeTest defer
	// inst.Close). One image cache, shared by both VM phases (they run sequentially).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One HTTP client (primordium transporter) for every host-side fetch; closed at run() exit
	// to stop its background workers.
	client := newDownloadClient()
	defer client.CloseIdleConnections()

	// Validate cheap local inputs BEFORE resolveBootstrap, which may download a
	// several-hundred-MB seed on a cold start — no point paying that to then fail on a typo'd
	// --init/--kernel-config path.
	for _, input := range []struct{ what, path string }{
		{"guest init binary", cfg.Init},
		{"kernel config", cfg.KernelConfig},
		{"kernel patches dir", cfg.KernelPatches},
	} {
		if _, err := os.Stat(input.path); err != nil {
			return fmt.Errorf("%s not found at %q: %w", input.what, input.path, err)
		}
	}

	bootstrap, err := resolveBootstrap(ctx, client, cfg)
	if err != nil {
		return err
	}

	// The Debian root disk (host-side OCI unpack → ext4, with our init embedded),
	// cached by image digest + init hash. Per-boot writable clones are made from it.
	rootImage, err := prepareRootfs(ctx, cfg)
	if err != nil {
		return err
	}

	// The workdir becomes /kernel in the guest (virtio-fs): source + config + build.sh +
	// build.env go in, kernel-arm64 + perf-arm64 + status come out. The build SCRATCH
	// (/kbuild) is NOT virtio-fs: the guest init mounts it as tmpfs (a real fs the guest
	// kernel owns), so size the VM's memory to fit the build.
	workdir := filepath.Join(scratchDir, "kernel")
	if err := stageWorkdir(cfg, workdir); err != nil {
		return err
	}

	if err := fetchFile(
		ctx,
		client,
		cfg.SourceURL,
		filepath.Join(workdir, "source.tar.xz"),
		cfg.SourceSHA,
		"kernel source",
	); err != nil {
		return err
	}
	// Toolchain: downloaded, extracted, and trimmed ON THE HOST, then mounted read-only
	// into the build VM (the guest never touches the 1.8 GB tarball). sha256 is REQUIRED.
	llvmDir, err := prepareLLVM(ctx, client, cfg)
	if err != nil {
		return err
	}

	slog.Info("clean build (allowlist)", "config", cfg.KernelConfig)

	if err := buildKernel(ctx, bootstrap, rootImage, workdir, llvmDir); err != nil {
		return fmt.Errorf("kernel build: %w", err)
	}

	built := filepath.Join(workdir, "kernel-arm64")
	if _, err := os.Stat(built); err != nil {
		return fmt.Errorf("%w (looked in %s)", errNoVmlinux, workdir)
	}

	// Boot-test the fresh kernel BEFORE promoting it to --out: a kernel that can't boot +
	// run userland never becomes the self-host default, so a bad build can't wedge the
	// factory — --out keeps the last kernel that worked. (Container-contract validation now
	// lives in ossein's own test suite.)
	if err := smokeTest(ctx, built, rootImage); err != nil {
		return fmt.Errorf("built kernel failed its boot smoke-test — NOT promoted to %s: %w", cfg.Out, err)
	}

	if err := copyFile(built, cfg.Out); err != nil {
		return err
	}

	slog.Info("kernel built + boot-verified", "path", cfg.Out)

	// perf rides out next to the kernel for the cross-runtime `perf bench` harness. build.sh
	// builds it as a hard step, so a successful build always leaves it here — its absence is a bug.
	builtPerf := filepath.Join(workdir, "perf-arm64")
	if _, err := os.Stat(builtPerf); err != nil {
		return fmt.Errorf("%w (looked in %s)", errNoPerf, workdir)
	}

	perfOut := filepath.Join(filepath.Dir(cfg.Out), "perf-arm64")
	if err := copyExecutable(builtPerf, perfOut); err != nil {
		return err
	}

	slog.Info("perf built + promoted", "path", perfOut)

	return nil
}

// stageWorkdir populates the /kernel virtio-fs share: build.sh, the config, the patch
// set, and the env file the init hands to build.sh. Source + LLVM are staged by the
// callers after this (they download).
func stageWorkdir(cfg config, workdir string) error {
	if err := os.MkdirAll(workdir, filesystem.DirPermissionsPrivate); err != nil {
		return fmt.Errorf("create workdir %s: %w", workdir, err)
	}

	if err := filesystem.WriteFile(
		filepath.Join(workdir, "build.sh"),
		buildScript,
		filesystem.FilePermissionsPrivate,
	); err != nil {
		return err
	}
	// allowlist model — tinyconfig + our single fragment (build.sh merges it).
	if err := copyFile(cfg.KernelConfig, filepath.Join(workdir, "kernel-fragment")); err != nil {
		return err
	}
	// The source tarball stays pristine (pinned url + sha256); our deltas are explicit patch
	// files that build.sh applies in lexical order, failing the build if any stops applying.
	if err := stagePatches(cfg.KernelPatches, filepath.Join(workdir, "patches")); err != nil {
		return err
	}

	// The build env used to ride in via the container RunSpec; now the init reads it from
	// this file and hands it to build.sh. LocalVer is the bare tag (kong can't take a
	// leading-dash value); prepend exactly one dash (TrimLeft tolerates a stray "-").
	env := strings.Join([]string{
		"LOCALVERSION=-" + strings.TrimLeft(cfg.LocalVer, "-"),
		"OSSEIN_DEBIAN_SUITE=" + cfg.DebianSuite,
		"OSSEIN_APT_SNAPSHOT=" + cfg.AptSnapshot,
	}, "\n") + "\n"

	if err := filesystem.WriteFile(
		filepath.Join(workdir, "build.env"),
		[]byte(env),
		filesystem.FilePermissionsPrivate,
	); err != nil {
		return err
	}

	// Clear status + log from a prior run so a failed boot can't be read as stale success.
	_ = os.Remove(filepath.Join(workdir, statusFile))
	_ = os.Remove(filepath.Join(workdir, buildLogFile))

	return nil
}

func buildKernel(ctx context.Context, bootstrap, rootImage, workdir, llvmDir string) error {
	// Give the build VM every core but one (leave the host responsive); build.sh
	// derives make -j from the VM's vCPU count.
	cpus := max(runtime.NumCPU()-1, 1)

	machine, err := vm.New(vm.Config{
		Kernel: bootstrap,
		// Pristine ext4 attached read-only; the init overlays a tmpfs so writes go to RAM.
		RootDisk:  rootImage,
		Init:      initGuestPath,
		CPUs:      uint(cpus),
		MemoryMiB: buildVMMemoryMiB,
		// ossein.net=1: build.sh apt-gets from snapshot.debian.org. ossein.llvm=1: declares
		// the /opt/llvm share so the init hard-requires its mount instead of tolerating a
		// failure that would resurface minutes later as "clang: not found" mid-build (the
		// smoke-test VM declares neither).
		Cmdline: []string{"ossein.net=1", "ossein.llvm=1"},
		Network: true,
		Console: os.Stdout,
		Shares: []vm.Share{
			{Tag: "kernel", Dir: workdir},
			{Tag: "llvm", Dir: llvmDir, ReadOnly: true},
		},
	})
	if err != nil {
		return err
	}

	slog.Info("booting build VM", "cpus", cpus, "memMiB", buildVMMemoryMiB)

	code, err := boot(ctx, machine, workdir)
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("%w: status %d (full guest output: %s)",
			errBuildExit, code, filepath.Join(workdir, buildLogFile))
	}

	return nil
}

func smokeTest(ctx context.Context, kernelPath, rootImage string) error {
	slog.Info("boot smoke-test: booting the freshly-built kernel", "kernel", kernelPath)

	// A throwaway /kernel share whose build.sh just proves userland runs. No network.
	smokeDir := filepath.Join(scratchDir, "smoke")
	if err := os.MkdirAll(smokeDir, filesystem.DirPermissionsPrivate); err != nil {
		return fmt.Errorf("create smoke dir: %w", err)
	}

	_ = os.Remove(filepath.Join(smokeDir, statusFile))

	smoke := "#!/bin/bash\nset -e\necho \"smoke: kernel $(uname -r) booted; userland OK\"\nexit 0\n"
	if err := filesystem.WriteFile(
		filepath.Join(smokeDir, "build.sh"),
		[]byte(smoke),
		filesystem.FilePermissionsPrivate,
	); err != nil {
		return err
	}

	machine, err := vm.New(vm.Config{
		Kernel:    kernelPath,
		RootDisk:  rootImage, // pristine, read-only; init overlays a tmpfs
		Init:      initGuestPath,
		CPUs:      1,
		MemoryMiB: smokeTestMemoryMiB,
		Console:   os.Stdout,
		Shares:    []vm.Share{{Tag: "kernel", Dir: smokeDir}},
	})
	if err != nil {
		return err
	}

	code, err := boot(ctx, machine, smokeDir)
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("%w: exit code %d", errSmokeExit, code)
	}

	slog.Info("boot smoke-test: passed (kernel boots + userland runs)")

	return nil
}

// boot starts the VM, waits for the guest to power itself off, and reads the exit code
// the init wrote to <share>/status. ctx cancellation hard-stops the VM (see vm.Wait).
func boot(ctx context.Context, machine *vm.VM, sharedDir string) (int, error) {
	if err := machine.Start(); err != nil {
		return 0, err
	}

	defer func() { _ = machine.Stop() }()

	if err := machine.Wait(ctx); err != nil {
		return 0, fmt.Errorf("vm wait: %w", err)
	}

	return readStatus(filepath.Join(sharedDir, statusFile))
}

func readStatus(statusPath string) (int, error) {
	data, err := os.ReadFile(statusPath) //nolint:gosec // G304: statusPath is a fixed scratchDir path
	if err != nil {
		return 0, fmt.Errorf("%w at %s: %w", errNoStatus, statusPath, err)
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0, fmt.Errorf("%w at %s: empty", errNoStatus, statusPath)
	}

	code, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parse guest status %q: %w", trimmed, err)
	}

	return code, nil
}

// prepareRootfs builds (or reuses) the pristine Debian root disk, keyed on BOTH the
// image ref and the embedded init binary (so rebuilding the init invalidates the
// cache). Per-boot clones are made from it so the pristine copy is never mutated.
func prepareRootfs(ctx context.Context, cfg config) (string, error) {
	rootImage := filepath.Join(scratchDir, "debian-rootfs.ext4")
	stamp := filepath.Join(scratchDir, ".debian-rootfs-ref")

	key, err := rootfsCacheKey(cfg)
	if err != nil {
		return "", err
	}

	//nolint:gosec // G304: stamp is a fixed scratchDir path
	if prev, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(prev)) == key {
		if fi, err := os.Stat(rootImage); err == nil && fi.Size() > 0 {
			slog.Info("reusing Debian root disk", "path", rootImage, "image", cfg.Image)

			return rootImage, nil
		}
	}

	if err := os.MkdirAll(scratchDir, filesystem.DirPermissionsPrivate); err != nil {
		return "", fmt.Errorf("create scratch dir: %w", err)
	}

	slog.Info("building Debian root disk from image", "image", cfg.Image)

	// Build to a temp path then rename — the same atomicity contract as fetchFile and
	// fetchSeedKernel: an uncleanly-killed build (SIGKILL, power loss) must not leave a
	// partial ext4 at the final path, where the stamp-match + Size()>0 reuse check above
	// would accept it forever, wedging every future boot on a corrupt disk.
	partial := rootImage + partialSuffix
	if err := rootfs.Build(ctx, cfg.Image, cfg.Init, partial, rootfsSizeBytes); err != nil {
		return "", fmt.Errorf("build rootfs: %w", err)
	}

	if err := os.Rename(partial, rootImage); err != nil {
		return "", fmt.Errorf("rename root disk into place: %w", err)
	}

	if err := filesystem.WriteFile(stamp, []byte(key), filesystem.FilePermissionsPrivate); err != nil {
		return "", err
	}

	slog.Info("Debian root disk ready", "path", rootImage)

	return rootImage, nil
}

// rootfsCacheKey pins the cached root disk to the image ref AND the init binary's
// content — the init is embedded in the disk, so a rebuilt init must invalidate it.
func rootfsCacheKey(cfg config) (string, error) {
	handle, err := os.Open(cfg.Init)
	if err != nil {
		return "", fmt.Errorf("open init %s: %w", cfg.Init, err)
	}
	defer func() { _ = handle.Close() }()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, handle); err != nil {
		return "", fmt.Errorf("hash init %s: %w", cfg.Init, err)
	}

	return cfg.Image + "@init:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// newDownloadClient builds the shared client for host-side fetches: primordium's transporter,
// which layers retry/backoff/jitter/Retry-After/User-Agent over the standard transport. The
// caller closes it (CloseIdleConnections) to stop the client's background workers.
func newDownloadClient() *http.Client {
	return transporter.NewClient(transporter.Options{
		MaxRetries:     3,
		InitialBackoff: time.Second,
		MaxBackoff:     30 * time.Second,
		UserAgent:      "ossein-kernel",
	})
}

func fetchFile(ctx context.Context, client *http.Client, url, dest, wantSHA, label string) error {
	if fi, err := os.Stat(dest); err == nil && fi.Size() > 0 {
		if wantSHA == "" {
			slog.Info("reusing cached artifact", "artifact", label, "path", dest)

			return nil
		}

		slog.Info("verifying cached checksum", "artifact", label)

		if err := verifySHA(dest, wantSHA); err == nil {
			slog.Info("reusing verified artifact", "artifact", label, "path", dest)

			return nil
		}

		slog.Warn("cached artifact failed checksum; re-downloading", "artifact", label)
	}

	// Ensure the destination dir exists — fetchFile is order-independent (the seed download
	// runs before run()'s workdir MkdirAll, and build/ may have just been removed).
	if err := os.MkdirAll(filepath.Dir(dest), filesystem.DirPermissionsPrivate); err != nil {
		return fmt.Errorf("create dest dir for %s: %w", label, err)
	}

	tmp := dest + partialSuffix

	var (
		written int64
		err     error
	)

	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if attempt > 1 {
			slog.Warn("retrying download", "artifact", label,
				"attempt", attempt, "of", downloadAttempts, "err", err)

			select {
			case <-ctx.Done():
				return fmt.Errorf("download %s interrupted: %w", label, ctx.Err())
			case <-time.After(downloadRetryWait):
			}
		}

		slog.Info("downloading", "artifact", label, "url", url)

		if written, err = downloadAttempt(ctx, client, url, tmp, label); err == nil {
			break
		}
		// A user Ctrl-C (parent ctx cancelled) is terminal — don't burn retries on it.
		if ctx.Err() != nil {
			return fmt.Errorf("download %s interrupted: %w", label, ctx.Err())
		}
	}

	if err != nil {
		return err
	}

	if wantSHA != "" {
		// A full re-read of the file to hash it — for the ~1.8 GB tarball this is a
		// multi-second pass with no network, so label it or it reads as a post-100% hang.
		slog.Info("verifying checksum", "artifact", label, "mb", written>>20)

		if err := verifySHA(tmp, wantSHA); err != nil {
			return err
		}
	} else {
		slog.Warn("no checksum pin; artifact unverified", "artifact", label, "path", dest)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp, dest, err)
	}

	return nil
}

// downloadAttempt performs one GET into tmp (truncating it), bounded by a mid-body idle
// watchdog: if no bytes arrive within downloadIdleTimeout it cancels the request so the
// otherwise-blocked Read returns and fetchFile can retry. Returns bytes written on success.
func downloadAttempt(ctx context.Context, client *http.Client, url, tmp, label string) (int64, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request for %s: %w", label, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", label, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: %s HTTP %s", errDownloadHTTP, label, resp.Status)
	}

	outFile, err := os.Create(tmp) //nolint:gosec // G304: tmp is a program-built path
	if err != nil {
		return 0, fmt.Errorf("create temp file %s: %w", tmp, err)
	}

	body := newStallReader(resp.Body, downloadIdleTimeout, cancel)
	defer body.stop()

	written, err := copyWithProgress(outFile, body, resp.ContentLength, label)
	if closeErr := outFile.Close(); closeErr != nil && err == nil {
		err = fmt.Errorf("close temp file %s: %w", tmp, closeErr)
	}

	if err != nil {
		// The watchdog cancels with errDownloadStall; surface that rather than the generic
		// "context canceled" the interrupted Read reports.
		if cause := context.Cause(ctx); errors.Is(cause, errDownloadStall) {
			return written, fmt.Errorf("download %s: %w", label, cause)
		}

		return written, err
	}

	return written, nil
}

// stallReader wraps a download body and fires cancel(errDownloadStall) if no Read makes
// progress within idle. The timer is reset on every non-empty Read; stop() disarms it. Uses an
// AfterFunc timer (no channel), so Reset from within Read is race-free for our single reader.
type stallReader struct {
	r      io.Reader
	idle   time.Duration
	cancel context.CancelCauseFunc
	timer  *time.Timer
}

func newStallReader(r io.Reader, idle time.Duration, cancel context.CancelCauseFunc) *stallReader {
	sr := &stallReader{r: r, idle: idle, cancel: cancel}
	sr.timer = time.AfterFunc(idle, func() { cancel(errDownloadStall) })

	return sr
}

func (sr *stallReader) Read(p []byte) (int, error) {
	nread, err := sr.r.Read(p)
	if nread > 0 {
		sr.timer.Reset(sr.idle)
	}

	//nolint:wrapcheck // io.Reader passthrough — must forward err verbatim (io.EOF sentinel)
	return nread, err
}

func (sr *stallReader) stop() { sr.timer.Stop() }

func prepareLLVM(ctx context.Context, client *http.Client, cfg config) (string, error) {
	// Check the extraction stamp FIRST: if the toolchain is already extracted for this exact
	// sha, we don't need the tarball at all — skip the multi-second re-hash fetchFile does.
	llvmDir := filepath.Join(scratchDir, "llvm")

	stamp := filepath.Join(llvmDir, ".ossein-llvm-sha")

	stampSHA, err := os.ReadFile(stamp) //nolint:gosec // G304: stamp is a fixed scratchDir path
	if err == nil && strings.TrimSpace(string(stampSHA)) == cfg.LLVMSHA {
		slog.Info("reusing extracted LLVM toolchain", "path", llvmDir)

		return llvmDir, nil
	}

	tarball := filepath.Join(scratchDir, "llvm.tar.xz")
	if err := fetchFile(ctx, client, cfg.LLVMURL, tarball, cfg.LLVMSHA, "LLVM toolchain"); err != nil {
		return "", err
	}

	slog.Info("extracting + trimming LLVM toolchain", "dest", llvmDir)

	if err := os.RemoveAll(llvmDir); err != nil {
		return "", fmt.Errorf("clear llvm dir: %w", err)
	}

	if err := os.MkdirAll(llvmDir, filesystem.DirPermissionsPrivate); err != nil {
		return "", fmt.Errorf("create llvm dir: %w", err)
	}

	if err := extractLLVM(tarball, llvmDir); err != nil {
		return "", fmt.Errorf("extract LLVM toolchain: %w", err)
	}

	if err := filesystem.WriteFile(stamp, []byte(cfg.LLVMSHA), filesystem.FilePermissionsPrivate); err != nil {
		return "", err
	}

	return llvmDir, nil
}

func extractLLVM(tarball, destDir string) error {
	tarFile, err := os.Open(tarball) //nolint:gosec // G304: tarball is a fixed scratchDir path
	if err != nil {
		return fmt.Errorf("open llvm tarball %s: %w", tarball, err)
	}
	defer func() { _ = tarFile.Close() }()

	// One-time bypass of primordium's compress.Decompress front door — deliberate, documented.
	// The LLVM tarball is ~470 xz blocks (11.8 GB uncompressed), and mycophonic/xz can decode
	// them concurrently via NewParallelReader — but that needs random access (io.ReaderAt) + the
	// file size to read the block index, which compress.Decompress's io.Reader signature can't
	// express. A raw *os.File gives us both. Measured on this host (18 workers): 6.7s vs 76.9s
	// single-threaded through the front door (and 702s on the original unbuffered ulikunitz path)
	// — the difference between an unacceptable and a negligible step of the build. This works ONLY
	// because the input is a seekable file; a non-seekable stream (e.g. the seed decompressed
	// straight off the network) legitimately stays on the front door.
	size, err := tarFile.Stat()
	if err != nil {
		return fmt.Errorf("stat llvm tarball %s: %w", tarball, err)
	}

	decompressor, err := uxz.NewParallelReader(tarFile, size.Size())
	if err != nil {
		return fmt.Errorf("parallel xz reader: %w", err)
	}
	defer func() { _ = decompressor.Close() }()

	root := filepath.Clean(destDir) + string(os.PathSeparator)

	tarReader := tar.NewReader(decompressor)
	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("read llvm tar: %w", err)
		}

		// Strip the leading LLVM-<ver>-… path component.
		rel := strings.TrimPrefix(hdr.Name, "./")

		slash := strings.IndexByte(rel, '/')
		if slash < 0 {
			continue // the top-level directory entry itself
		}

		rel = rel[slash+1:]
		if rel == "" || pruneLLVMPath(rel) {
			continue
		}

		target := filepath.Join(destDir, rel)
		if !strings.HasPrefix(target, root) {
			return fmt.Errorf("%w: %s", errTarEscape, hdr.Name)
		}

		if err := writeLLVMEntry(root, target, hdr, tarReader); err != nil {
			return err
		}
	}

	return nil
}

// writeLLVMEntry materializes one tar entry (directory, regular file, or symlink)
// at target. Split out of extractLLVM so that loop stays within the cognitive-complexity
// budget; the caller has already validated that target is inside the destination root
// (root, trailing-separator terminated, is passed through only to validate symlink targets).
func writeLLVMEntry(root, target string, hdr *tar.Header, tarReader *tar.Reader) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := os.MkdirAll(target, filesystem.DirPermissionsPrivate); err != nil {
			return fmt.Errorf("mkdir %s: %w", target, err)
		}
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), filesystem.DirPermissionsPrivate); err != nil {
			return fmt.Errorf("mkdir parent of %s: %w", target, err)
		}
		// Preserve the archive's mode bits (.Perm()) — clang/ld.lld need their exec bit.
		mode := os.FileMode(hdr.Mode).Perm() //nolint:gosec // G115: a tar file mode always fits uint32

		out, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // G304: path validated
		if err != nil {
			return fmt.Errorf("create %s: %w", target, err)
		}

		if _, err := io.Copy(out, tarReader); err != nil {
			_ = out.Close()

			return fmt.Errorf("write %s: %w", target, err)
		}

		if err := out.Close(); err != nil {
			return fmt.Errorf("close %s: %w", target, err)
		}
	case tar.TypeSymlink:
		// Reject a link whose target escapes the destination root — an absolute target, or one
		// that climbs out via `..`. The archive is sha256-pinned so this is hardening, not a live
		// threat, but the lexical target check above is blind to symlinks: an in-root symlinked
		// dir that a later entry writes through would defeat it, and an escaping link is the way
		// to create one. Resolve the target relative to the link's own directory before checking.
		linkTarget := hdr.Linkname
		if !filepath.IsAbs(linkTarget) {
			//nolint:gosec // G305: joined only to validate the target; escapes are rejected below
			linkTarget = filepath.Join(filepath.Dir(target), linkTarget)
		}

		if !strings.HasPrefix(filepath.Clean(linkTarget)+string(os.PathSeparator), root) {
			return fmt.Errorf("%w: symlink %s -> %s", errTarEscape, hdr.Name, hdr.Linkname)
		}

		if err := os.MkdirAll(filepath.Dir(target), filesystem.DirPermissionsPrivate); err != nil {
			return fmt.Errorf("mkdir parent of %s: %w", target, err)
		}

		_ = os.Remove(target) // idempotent re-extract
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("symlink %s: %w", target, err)
		}
	default:
		// hardlinks/devices/etc — none expected in the LLVM bundle. Log rather than drop
		// silently, so a future bundle that ships one surfaces here instead of as a baffling
		// "clang: not found" later in the build.
		slog.Warn("skipping unsupported llvm tar entry", "name", hdr.Name, "type", hdr.Typeflag)
	}

	return nil
}

func pruneLLVMPath(rel string) bool {
	switch {
	case rel == "include" || strings.HasPrefix(rel, "include/"):
		return true
	case rel == "share" || strings.HasPrefix(rel, "share/"):
		return true
	case rel == "lib/cmake" || strings.HasPrefix(rel, "lib/cmake/"):
		return true
	case strings.HasPrefix(rel, "bin/lldb"), strings.HasPrefix(rel, "lib/liblldb"):
		return true
	// Only depth-1 lib/*.a static archives; keep lib/clang/**'s compiler-rt libs.
	case strings.HasPrefix(rel, "lib/") && strings.HasSuffix(rel, ".a") && strings.Count(rel, "/") == 1:
		return true
	}

	return false
}

func resolveBootstrap(ctx context.Context, client *http.Client, cfg config) (string, error) {
	// Self-host on the previous output — but guard against a truncated/garbage --out (a real
	// kernel is ~10 MB) wedging every build at VM boot; fall back to the seed instead.
	if info, err := os.Stat(cfg.Out); err == nil {
		if info.Size() >= minKernelBytes {
			slog.Info("bootstrap: self-hosting", "path", cfg.Out)

			return cfg.Out, nil
		}

		slog.Warn(
			"existing --out is implausibly small; ignoring it and using the seed",
			"path",
			cfg.Out,
			"bytes",
			info.Size(),
		)
	}

	return fetchSeedKernel(ctx, client, cfg.KataURL, cfg.KataSHA)
}

func fetchSeedKernel(ctx context.Context, client *http.Client, url, wantSHA string) (string, error) {
	cache := filepath.Join(scratchDir, "bootstrap-kernel-arm64")
	if fi, err := os.Stat(cache); err == nil && fi.Size() > 0 {
		slog.Info("bootstrap: cached kernel", "path", cache)

		return cache, nil
	}

	tarball := filepath.Join(scratchDir, "seed-kernel.tar.zst")
	if err := fetchFile(ctx, client, url, tarball, wantSHA, "Seed kernel"); err != nil {
		return "", err
	}

	tarFile, err := os.Open(tarball) //nolint:gosec // G304: tarball is a fixed scratchDir path
	if err != nil {
		return "", fmt.Errorf("open seed tarball %s: %w", tarball, err)
	}
	defer func() { _ = tarFile.Close() }()

	// Pure-Go decompression via primordium (magic-byte detection → zstd here).
	decompressor, err := compress.Decompress(tarFile)
	if err != nil {
		return "", err
	}
	defer func() { _ = decompressor.Close() }()

	tarReader := tar.NewReader(decompressor)
	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return "", fmt.Errorf("read seed tar: %w", err)
		}
		// The uncompressed vmlinux — a real file (TypeReg excludes the vmlinux.container
		// symlink; "vmlinuz" can't match the "vmlinux" prefix, so no explicit exclusion).
		base := path.Base(hdr.Name)
		if hdr.Typeflag == tar.TypeReg && strings.HasPrefix(base, "vmlinux") {
			if err := os.MkdirAll(filepath.Dir(cache), filesystem.DirPermissionsPrivate); err != nil {
				return "", fmt.Errorf("create seed cache dir: %w", err)
			}
			// Extract to a temp path then rename — an interrupted cold-start extract must
			// not leave a truncated kernel that the Size()>0 reuse check (above) accepts
			// forever, wedging every future cold start at VM boot.
			partial := cache + partialSuffix

			out, err := os.Create(partial) //nolint:gosec // G304: partial is a fixed scratchDir path
			if err != nil {
				return "", fmt.Errorf("create seed file %s: %w", partial, err)
			}

			if _, err := io.Copy(out, tarReader); err != nil { //nolint:gosec // sha-verified pinned release
				_ = out.Close()

				return "", fmt.Errorf("write seed kernel: %w", err)
			}

			if err := out.Close(); err != nil {
				return "", fmt.Errorf("close seed file %s: %w", partial, err)
			}

			if err := os.Rename(partial, cache); err != nil {
				return "", fmt.Errorf("rename seed kernel into place: %w", err)
			}

			slog.Info("bootstrap: extracted seed kernel", "path", cache)

			return cache, nil
		}
	}

	return "", fmt.Errorf("%w: %s", errNoSeedVmlinux, url)
}

func copyWithProgress(dst io.Writer, src io.Reader, total int64, label string) (int64, error) {
	const tick = 2 * time.Second

	buf := make([]byte, 1<<20)

	var written int64

	start := time.Now()
	lastLog, lastBytes := start, int64(0)

	logLine := func(deltaBytes int64, since time.Duration) {
		mbps := 0.0
		if s := since.Seconds(); s > 0 {
			mbps = float64(deltaBytes) / s / (1 << 20)
		}

		switch {
		case total > 0 && mbps > 0:
			etaSecs := float64(total-written) / (mbps * (1 << 20))
			eta := time.Duration(etaSecs * float64(time.Second)).Round(time.Second)
			slog.Info("downloading", "artifact", label, "mb", written>>20, "of", total>>20,
				"pct", written*100/total, "rate", fmt.Sprintf("%.1f MB/s", mbps), "eta", eta)
		case total > 0:
			slog.Info("downloading", "artifact", label, "mb", written>>20, "of", total>>20,
				"pct", written*100/total)
		default:
			slog.Info("downloading", "artifact", label, "mb", written>>20,
				"rate", fmt.Sprintf("%.1f MB/s", mbps))
		}
	}

	for {
		nread, rerr := src.Read(buf)
		if nread > 0 {
			if _, werr := dst.Write(buf[:nread]); werr != nil {
				return written, fmt.Errorf("write download: %w", werr)
			}

			written += int64(nread)
			if now := time.Now(); now.Sub(lastLog) >= tick {
				logLine(written-lastBytes, now.Sub(lastLog))
				lastLog, lastBytes = now, written
			}
		}

		if rerr == io.EOF {
			logLine(written, time.Since(start)) // final line: average over the whole transfer

			return written, nil
		}

		if rerr != nil {
			return written, fmt.Errorf("read download: %w", rerr)
		}
	}
}

func verifySHA(file, want string) error {
	handle, err := os.Open(file) //nolint:gosec // G304: file is a program-built artifact path
	if err != nil {
		return fmt.Errorf("open %s: %w", file, err)
	}
	defer func() { _ = handle.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, handle); err != nil {
		return fmt.Errorf("hash %s: %w", file, err)
	}

	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%w: got %s want %s", errSHAMismatch, got, want)
	}

	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: src is a program-built path (config/patch/artifact)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}

	if err := os.MkdirAll(filepath.Dir(dst), filesystem.DirPermissionsPrivate); err != nil {
		return fmt.Errorf("create dir for %s: %w", dst, err)
	}

	return filesystem.WriteFile(dst, data, filesystem.FilePermissionsPrivate)
}

// stagePatches copies srcDir/*.patch into destDir for build.sh to apply. destDir is
// recreated each run so a patch deleted from the repo can't linger in the scratch dir and
// keep getting applied. Ordering is lexical (0001-, 0002-, …) — the same order build.sh's
// glob yields — so the sequence is the file names, not directory iteration order.
func stagePatches(srcDir, destDir string) error {
	patches, err := filepath.Glob(filepath.Join(srcDir, "*.patch"))
	if err != nil {
		return fmt.Errorf("glob patches in %s: %w", srcDir, err)
	}

	slices.Sort(patches)

	if err := os.RemoveAll(destDir); err != nil {
		return fmt.Errorf("clear patch dir: %w", err)
	}

	if err := os.MkdirAll(destDir, filesystem.DirPermissionsPrivate); err != nil {
		return fmt.Errorf("create patch dir: %w", err)
	}

	for _, p := range patches {
		if err := copyFile(p, filepath.Join(destDir, filepath.Base(p))); err != nil {
			return err
		}
	}

	if len(patches) == 0 {
		slog.Warn("no patches found; building pristine upstream", "dir", srcDir)
	} else {
		names := make([]string, 0, len(patches))
		for _, p := range patches {
			names = append(names, filepath.Base(p))
		}

		slog.Info("staged kernel patches", "count", len(patches), "patches", strings.Join(names, ", "))
	}

	return nil
}

// copyExecutable copies src to dst, preserving src's mode bits. copyFile writes
// FilePermissionsPrivate (0o600), which would leave a promoted perf non-executable;
// preserving the source mode (perf ships +x) keeps it runnable without a hardcoded 0o755.
func copyExecutable(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}

	if err := copyFile(src, dst); err != nil {
		return err
	}

	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}

	return nil
}
