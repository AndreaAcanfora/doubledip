package hunt

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/AndreaAcanfora/doubledip/internal/analyze"
)

// Text writes the report for people.
func (r *Report) Text(w io.Writer, color bool) {
	red, green, yellow, dim, bold, reset := "", "", "", "", "", ""
	if color {
		red, green, yellow, dim, bold, reset = "\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[2m", "\x1b[1m", "\x1b[0m"
	}
	txs := map[int]bool{}
	for _, s := range r.Steps {
		txs[s.TxIndex] = true
	}
	fmt.Fprintf(w, "\n%srecorded%s %s in %s\n", dim, reset, plural(len(r.Steps), "statement"), plural(len(txs), "transaction"))
	if len(r.Findings) == 0 {
		fmt.Fprintf(w, "\n%s✓ no read-then-write pattern to race%s: every write either has no earlier read of its table or nothing was written.\n", green, reset)
		return
	}
	for _, f := range r.Findings {
		fmt.Fprintln(w)
		switch f.Outcome {
		case Confirmed:
			fmt.Fprintf(w, "%s%s✗ RACE CONFIRMED%s  %s on %s\n", bold, red, reset, f.Pattern, f.Write.Stmt.Target)
		case Held:
			fmt.Fprintf(w, "%s✓ safe%s  %s on %s: both requests were held at the write and the result matched a serial run\n", green, reset, f.Pattern, f.Write.Stmt.Target)
		case NotReached:
			fmt.Fprintf(w, "%s✓ serialized%s  %s on %s: the second request could not reach the write while the first waited (a lock or an earlier check stopped it)\n", green, reset, f.Pattern, f.Write.Stmt.Target)
		default:
			fmt.Fprintf(w, "%s? inconclusive%s  %s on %s: %s\n", yellow, reset, f.Pattern, f.Write.Stmt.Target, f.Note)
		}
		step := func(label string, s analyze.Step) {
			params := ""
			if len(s.Params) > 0 {
				params = dim + "  [" + strings.Join(s.Params, ", ") + "]" + reset
			}
			fmt.Fprintf(w, "  %-6s %stx%d%s  %s%s\n", label, dim, s.TxIndex, reset, oneLine(s.SQL), params)
		}
		step("read", f.Read)
		step("write", f.Write)
		if f.Gate.Seq != f.Write.Seq {
			step("held", f.Gate)
		}
		if f.Outcome != Confirmed {
			if f.Safe && f.Why != "" {
				fmt.Fprintf(w, "  %swhy: %s%s\n", dim, f.Why, reset)
			}
			continue
		}
		fmt.Fprintf(w, "  %sboth requests read, then both wrote: %d of 2 reached the write and were held there%s\n", dim, f.Arrived, reset)
		fmt.Fprintf(w, "  one after the other  %s\n", results(f.Serial))
		fmt.Fprintf(w, "  at the same time     %s\n", results(f.Outputs))
		fmt.Fprintf(w, "  database, serial vs concurrent:\n")
		for _, d := range f.Diff {
			fmt.Fprintf(w, "    %s%s%s  %s serially, %s concurrently\n", bold, d.Table, reset, rows(d.Count[0]), rows(d.Count[1]))
			for i, row := range d.OnlyA {
				if i == 4 {
					fmt.Fprintf(w, "      %s... %d more%s\n", dim, len(d.OnlyA)-4, reset)
					break
				}
				fmt.Fprintf(w, "      %s- %s%s\n", red, row, reset)
			}
			for i, row := range d.OnlyB {
				if i == 4 {
					fmt.Fprintf(w, "      %s... %d more%s\n", dim, len(d.OnlyB)-4, reset)
					break
				}
				fmt.Fprintf(w, "      %s+ %s%s\n", green, row, reset)
			}
		}
		for i, fix := range f.Fix {
			label := "fix"
			if i > 0 {
				label = "   "
			}
			fmt.Fprintf(w, "  %s%s%s  %s\n", yellow, label, reset, fix)
		}
	}
	if len(r.Unstable) > 0 {
		fmt.Fprintf(w, "\n%snot compared (they change between identical runs): %s%s\n", dim, strings.Join(r.Unstable, ", "), reset)
	}
	n := len(r.Confirmed())
	fmt.Fprintln(w)
	if n > 0 {
		fmt.Fprintf(w, "%s%s%d race(s) confirmed%s\n", bold, red, n, reset)
	} else {
		fmt.Fprintf(w, "%sno race reproduced%s\n", green, reset)
	}
}

func results(r [2]RunResult) string {
	var parts []string
	for i, x := range r {
		parts = append(parts, fmt.Sprintf("#%d exit %d %q", i+1, x.ExitCode, clip(oneLine(x.Output), 48)))
	}
	return strings.Join(parts, "   ")
}

func rows(n int) string { return plural(n, "row") }

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// JSON writes the report for machines.
func (r *Report) JSON(w io.Writer) error {
	type step struct {
		Tx        int      `json:"tx"`
		SQL       string   `json:"sql"`
		Params    []string `json:"params,omitempty"`
		Tag       string   `json:"tag,omitempty"`
		Error     string   `json:"error,omitempty"`
		Isolation string   `json:"isolation"`
	}
	type diff struct {
		Table          string   `json:"table"`
		Serial         int      `json:"serial_rows"`
		Concurrent     int      `json:"concurrent_rows"`
		OnlySerial     []string `json:"only_serial,omitempty"`
		OnlyConcurrent []string `json:"only_concurrent,omitempty"`
	}
	type finding struct {
		Outcome    Outcome      `json:"outcome"`
		Pattern    string       `json:"pattern"`
		Table      string       `json:"table"`
		Read       step         `json:"read"`
		Write      step         `json:"write"`
		Held       step         `json:"held_at"`
		Arrived    int          `json:"arrived"`
		Diff       []diff       `json:"diff,omitempty"`
		Fix        []string     `json:"fix,omitempty"`
		Why        string       `json:"why,omitempty"`
		Concurrent [2]RunResult `json:"concurrent"`
		Serial     [2]RunResult `json:"serial"`
	}
	conv := func(s analyze.Step) step {
		return step{Tx: s.TxIndex, SQL: s.SQL, Params: s.Params, Tag: s.Tag, Error: s.Err, Isolation: s.Isolation}
	}
	out := struct {
		Requests   []string  `json:"requests"`
		Statements []step    `json:"statements"`
		Findings   []finding `json:"findings"`
		Confirmed  int       `json:"confirmed"`
		Unstable   []string  `json:"unstable_tables,omitempty"`
	}{Requests: r.Requests, Confirmed: len(r.Confirmed()), Unstable: r.Unstable, Findings: []finding{}}
	for _, s := range r.Steps {
		out.Statements = append(out.Statements, conv(s))
	}
	for _, f := range r.Findings {
		fd := finding{Outcome: f.Outcome, Pattern: string(f.Pattern), Table: f.Write.Stmt.Target,
			Read: conv(f.Read), Write: conv(f.Write), Held: conv(f.Gate), Arrived: f.Arrived,
			Fix: f.Fix, Why: f.Why, Concurrent: f.Outputs, Serial: f.Serial}
		for _, d := range f.Diff {
			fd.Diff = append(fd.Diff, diff{Table: d.Table, Serial: d.Count[0], Concurrent: d.Count[1], OnlySerial: d.OnlyA, OnlyConcurrent: d.OnlyB})
		}
		out.Findings = append(out.Findings, fd)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
