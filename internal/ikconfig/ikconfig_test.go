package ikconfig_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"testing"

	"github.com/farcloser/ossein-kernel/internal/ikconfig"
)

// image wraps config the way kernel/configs.c does, between padding that
// stands in for the rest of the kernel.
func image(t *testing.T, config string) []byte {
	t.Helper()

	var stream bytes.Buffer

	writer := gzip.NewWriter(&stream)
	if _, err := writer.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	out.WriteString("\x7fELF padding before the config ")
	out.WriteString("IKCFG_ST")
	out.Write(stream.Bytes())
	out.WriteString("IKCFG_ED")
	out.WriteString(" padding after, with a gzip header \x1f\x8b\x08 that must not be read")

	return out.Bytes()
}

func TestExtract(t *testing.T) {
	t.Parallel()

	const config = "#\n# Automatically generated file; DO NOT EDIT.\n#\nCONFIG_ARM64=y\nCONFIG_LTO_CLANG_THIN=y\n# CONFIG_ARM64_SME is not set\n"

	got, err := ikconfig.Extract(image(t, config))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if string(got) != config {
		t.Fatalf("Extract = %q, want %q", got, config)
	}
}

func TestExtractWithoutMarker(t *testing.T) {
	t.Parallel()

	_, err := ikconfig.Extract([]byte("a kernel built without CONFIG_IKCONFIG"))
	if !errors.Is(err, ikconfig.ErrNoConfig) {
		t.Fatalf("Extract error = %v, want %v", err, ikconfig.ErrNoConfig)
	}
}

func TestExtractTruncatedStream(t *testing.T) {
	t.Parallel()

	whole := image(t, "CONFIG_ARM64=y\n")
	cut := whole[:bytes.Index(whole, []byte("IKCFG_ST"))+12]

	if _, err := ikconfig.Extract(cut); err == nil {
		t.Fatal("Extract on a truncated stream returned no error")
	}
}
