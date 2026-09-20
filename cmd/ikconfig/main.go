// Command ikconfig reads the .config a kernel image embeds (CONFIG_IKCONFIG)
// and judges it: the shipped binary, not the fragment, is what carries the
// decisions. It replaces the kernel's scripts/extract-ikconfig, which needs
// GNU tr and fails on macOS, with nothing outside the pinned toolchain.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/farcloser/ossein-kernel/internal/ikconfig"
)

var errDrift = errors.New("a fragment line reverted or the enabled-symbol set changed")

type cli struct {
	Extract extractCmd `cmd:"" help:"Print the embedded .config."`
	Golden  goldenCmd  `cmd:"" help:"Print the normalized set of decided symbols, sorted (the golden snapshot)."`
	Verify  verifyCmd  `cmd:"" help:"Check every decision in the policy and the drift against a committed golden."`
}

type extractCmd struct {
	Image string `arg:"" help:"kernel image built with CONFIG_IKCONFIG" type:"existingfile"`
}

type goldenCmd struct {
	Image string `arg:"" help:"kernel image built with CONFIG_IKCONFIG" type:"existingfile"`
}

type verifyCmd struct {
	Image  string `arg:""                                                                                              help:"kernel image built with CONFIG_IKCONFIG" type:"existingfile"`
	Golden string `help:"committed golden snapshot to diff against (skipped with a note when the file does not exist)" name:"golden"                                  type:"path"`
}

func main() {
	var root cli

	ctx := kong.Parse(&root,
		kong.Name("ikconfig"),
		kong.Description("Read and verify the .config embedded in a kernel image."),
		kong.UsageOnError(),
	)

	if err := ctx.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "ikconfig:", err)
		os.Exit(1)
	}
}

func (c *extractCmd) Run() error {
	config, err := embedded(c.Image)
	if err != nil {
		return err
	}

	return write(config)
}

func (c *goldenCmd) Run() error {
	config, err := embedded(c.Image)
	if err != nil {
		return err
	}

	return write([]byte(strings.Join(ikconfig.Parse(config).Golden(), "\n") + "\n"))
}

func (c *verifyCmd) Run() error {
	config, err := embedded(c.Image)
	if err != nil {
		return err
	}

	golden, err := goldenLines(c.Golden)
	if err != nil {
		return err
	}

	ok := ikconfig.Verify(ikconfig.Parse(config), ikconfig.Policy, golden, os.Stdout)

	if !ok {
		return fmt.Errorf("%w: see FAIL/DRIFT above", errDrift)
	}

	return write([]byte("\nconfig OK: every intended decision survived, and no unexpected symbol drift.\n"))
}

// embedded is the .config a kernel image carries.
func embedded(path string) ([]byte, error) {
	image, err := os.ReadFile(path) // #nosec G304 -- the operator names the image on the command line
	if err != nil {
		return nil, fmt.Errorf("read image: %w", err)
	}

	config, err := ikconfig.Extract(image)
	if err != nil {
		return nil, err
	}

	if len(config) == 0 {
		return nil, fmt.Errorf("%w: extracted an empty config from %s", ikconfig.ErrNoConfig, path)
	}

	return config, nil
}

// goldenLines reads the committed golden; no path or no file is nil, which
// Verify reports as a note rather than a failure.
func goldenLines(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path) // #nosec G304 -- the operator names the golden on the command line
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read golden: %w", err)
	}

	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"), nil
}

func write(data []byte) error {
	if _, err := os.Stdout.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	return nil
}
