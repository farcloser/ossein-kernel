// Package ikconfig reads the .config a Linux kernel embeds when built with
// CONFIG_IKCONFIG: a gzip stream between the IKCFG_ST and IKCFG_ED markers.
package ikconfig

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// ErrNoConfig is an image without an embedded config (CONFIG_IKCONFIG not set).
var ErrNoConfig = errors.New("no IKCFG_ST marker in image")

// The markers the kernel wraps the gzip stream in (kernel/configs.c).
const (
	startMarker = "IKCFG_ST"
	endMarker   = "IKCFG_ED"
)

// Extract returns the .config embedded in a kernel image.
func Extract(image []byte) ([]byte, error) {
	_, stream, found := bytes.Cut(image, []byte(startMarker))
	if !found {
		return nil, ErrNoConfig
	}

	// The end marker bounds the search, not the decode: gzip carries its own length.
	stream, _, _ = bytes.Cut(stream, []byte(endMarker))

	reader, err := gzip.NewReader(bytes.NewReader(stream))
	if err != nil {
		return nil, fmt.Errorf("embedded config: %w", err)
	}

	// One member only: whatever follows the stream is image, not another gzip member.
	reader.Multistream(false)

	config, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("embedded config: %w", err)
	}

	return config, nil
}
