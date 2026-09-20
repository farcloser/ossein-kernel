// Command ikconfig prints the .config a kernel image embeds (CONFIG_IKCONFIG).
// It replaces the kernel's scripts/extract-ikconfig, which needs GNU tr and
// fails on macOS, and does so with nothing outside the pinned toolchain.
package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"

	"github.com/farcloser/ossein-kernel/internal/ikconfig"
)

type cli struct {
	Image string `arg:"" help:"kernel image built with CONFIG_IKCONFIG" type:"existingfile"`
}

func main() {
	var root cli

	kong.Parse(&root,
		kong.Name("ikconfig"),
		kong.Description("Print the .config embedded in a kernel image."),
		kong.UsageOnError(),
	)

	if err := run(root.Image); err != nil {
		fmt.Fprintln(os.Stderr, "ikconfig:", err)
		os.Exit(1)
	}
}

func run(path string) error {
	image, err := os.ReadFile(path) // #nosec G304 -- the operator names the image on the command line
	if err != nil {
		return fmt.Errorf("read image: %w", err)
	}

	config, err := ikconfig.Extract(image)
	if err != nil {
		return err
	}

	if _, err := os.Stdout.Write(config); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}
