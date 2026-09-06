//nolint:testpackage // white-box: drives the unexported unpack path and path mapper
package rootfs

import (
	"archive/tar"
	"bytes"
	"io"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem/ext4"
)

// fuzzImageBytes sizes the throwaway ext4 image each FuzzUnpackTar iteration
// formats: sparse on disk, so the cost is the format, not the bytes. Two block
// groups at least — go-diskfs needs a backup group for the resize inode and
// refuses anything smaller.
const fuzzImageBytes = 256 << 20

// FuzzDiskPath pins the path mapper to what go-diskfs will accept: never a
// leading slash, always clean, and stable under a second application.
func FuzzDiskPath(f *testing.F) {
	for _, seed := range []string{"", ".", "/", "./", "//", "/etc/passwd", "./usr/bin/", "../x", "a//b", "/a/./b/../c"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got := diskPath(name)

		if strings.HasPrefix(got, "/") {
			t.Fatalf("diskPath(%q) = %q: leading slash, which go-diskfs rejects", name, got)
		}

		if got != path.Clean(got) {
			t.Fatalf("diskPath(%q) = %q is not clean", name, got)
		}

		if again := diskPath(got); again != got {
			t.Fatalf("diskPath is not idempotent: %q -> %q -> %q", name, got, again)
		}
	})
}

func fuzzSeedTar() []byte {
	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)
	entries := []struct {
		hdr  tar.Header
		body string
	}{
		{hdr: tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "./etc/", Typeflag: tar.TypeDir, Mode: 0o755}},
		{hdr: tar.Header{Name: "./etc/hostname", Typeflag: tar.TypeReg, Mode: 0o644}, body: "guest\n"},
		{hdr: tar.Header{Name: "./etc/alias", Typeflag: tar.TypeLink, Linkname: "etc/hostname"}},
		{hdr: tar.Header{Name: "./lib", Typeflag: tar.TypeSymlink, Linkname: "usr/lib"}},
	}

	for _, entry := range entries {
		entry.hdr.Size = int64(len(entry.body))
		_ = writer.WriteHeader(&entry.hdr)
		_, _ = io.WriteString(writer, entry.body)
	}

	_ = writer.Close()

	return buf.Bytes()
}

// FuzzUnpackTar unpacks an arbitrary archive into a freshly formatted ext4
// image. The archive is the untrusted input of the whole builder (it comes
// from a registry), so the contract is: a malformed one fails with an error,
// never a panic — in this code or in the ext4 writer beneath it.
func FuzzUnpackTar(f *testing.F) {
	f.Add(fuzzSeedTar())
	f.Add([]byte{})
	f.Add([]byte("not a tar"))

	f.Fuzz(func(t *testing.T, raw []byte) {
		out := filepath.Join(t.TempDir(), "fs.ext4")

		back, err := file.CreateFromPath(out, fuzzImageBytes)
		if err != nil {
			t.Fatalf("create image file: %v", err)
		}

		defer func() { _ = back.Close() }()

		efs, err := ext4.Create(back, fuzzImageBytes, 0, ext4SectorSize, &ext4.Params{
			SectorsPerBlock: sectorsPerBlock4k,
			VolumeName:      volumeLabel,
		})
		if err != nil {
			t.Fatalf("format ext4: %v", err)
		}

		_ = unpackTar(efs, bytes.NewReader(raw)) // an error is the expected outcome for garbage
	})
}
