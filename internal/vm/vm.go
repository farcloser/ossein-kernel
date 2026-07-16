//go:build darwin && arm64

// Package vm boots the kernel-build microVM directly on Virtualization.framework
// (Code-Hex/vz), with NO dependency on ossein's container runtime. The guest is
// kernel + a Debian rootfs disk (/dev/vda, read-only — the init pivots into a
// tmpfs overlay) whose init is our own PID 1 (cmd/ossein-kernel/init) — it
// mounts the shares, runs build.sh, and powers off.
//
// Boot contract:
//
//	console=hvc0 panic=-1 mitigations=off root=/dev/vda ro rootfstype=ext4 init=/ossein-init
//
// This is deliberately the thinnest possible VZ config: block root, virtio-fs
// shares, an optional NAT NIC for apt, a console, and entropy. No vsock, no
// balloon, no Rosetta — the build never needs them.
package vm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Code-Hex/vz/v3"
)

var (
	errStartFailed = errors.New("vm failed to reach running state")
	errRunError    = errors.New("vm entered error state")
)

// Share is a virtio-fs directory share, mounted in-guest by tag.
type Share struct {
	Tag      string
	Dir      string
	ReadOnly bool
}

// Config describes one build microVM.
type Config struct {
	Kernel    string // uncompressed arm64 Image (kernel-arm64)
	RootDisk  string // Debian rootfs ext4 → /dev/vda (attached read-only; the init overlays a tmpfs)
	Init      string // guest init path for the kernel init= arg (e.g. /ossein-init)
	CPUs      uint
	MemoryMiB uint64
	Cmdline   []string // extra kernel args appended to the boot contract
	Shares    []Share
	Network   bool     // attach a VZ NAT NIC (build needs it for apt; smoke test does not)
	Console   *os.File // guest console (hvc0) sink; nil = discard
}

// VM wraps a running/startable vz VirtualMachine.
type VM struct {
	vm *vz.VirtualMachine
}

func cmdline(cfg Config) string {
	args := []string{
		"console=hvc0",
		// A guest panic reboots immediately instead of wedging the VM (holding RAM +
		// the host process) forever — right for a single-purpose throwaway VM.
		"panic=-1",
		// Single-tenant throwaway VM: CPU side-channel mitigations only defend against
		// cross-tenant attackers we don't have, and they tax every syscall.
		"mitigations=off",
		"root=/dev/vda",
		// The ext4 root is read-only; the init pivots into a tmpfs overlay so all writes
		// (apt, /var, dpkg) land in RAM and the disk image stays pristine.
		"ro",
		"rootfstype=ext4",
		"init=" + cfg.Init,
	}
	args = append(args, cfg.Cmdline...)

	return strings.Join(args, " ")
}

// New assembles the VM configuration. It does not start it.
func New(cfg Config) (*VM, error) {
	boot, err := vz.NewLinuxBootLoader(cfg.Kernel, vz.WithCommandLine(cmdline(cfg)))
	if err != nil {
		return nil, fmt.Errorf("bootloader: %w", err)
	}

	vmc, err := vz.NewVirtualMachineConfiguration(boot, cfg.CPUs, cfg.MemoryMiB*1024*1024)
	if err != nil {
		return nil, fmt.Errorf("vm configuration: %w", err)
	}

	if err := configureStorage(vmc, cfg); err != nil {
		return nil, err
	}

	if err := configureConsole(vmc, cfg); err != nil {
		return nil, err
	}

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return nil, fmt.Errorf("entropy: %w", err)
	}

	vmc.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

	if cfg.Network {
		if err := configureNAT(vmc); err != nil {
			return nil, err
		}
	}

	if err := configureShares(vmc, cfg); err != nil {
		return nil, err
	}

	if ok, err := vmc.Validate(); !ok || err != nil {
		return nil, fmt.Errorf("configuration invalid: %w", err)
	}

	machine, err := vz.NewVirtualMachine(vmc)
	if err != nil {
		return nil, fmt.Errorf("new vm: %w", err)
	}

	return &VM{vm: machine}, nil
}

func configureStorage(vmc *vz.VirtualMachineConfiguration, cfg Config) error {
	// The Debian rootfs is the ONLY block device: /dev/vda, attached READ-ONLY. The guest
	// never mutates it — the init overlays a tmpfs on top (writes go to RAM) — so the same
	// pristine image is shared by every boot with no per-build clone.
	attachment, err := vz.NewDiskImageStorageDeviceAttachment(cfg.RootDisk, true)
	if err != nil {
		return fmt.Errorf("root disk %s: %w", cfg.RootDisk, err)
	}

	blockDev, err := vz.NewVirtioBlockDeviceConfiguration(attachment)
	if err != nil {
		return fmt.Errorf("root blk %s: %w", cfg.RootDisk, err)
	}

	vmc.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{blockDev})

	return nil
}

