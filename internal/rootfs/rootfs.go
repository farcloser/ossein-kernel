// Package rootfs builds the build VM's Debian root disk on the host, in pure Go,
// with no dependency on ossein's image runtime. It pulls the pinned OCI image
// (by digest), flattens it to a single rootfs tar (mutate.Extract applies the
// layer whiteouts), unpacks that into an ext4 image via go-diskfs, and drops our
// static init binary in at /ossein-init. The resulting ext4 is attached
// read-only as the VM's /dev/vda; the guest init overlays a tmpfs on it, so
// every boot shares the same pristine image and all writes land in RAM.
//
// This reuses the exact ext4 writer ossein uses for its initfs and cache disks —
// the only new surface is the OCI pull + tar-into-ext4 unpack, which is self-
// contained here.
package rootfs

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

const (
	ext4SectorSize    = 512
	sectorsPerBlock4k = 8
	volumeLabel       = "ossein-debian"
	initMode          = 0o755
	slash             = "/"

	// initGuestPath is where the init binary lands in the rootfs; it must match the
	// kernel init= arg (internal/vm sets init=/ossein-init).
	initGuestPath = "/ossein-init"
)

// Build pulls imageRef (a digest-pinned OCI reference), unpacks it into a new ext4
// image at outExt4 of the given nominal size (sparse), and embeds initBin as
// /ossein-init. The image is arm64/linux.
func Build(ctx context.Context, imageRef, initBin, outExt4 string, sizeBytes int64) error {
	img, err := crane.Pull(imageRef,
		crane.WithContext(ctx),
		crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: "arm64"}),
	)
	if err != nil {
		return fmt.Errorf("pull %s: %w", imageRef, err)
	}

	stream := mutate.Extract(img)
	defer func() { _ = stream.Close() }()

	_ = os.Remove(outExt4)

	back, err := file.CreateFromPath(outExt4, sizeBytes)
	if err != nil {
		return fmt.Errorf("create image file: %w", err)
	}

	efs, err := ext4.Create(back, sizeBytes, 0, ext4SectorSize, &ext4.Params{
		SectorsPerBlock: sectorsPerBlock4k,
		VolumeName:      volumeLabel,
	})
	if err != nil {
		return cleanup(back, outExt4, fmt.Errorf("format ext4: %w", err))
	}

	if err := unpackTar(efs, stream); err != nil {
		return cleanup(back, outExt4, err)
	}

	// /ram is the init's overlay mountpoint. It must be baked in: the root is mounted
	// read-only, so the init cannot create it at boot. Keep this in sync with the init's
	// overlayMountpoint.
	if err := efs.Mkdir("ram"); err != nil {
		return cleanup(back, outExt4, fmt.Errorf("mkdir /ram: %w", err))
	}

	if err := embedInit(efs, initBin); err != nil {
		return cleanup(back, outExt4, err)
	}

	if err := back.Close(); err != nil {
		_ = os.Remove(outExt4)

		return fmt.Errorf("flush image: %w", err)
	}

	return nil
}

func cleanup(back io.Closer, out string, cause error) error {
	_ = back.Close()
	_ = os.Remove(out)

	return cause
}

// unpackTar writes every entry of the flattened rootfs tar into the ext4. The
// stream is already whiteout-resolved by mutate.Extract, so there are no `.wh.`
// markers to handle. Device/fifo nodes don't appear in a Docker base image, so
// they're skipped.
func unpackTar(efs *ext4.FileSystem, src io.Reader) error {
	tarReader := tar.NewReader(src)

	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}

		if err != nil {
			return fmt.Errorf("read rootfs tar: %w", err)
		}

		name := diskPath(hdr.Name)
		if name == "" || name == "." {
			continue
		}

		if err := writeEntry(efs, name, hdr, tarReader); err != nil {
			return err
		}
	}

	return nil
}

func writeEntry(efs *ext4.FileSystem, name string, hdr *tar.Header, tarReader io.Reader) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := efs.Mkdir(name); err != nil {
			return fmt.Errorf("mkdir %s: %w", name, err)
		}

		return applyMeta(efs, name, hdr)

	case tar.TypeReg:
		if err := mkParent(efs, name); err != nil {
			return err
		}

		if err := writeReg(efs, name, tarReader); err != nil {
			return err
		}

		return applyMeta(efs, name, hdr)

	case tar.TypeSymlink:
		if err := mkParent(efs, name); err != nil {
			return err
		}

		if err := efs.Symlink(hdr.Linkname, name); err != nil {
			return fmt.Errorf("symlink %s -> %s: %w", name, hdr.Linkname, err)
		}

		return nil // ext4 symlink perms are fixed; no chmod needed

	case tar.TypeLink:
		if err := mkParent(efs, name); err != nil {
			return err
		}

		// go-diskfs's ext4 writer doesn't implement hardlinks. Materialize them as
		// absolute symlinks instead: functionally identical for running a build (same
		// bytes, argv[0] is still the link's own name), and — unlike a hardlink — the
		// target need not already be extracted. Debian uses these for multi-call and
		// versioned binaries (e.g. perl5.40.1 -> perl).
		target := slash + diskPath(hdr.Linkname)
		if err := efs.Symlink(target, name); err != nil {
			return fmt.Errorf("hardlink-as-symlink %s -> %s: %w", name, target, err)
		}

		return nil

	default:
		// char/block/fifo/etc — absent from a Docker base image; skip.
		return nil
	}
}

func writeReg(efs *ext4.FileSystem, name string, src io.Reader) error {
	out, err := efs.OpenFile(name, os.O_CREATE|os.O_RDWR|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}

	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()

		return fmt.Errorf("write %s: %w", name, err)
	}

	if err := out.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}

	return nil
}

// applyMeta sets ownership and mode from the tar header. Debian's base is almost
// entirely root:root, but honour whatever the archive says.
func applyMeta(efs *ext4.FileSystem, name string, hdr *tar.Header) error {
	//nolint:gosec // G115: a tar file mode always fits FileMode's low bits
	if err := efs.Chmod(name, os.FileMode(hdr.Mode).Perm()); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}

	if err := efs.Chown(name, hdr.Uid, hdr.Gid); err != nil {
		return fmt.Errorf("chown %s: %w", name, err)
	}

	return nil
}

// mkParent ensures the parent directory chain exists (tar streams are usually
// ordered parents-first, but not guaranteed to emit every intermediate dir).
func mkParent(efs *ext4.FileSystem, name string) error {
	dir := path.Dir(name)
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}

	if err := efs.Mkdir(dir); err != nil {
		return fmt.Errorf("mkdir parent %s: %w", dir, err)
	}

	return nil
}

func embedInit(efs *ext4.FileSystem, initBin string) error {
	src, err := os.Open(initBin) //nolint:gosec // G304: initBin is a program-supplied path
	if err != nil {
		return fmt.Errorf("open init binary %s: %w", initBin, err)
	}
	defer func() { _ = src.Close() }()

	name := diskPath(initGuestPath)
	if err := writeReg(efs, name, src); err != nil {
		return err
	}

	if err := efs.Chmod(name, initMode); err != nil {
		return fmt.Errorf("chmod init: %w", err)
	}

	return nil
}

// diskPath converts a tar/absolute path to the relative, leading-slash-free form
// go-diskfs requires (its validatePath rejects a leading '/').
func diskPath(p string) string {
	p = strings.TrimPrefix(p, "."+slash)
	p = strings.TrimPrefix(p, slash)
	p = strings.TrimSuffix(p, slash)

	return path.Clean(p)
}
