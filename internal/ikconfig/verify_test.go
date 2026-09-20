package ikconfig_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/farcloser/ossein-kernel/internal/ikconfig"
)

const sample = `#
# Automatically generated file; DO NOT EDIT.
# Linux/arm64 7.2.6 Kernel Configuration
#
CONFIG_ARM64=y
CONFIG_NR_CPUS=512
CONFIG_CMDLINE="console=hvc0"
CONFIG_PHYS_OFFSET=0x40000000
CONFIG_LTO_CLANG_THIN=y
# CONFIG_ARM64_SME is not set
CONFIG_EXT4_FS=m
CONFIG_LOG_BUF_SHIFT=14
`

func TestParseStates(t *testing.T) {
	t.Parallel()

	config := ikconfig.Parse([]byte(sample))

	cases := map[string]ikconfig.State{
		"ARM64":      ikconfig.StateY,
		"EXT4_FS":    ikconfig.StateM,
		"ARM64_SME":  ikconfig.StateN,
		"NEVER_SEEN": ikconfig.StateAbsent,
		// A numeric is decided but neither y nor m: no state of its own.
		"NR_CPUS": ikconfig.StateAbsent,
	}

	for symbol, want := range cases {
		if got := config.State(symbol); got != want {
			t.Errorf("State(%q) = %q, want %q", symbol, got, want)
		}
	}
}

func TestGoldenKeepsEveryDecidedValueSorted(t *testing.T) {
	t.Parallel()

	got := ikconfig.Parse([]byte(sample)).Golden()

	want := []string{
		`CONFIG_ARM64=y`,
		`CONFIG_CMDLINE="console=hvc0"`,
		`CONFIG_EXT4_FS=m`,
		`CONFIG_LOG_BUF_SHIFT=14`,
		`CONFIG_LTO_CLANG_THIN=y`,
		`CONFIG_NR_CPUS=512`,
		`CONFIG_PHYS_OFFSET=0x40000000`,
	}

	if !slices.Equal(got, want) {
		t.Fatalf("Golden = %q, want %q", got, want)
	}
}

func TestVerifyHoldsAndFails(t *testing.T) {
	t.Parallel()

	config := ikconfig.Parse([]byte(sample))

	policy := []ikconfig.Section{{Title: "t", Decisions: []ikconfig.Decision{
		{Symbol: "ARM64", Want: ikconfig.WantY, Why: "arch"},
		{Symbol: "ARM64_SME", Want: ikconfig.WantNotY, Why: "explicitly off"},
		{Symbol: "NEVER_SEEN", Want: ikconfig.WantNotY, Why: "absent counts as off"},
	}}}

	var out bytes.Buffer

	if !ikconfig.Verify(config, policy, config.Golden(), &out) {
		t.Fatalf("Verify failed on a config that meets its policy:\n%s", out.String())
	}

	if !strings.Contains(out.String(), "no drift (7 enabled symbols") {
		t.Fatalf("report lacks the no-drift line:\n%s", out.String())
	}

	failing := []ikconfig.Section{{Title: "t", Decisions: []ikconfig.Decision{
		{Symbol: "EXT4_FS", Want: ikconfig.WantY, Why: "built in, not a module"},
	}}}

	out.Reset()

	if ikconfig.Verify(config, failing, config.Golden(), &out) {
		t.Fatal("Verify passed a decision the config does not meet")
	}

	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "CONFIG_EXT4_FS") {
		t.Fatalf("report does not name the failing symbol:\n%s", out.String())
	}
}

func TestVerifyReportsDrift(t *testing.T) {
	t.Parallel()

	config := ikconfig.Parse([]byte(sample))

	golden := config.Golden()
	golden = slices.DeleteFunc(golden, func(line string) bool { return line == "CONFIG_ARM64=y" })
	golden = append(golden, "CONFIG_GONE=y")
	slices.Sort(golden)

	var out bytes.Buffer

	if ikconfig.Verify(config, nil, golden, &out) {
		t.Fatal("Verify passed with drift against the golden")
	}

	report := out.String()
	for _, want := range []string{"+ CONFIG_ARM64=y", "- CONFIG_GONE=y", "turned ON", "now OFF"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

func TestVerifyWithoutGolden(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	if !ikconfig.Verify(ikconfig.Parse([]byte(sample)), nil, nil, &out) {
		t.Fatal("Verify failed with no golden to diff against")
	}

	if !strings.Contains(out.String(), "no kernel/config/kernel-golden committed") {
		t.Fatalf("report lacks the missing-golden note:\n%s", out.String())
	}
}