func configureConsole(vmc *vz.VirtualMachineConfiguration, cfg Config) error {
	write := cfg.Console
	if write == nil {
		devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("open %s: %w", os.DevNull, err)
		}

		write = devNull
	}

	// read side: the guest never reads console input, so feed it /dev/null.
	readNull, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}

	att, err := vz.NewFileHandleSerialPortAttachment(readNull, write)
	if err != nil {
		return fmt.Errorf("console attachment: %w", err)
	}

	console, err := vz.NewVirtioConsoleDeviceSerialPortConfiguration(att)
	if err != nil {
		return fmt.Errorf("console: %w", err)
	}

	vmc.SetSerialPortsVirtualMachineConfiguration([]*vz.VirtioConsoleDeviceSerialPortConfiguration{console})

	return nil
}

func configureNAT(vmc *vz.VirtualMachineConfiguration) error {
	// VZ's built-in NAT (unprivileged): the guest gets outbound via a virtio-net that
	// DHCPs against VZ's host-side DHCP/DNS. This replaces ossein's gvisor-tap-vsock
	// netstack — the build only needs plain outbound to snapshot.debian.org.
	att, err := vz.NewNATNetworkDeviceAttachment()
	if err != nil {
		return fmt.Errorf("nat attachment: %w", err)
	}

	netdev, err := vz.NewVirtioNetworkDeviceConfiguration(att)
	if err != nil {
		return fmt.Errorf("net device: %w", err)
	}

	vmc.SetNetworkDevicesVirtualMachineConfiguration([]*vz.VirtioNetworkDeviceConfiguration{netdev})

	return nil
}

func configureShares(vmc *vz.VirtualMachineConfiguration, cfg Config) error {
	if len(cfg.Shares) == 0 {
		return nil
	}

	fsDevices := make([]vz.DirectorySharingDeviceConfiguration, 0, len(cfg.Shares))

	for _, share := range cfg.Shares {
		fsDev, err := vz.NewVirtioFileSystemDeviceConfiguration(share.Tag)
		if err != nil {
			return fmt.Errorf("virtiofs %s: %w", share.Tag, err)
		}

		dir, err := vz.NewSharedDirectory(share.Dir, share.ReadOnly)
		if err != nil {
			return fmt.Errorf("shared dir %s: %w", share.Dir, err)
		}

		single, err := vz.NewSingleDirectoryShare(dir)
		if err != nil {
			return fmt.Errorf("share %s: %w", share.Dir, err)
		}

		fsDev.SetDirectoryShare(single)
		fsDevices = append(fsDevices, fsDev)
	}

	vmc.SetDirectorySharingDevicesVirtualMachineConfiguration(fsDevices)

	return nil
}

// Start boots the VM and returns once it reports running.
func (m *VM) Start() error {
	if err := m.vm.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	deadline := time.After(30 * time.Second)

	for {
		select {
		case state := <-m.vm.StateChangedNotify():
			if state == vz.VirtualMachineStateRunning {
				return nil
			}

			if state == vz.VirtualMachineStateError || state == vz.VirtualMachineStateStopped {
				return fmt.Errorf("%w: entered state %v during start", errStartFailed, state)
			}
		case <-deadline:
			if m.vm.State() == vz.VirtualMachineStateRunning {
				return nil
			}

			return fmt.Errorf("%w: state %v at deadline", errStartFailed, m.vm.State())
		}
	}
}

// Wait blocks until the guest powers itself off (Stopped), errors, or ctx is
// cancelled (on which it hard-stops the VM). The build's own exit status is
// conveyed out-of-band via a status file on a shared dir, not by this return.
func (m *VM) Wait(ctx context.Context) error {
	for {
		select {
		case state := <-m.vm.StateChangedNotify():
			//nolint:exhaustive // only the two terminal states matter; default keeps waiting
			switch state {
			case vz.VirtualMachineStateStopped:
				return nil
			case vz.VirtualMachineStateError:
				return errRunError
			default:
				// non-terminal transition; keep waiting
			}
		case <-ctx.Done():
			_ = m.Stop()

			return fmt.Errorf("vm wait cancelled: %w", ctx.Err())
		}
	}
}

// Stop hard-stops the VM. The guest is ephemeral, so there is nothing to flush.
func (m *VM) Stop() error {
	if m.vm.State() == vz.VirtualMachineStateStopped {
		return nil
	}

	if m.vm.CanStop() {
		if err := m.vm.Stop(); err != nil {
			return fmt.Errorf("stopping vm: %w", err)
		}
	}

	return nil
}
