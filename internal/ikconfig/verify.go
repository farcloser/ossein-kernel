package ikconfig

import (
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// State is what a .config says about a symbol.
type State string

// The four states a symbol can be in. StateN is an explicit "is not set"; a
// symbol the config never mentions is StateAbsent, and a decision that wants
// a symbol off accepts either.
const (
	StateY      State = "y"
	StateM      State = "m"
	StateN      State = "n"
	StateAbsent State = "absent"
)

// Want is what a decision demands of a symbol.
type Want int

// WantY demands =y; WantNotY accepts anything else, n and absent alike.
const (
	WantY Want = iota
	WantNotY
)

func (w Want) String() string {
	if w == WantY {
		return "y"
	}

	return "not-y"
}

func (w Want) met(state State) bool {
	if w == WantY {
		return state == StateY
	}

	return state != StateY
}

// Decision is one intended config choice and the reason it was made.
type Decision struct {
	Symbol string
	Want   Want
	Why    string
}

// Section groups decisions under the heading the report prints them with.
type Section struct {
	Title     string
	Decisions []Decision
}

// Config is a parsed .config.
type Config struct {
	title  string
	states map[string]State
	golden []string
}

// goldenLine is a decided symbol: =y, =m, or a numeric, hex or string value.
// Numerics and strings count, not only bools: arm64 defaults NR_CPUS=512, which
// force-selects CPUMASK_OFFSTACK, and a =y-only golden would never see it move.
var goldenLine = regexp.MustCompile(`^CONFIG_[A-Z0-9_]+=(y|m|[0-9]+|0x[0-9a-fA-F]+|".*")$`)

// Parse reads a .config into states and the golden set.
func Parse(config []byte) Config {
	parsed := Config{states: map[string]State{}}

	for line := range strings.SplitSeq(string(config), "\n") {
		switch {
		case parsed.title == "" && strings.Contains(line, "Kernel Configuration"):
			parsed.title = strings.TrimPrefix(line, "# ")
		case strings.HasPrefix(line, "# CONFIG_") && strings.HasSuffix(line, " is not set"):
			symbol := strings.TrimSuffix(strings.TrimPrefix(line, "# CONFIG_"), " is not set")
			parsed.states[symbol] = StateN
		case strings.HasPrefix(line, "CONFIG_"):
			symbol, value, _ := strings.Cut(strings.TrimPrefix(line, "CONFIG_"), "=")

			switch value {
			case "y":
				parsed.states[symbol] = StateY
			case "m":
				parsed.states[symbol] = StateM
			default:
				// A numeric, hex or string value: decided, but neither y nor m.
			}

			if goldenLine.MatchString(line) {
				parsed.golden = append(parsed.golden, line)
			}
		default:
			// Comments and blank lines.
		}
	}

	// Byte order, the same order LC_ALL=C sort gave the committed golden.
	slices.Sort(parsed.golden)

	return parsed
}

// State reports a symbol's state.
func (c Config) State(symbol string) State {
	if state, ok := c.states[symbol]; ok {
		return state
	}

	return StateAbsent
}

// Golden is the normalized set of decided symbols, sorted: what the committed
// golden snapshot holds and what Verify diffs against it.
func (c Config) Golden() []string {
	return slices.Clone(c.golden)
}

// Verify prints the report for every decision in policy and, when golden is
// not nil, the drift between the config's golden set and it. It returns
// whether every decision held and nothing drifted.
func Verify(config Config, policy []Section, golden []string, out io.Writer) bool {
	held := true

	fmt.Fprintf(out, "=== verifying shipped config: %s ===\n", config.title)

	for _, section := range policy {
		fmt.Fprintf(out, "--- %s ---\n", section.Title)

		for _, decision := range section.Decisions {
			got := config.State(decision.Symbol)

			if decision.Want.met(got) {
				fmt.Fprintf(
					out,
					"  \033[32mok\033[0m   %-38s = %-6s  %s\n",
					"CONFIG_"+decision.Symbol,
					got,
					decision.Why,
				)

				continue
			}

			fmt.Fprintf(out, "  \033[31mFAIL\033[0m %-38s = %-6s  (want %s) %s\n",
				"CONFIG_"+decision.Symbol, got, decision.Want, decision.Why)

			held = false
		}
	}

	if golden == nil {
		fmt.Fprintln(
			out,
			"--- golden-config: no kernel/config/kernel-golden committed (run 'just kernel-golden' to create it) ---",
		)

		return held
	}

	added, removed := drift(golden, config.golden)

	if len(added) == 0 && len(removed) == 0 {
		fmt.Fprintf(
			out,
			"--- golden-config: no drift (%d enabled symbols match kernel/config/kernel-golden) ---\n",
			len(golden),
		)

		return held
	}

	fmt.Fprintln(out, "--- golden-config DRIFT vs kernel/config/kernel-golden ---")

	if len(added) > 0 {
		fmt.Fprintln(out, "  turned ON (unexpected — new default-y or drift):")

		for _, line := range added {
			fmt.Fprintf(out, "    + %s\n", line)
		}
	}

	if len(removed) > 0 {
		fmt.Fprintln(out, "  now OFF (expected symbol lost):")

		for _, line := range removed {
			fmt.Fprintf(out, "    - %s\n", line)
		}
	}

	fmt.Fprintln(
		out,
		"  → review the delta; if intentional, run 'just kernel-golden' and commit kernel/config/kernel-golden",
	)

	return false
}

// drift splits two sorted sets: lines only in got (added) and only in golden (removed).
func drift(golden, got []string) (added, removed []string) {
	inGolden := map[string]bool{}
	for _, line := range golden {
		inGolden[line] = true
	}

	inGot := map[string]bool{}
	for _, line := range got {
		inGot[line] = true
	}

	for _, line := range got {
		if !inGolden[line] {
			added = append(added, line)
		}
	}

	for _, line := range golden {
		if !inGot[line] {
			removed = append(removed, line)
		}
	}

	return added, removed
}
