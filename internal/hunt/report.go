package hunt

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/AndreaAcanfora/doubledip/internal/analyze"
	"github.com/AndreaAcanfora/doubledip/internal/sqlinfo"
)

// Text writes the report for people.
func (r *Report) Text(w io.Writer, color bool) {
	red, green, yellow, dim, bold, reset := "", "", "", "", "", ""
	if color {
		red, green, yellow, dim, bold, reset = "\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[2m", "\x1b[1m", "\x1b[0m"
	}
	if r.Name != "" {
		fmt.Fprintf(w, "\n%s── %s ──%s\n", bold, r.Name, reset)
	}
	if r.Error != "" {
		fmt.Fprintf(w, "\n%s! could not run%s: %s\n", yellow, reset, r.Error)
		return
	}
	txs := map[int]bool{}
	for _, s := range r.Steps {
		txs[s.TxIndex] = true
	}
	fmt.Fprintf(w, "\n%srecorded%s %s in %s\n", dim, reset, plural(len(r.Steps), "statement"), plural(len(txs), "transaction"))
	if len(r.Findings) == 0 {
		fmt.Fprintf(w, "\n%s✓ nothing to race%s: no write follows a read of the same table.\n", green, reset)
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
}

// Summary writes the closing line for a set of reports.
func Summary(w io.Writer, reps []*Report, color bool) {
	red, green, yellow, bold, reset := "", "", "", "", ""
	if color {
		red, green, yellow, bold, reset = "\x1b[31m", "\x1b[32m", "\x1b[33m", "\x1b[1m", "\x1b[0m"
	}
	n, failed := 0, 0
	for _, r := range reps {
		n += len(r.Confirmed())
		if r.Error != "" {
			failed++
		}
	}
	fmt.Fprintln(w)
	if n > 0 {
		fmt.Fprintf(w, "%s%s%s confirmed%s", bold, red, plural(n, "race"), reset)
	} else {
		fmt.Fprintf(w, "%sno race reproduced%s", green, reset)
	}
	if len(reps) > 1 {
		fmt.Fprintf(w, " in %s", plural(len(reps), "scenario"))
	}
	if failed > 0 {
		fmt.Fprintf(w, ", %s%d could not run%s", yellow, failed, reset)
	}
	fmt.Fprintln(w)
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

// JSON types. They are the stable, documented output of --json.

type jsonStep struct {
	Tx        int      `json:"tx"`
	SQL       string   `json:"sql"`
	Params    []string `json:"params,omitempty"`
	Tag       string   `json:"tag,omitempty"`
	Error     string   `json:"error,omitempty"`
	Isolation string   `json:"isolation"`
}

type jsonTrace struct {
	Conn    int      `json:"conn"`
	Tx      int64    `json:"tx"`
	SQL     string   `json:"sql"`
	Params  []string `json:"params,omitempty"`
	Tag     string   `json:"tag,omitempty"`
	Error   string   `json:"error,omitempty"`
	StartMS float64  `json:"start_ms"`
	EndMS   float64  `json:"end_ms"`
	Held    bool     `json:"held,omitempty"`
}

type jsonDiff struct {
	Table          string   `json:"table"`
	Serial         int      `json:"serial_rows"`
	Concurrent     int      `json:"concurrent_rows"`
	OnlySerial     []string `json:"only_serial,omitempty"`
	OnlyConcurrent []string `json:"only_concurrent,omitempty"`
}

type jsonFinding struct {
	Outcome    Outcome      `json:"outcome"`
	Pattern    string       `json:"pattern"`
	Table      string       `json:"table"`
	Read       jsonStep     `json:"read"`
	Write      jsonStep     `json:"write"`
	Held       jsonStep     `json:"held_at"`
	Arrived    int          `json:"arrived"`
	Diff       []jsonDiff   `json:"diff,omitempty"`
	Fix        []string     `json:"fix,omitempty"`
	Why        string       `json:"why,omitempty"`
	Serial     [2]RunResult `json:"serial"`
	Concurrent [2]RunResult `json:"concurrent"`
	Trace      []jsonTrace  `json:"trace"`
}

type jsonReport struct {
	Name       string        `json:"name,omitempty"`
	Requests   []string      `json:"requests"`
	Error      string        `json:"error,omitempty"`
	Statements []jsonStep    `json:"statements"`
	Findings   []jsonFinding `json:"findings"`
	Confirmed  int           `json:"confirmed"`
	Unstable   []string      `json:"unstable_tables,omitempty"`
}

func (r *Report) toJSON() jsonReport {
	conv := func(s analyze.Step) jsonStep {
		return jsonStep{Tx: s.TxIndex, SQL: s.SQL, Params: s.Params, Tag: s.Tag, Error: s.Err, Isolation: s.Isolation}
	}
	out := jsonReport{Name: r.Name, Requests: r.Requests, Error: r.Error, Confirmed: len(r.Confirmed()),
		Unstable: r.Unstable, Statements: []jsonStep{}, Findings: []jsonFinding{}}
	for _, s := range r.Steps {
		out.Statements = append(out.Statements, conv(s))
	}
	for _, f := range r.Findings {
		fd := jsonFinding{Outcome: f.Outcome, Pattern: string(f.Pattern), Table: f.Write.Stmt.Target,
			Read: conv(f.Read), Write: conv(f.Write), Held: conv(f.Gate), Arrived: f.Arrived,
			Fix: f.Fix, Why: f.Why, Concurrent: f.Outputs, Serial: f.Serial, Trace: []jsonTrace{}}
		for _, d := range f.Diff {
			fd.Diff = append(fd.Diff, jsonDiff{Table: d.Table, Serial: d.Count[0], Concurrent: d.Count[1], OnlySerial: d.OnlyA, OnlyConcurrent: d.OnlyB})
		}
		for _, e := range f.Trace {
			fp, _ := sqlinfo.Fingerprint(e.SQL)
			fd.Trace = append(fd.Trace, jsonTrace{Conn: e.Conn, Tx: e.Tx, SQL: e.SQL, Params: e.Params, Tag: e.Tag, Error: e.Err,
				StartMS: ms(e.Start.Sub(f.Started).Seconds()), EndMS: ms(e.At.Sub(f.Started).Seconds()), Held: fp == f.GateFP})
		}
		out.Findings = append(out.Findings, fd)
	}
	return out
}

func ms(sec float64) float64 { return float64(int64(sec*1e4)) / 10 }

// WriteJSON writes the reports for machines.
func WriteJSON(w io.Writer, version string, reps []*Report) error {
	out := struct {
		Version   string       `json:"doubledip"`
		Confirmed int          `json:"confirmed"`
		Scenarios []jsonReport `json:"scenarios"`
	}{Version: version, Scenarios: []jsonReport{}}
	for _, r := range reps {
		j := r.toJSON()
		out.Confirmed += j.Confirmed
		out.Scenarios = append(out.Scenarios, j)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// WriteMarkdown writes the reports as GitHub-flavoured Markdown, e.g. for
// a job summary.
func WriteMarkdown(w io.Writer, reps []*Report) {
	n := 0
	for _, r := range reps {
		n += len(r.Confirmed())
	}
	if n > 0 {
		fmt.Fprintf(w, "## doubledip: %s confirmed\n\n", plural(n, "race"))
	} else {
		fmt.Fprintf(w, "## doubledip: no race reproduced\n\n")
	}
	fmt.Fprintln(w, "| scenario | result | pattern | table |")
	fmt.Fprintln(w, "|---|---|---|---|")
	for i, r := range reps {
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("scenario %d", i+1)
		}
		switch {
		case r.Error != "":
			fmt.Fprintf(w, "| %s | ⚠️ could not run | | |\n", mdCell(name))
		case len(r.Findings) == 0:
			fmt.Fprintf(w, "| %s | ✅ nothing to race | | |\n", mdCell(name))
		}
		for _, f := range r.Findings {
			icon := map[Outcome]string{Confirmed: "❌ race confirmed", Held: "✅ safe", NotReached: "✅ serialized", Inconclusive: "⚠️ inconclusive"}[f.Outcome]
			fmt.Fprintf(w, "| %s | %s | %s | `%s` |\n", mdCell(name), icon, f.Pattern, f.Write.Stmt.Target)
		}
	}
	for i, r := range reps {
		for _, f := range r.Confirmed() {
			name := r.Name
			if name == "" {
				name = fmt.Sprintf("scenario %d", i+1)
			}
			fmt.Fprintf(w, "\n### ❌ %s: %s on `%s`\n\n", mdCell(name), f.Pattern, f.Write.Stmt.Target)
			fmt.Fprintf(w, "```sql\n-- read (tx%d)\n%s\n-- write (tx%d)\n%s\n```\n\n", f.Read.TxIndex, oneLine(f.Read.SQL), f.Write.TxIndex, oneLine(f.Write.SQL))
			fmt.Fprintf(w, "| | request 1 | request 2 |\n|---|---|---|\n")
			fmt.Fprintf(w, "| one after the other | %s | %s |\n", mdResult(f.Serial[0]), mdResult(f.Serial[1]))
			fmt.Fprintf(w, "| at the same time | %s | %s |\n\n", mdResult(f.Outputs[0]), mdResult(f.Outputs[1]))
			fmt.Fprintln(w, "```diff")
			for _, d := range f.Diff {
				fmt.Fprintf(w, "# %s: %s serially, %s concurrently\n", d.Table, rows(d.Count[0]), rows(d.Count[1]))
				for _, row := range d.OnlyA {
					fmt.Fprintf(w, "- %s\n", row)
				}
				for _, row := range d.OnlyB {
					fmt.Fprintf(w, "+ %s\n", row)
				}
			}
			fmt.Fprintln(w, "```")
			fmt.Fprintln(w)
			for _, fix := range f.Fix {
				fmt.Fprintf(w, "- %s\n", fix)
			}
		}
		if r.Error != "" {
			fmt.Fprintf(w, "\n**%s could not run:** %s\n", mdCell(r.Name), mdCell(r.Error))
		}
	}
}

func mdCell(s string) string {
	return strings.NewReplacer("|", `\|`, "\n", " ").Replace(s)
}

func mdResult(r RunResult) string {
	return fmt.Sprintf("exit %d `%s`", r.ExitCode, strings.ReplaceAll(clip(oneLine(r.Output), 40), "`", "'"))
}
