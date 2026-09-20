// clearsign-verify checks a PGP cleartext-signed file against one pinned public key and
// prints the signed text. Anything short of a valid signature by that exact key is an error
// with nothing printed, so a caller can pipe the output without a second check.
package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/alecthomas/kong"
)

var (
	errNotOneKey     = errors.New("key file must hold exactly one public key")
	errFingerprint   = errors.New("key fingerprint does not match the pinned one")
	errNotClearsign  = errors.New("no cleartext-signed block in file")
	errWrongSigner   = errors.New("signed by a key other than the pinned one")
	errTrailingBytes = errors.New("bytes after the cleartext-signed block")
)

type cli struct {
	Key         string `arg:"" help:"armored public key file"                                type:"existingfile"`
	Fingerprint string `arg:"" help:"expected fingerprint of that key (hex, spaces ignored)"`
	File        string `arg:"" help:"cleartext-signed file to verify"                        type:"existingfile"`
}

func main() {
	var root cli

	kong.Parse(&root,
		kong.Name("clearsign-verify"),
		kong.Description("Verify a PGP cleartext-signed file against one pinned key; print the signed text."),
		kong.UsageOnError(),
	)

	text, err := verify(root.Key, root.Fingerprint, root.File)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clearsign-verify:", err)
		os.Exit(1)
	}

	if _, err := os.Stdout.Write(text); err != nil {
		fmt.Fprintln(os.Stderr, "clearsign-verify:", err)
		os.Exit(1)
	}
}

func verify(keyPath, fingerprint, filePath string) ([]byte, error) {
	keyData, err := os.ReadFile(keyPath) // #nosec G304 -- the operator names the key on the command line
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}

	ring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(keyData))
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}

	if len(ring) != 1 {
		return nil, fmt.Errorf("%w: got %d", errNotOneKey, len(ring))
	}

	want := strings.ToUpper(strings.ReplaceAll(fingerprint, " ", ""))
	if got := fingerprintOf(ring[0]); got != want {
		return nil, fmt.Errorf("%w: key is %s", errFingerprint, got)
	}

	signed, err := os.ReadFile(filePath) // #nosec G304 -- the operator names the file on the command line
	if err != nil {
		return nil, fmt.Errorf("file: %w", err)
	}

	block, rest := clearsign.Decode(signed)
	if block == nil {
		return nil, errNotClearsign
	}

	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errTrailingBytes
	}

	signer, err := block.VerifySignature(ring, nil)
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}

	// VerifySignature accepts any key in the ring; the ring holds one, but say so explicitly
	// rather than rely on the count above.
	if fingerprintOf(signer) != want {
		return nil, errWrongSigner
	}

	return block.Plaintext, nil
}

func fingerprintOf(entity *openpgp.Entity) string {
	return strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint))
}
