// Command doubledip finds race conditions in the database code of web
// apps: it sends the same request twice at the same moment, holds both
// copies right before they write, and shows what the database looks like
// afterwards.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/AndreaAcanfora/doubledip/internal/hunt"
)

var version = "dev"

const usage = `doubledip: send the same request twice at the same moment and see what your database does.

Usage:
  doubledip [flags] REQUEST [OTHER_REQUEST]
  doubledip [flags] --config doubledip.yml

REQUEST is a shell command that sends one request to your app, e.g.
  'curl -fsS -X POST localhost:3000/coupons/WELCOME/redeem'
With OTHER_REQUEST, the two different requests are raced against each other.
Each command gets DOUBLEDIP_N=1 or 2 in its environment.

A config file lists several scenarios to check in one run; see
https://github.com/AndreaAcanfora/doubledip#config-file. Without arguments,
doubledip reads doubledip.yml if it exists. Flags override the file.

The app must talk to Postgres through doubledip's proxy. Start it with --app
and doubledip sets DATABASE_URL (and PGHOST/PGPORT) for it.

Use a development database: doubledip saves its contents first, empties
and refills every table between runs, and puts everything back at the end.
It refuses non-local databases unless you pass --allow-remote.

Flags:
`

func main() { os.Exit(run()) }

func run() int {
	fs := flag.NewFlagSet("doubledip", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, "\nExit status: 0 no race reproduced, 1 race confirmed, 2 error.\n")
	}
	configPath := fs.String("config", "", "YAML file with the app and the scenarios to check")
	only := fs.String("only", "", "run only the scenarios whose name contains this text")
	db := fs.String("db", os.Getenv("DOUBLEDIP_DB"), "postgres:// URL of the development database (or $DOUBLEDIP_DB)")
	listen := fs.String("listen", "127.0.0.1:6543", "address of the proxy the app connects to")
	app := fs.String("app", "", "shell command that starts the app; it gets DATABASE_URL pointing at the proxy")
	ready := fs.String("ready", "", "URL to poll until the app answers before starting")
	barrier := fs.Duration("barrier-timeout", 3*time.Second, "how long to hold the first request waiting for the second")
	maxC := fs.Int("max", 8, "maximum number of candidate races to test per scenario")
	ignore := fs.String("ignore", "", "comma-separated table.column list to leave out of comparisons")
	allowRemote := fs.Bool("allow-remote", false, "allow a database that is not on localhost (it must still be a disposable one)")
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	reportPath := fs.String("report", "", "also write the JSON report to this file")
	markdown := fs.String("markdown", "", "also write the report as Markdown to this file (e.g. $GITHUB_STEP_SUMMARY)")
	verbose := fs.Bool("v", false, "show the app's output and proxy details")
	showVersion := fs.Bool("version", false, "print the version")
	fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Println("doubledip", version)
		return 0
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg := hunt.Config{Listen: *listen, BarrierTimeout: *barrier, MaxCandidates: *maxC, Verbose: *verbose}
	var scenarios []hunt.Scenario
	if *configPath == "" && fs.NArg() == 0 {
		if _, err := os.Stat("doubledip.yml"); err == nil {
			*configPath = "doubledip.yml"
		}
	}
	if *configPath != "" {
		fc, err := loadConfig(*configPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "doubledip:", err)
			return 2
		}
		cfg.Upstream, cfg.App, cfg.Ready, cfg.AllowRemote = fc.DB, fc.App, fc.Ready, fc.AllowRemote
		if fc.Listen != "" && !set["listen"] {
			cfg.Listen = fc.Listen
		}
		if fc.BarrierTimeout != 0 && !set["barrier-timeout"] {
			cfg.BarrierTimeout = fc.BarrierTimeout
		}
		if fc.Max != 0 && !set["max"] {
			cfg.MaxCandidates = fc.Max
		}
		if fs.NArg() == 0 {
			for _, s := range fc.Scenarios {
				if *only == "" || strings.Contains(s.Name, *only) {
					scenarios = append(scenarios, s.scenario())
				}
			}
			if len(scenarios) == 0 {
				fmt.Fprintf(os.Stderr, "doubledip: no scenario matches %q\n", *only)
				return 2
			}
		}
	}
	if set["db"] || cfg.Upstream == "" {
		cfg.Upstream = *db
	}
	if set["app"] {
		cfg.App = *app
	}
	if set["ready"] {
		cfg.Ready = *ready
	}
	if set["allow-remote"] {
		cfg.AllowRemote = *allowRemote
	}
	if fs.NArg() > 0 {
		var ign []string
		if *ignore != "" {
			ign = strings.Split(*ignore, ",")
		}
		scenarios = []hunt.Scenario{{Requests: fs.Args(), Ignore: ign}}
	}
	if len(scenarios) == 0 || fs.NArg() > 2 || cfg.Upstream == "" {
		fs.Usage()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cfg.Log = os.Stderr
	if *verbose {
		cfg.AppLog = os.Stderr
	}
	sess, err := hunt.Open(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doubledip:", err)
		return 2
	}
	var reps []*hunt.Report
	color := isTerminal(os.Stdout) && !*jsonOut
	for _, sc := range scenarios {
		rep, err := sess.Hunt(ctx, sc)
		if rep == nil {
			rep = &hunt.Report{Name: sc.Name, Requests: sc.Requests}
		}
		if err != nil {
			if len(scenarios) == 1 {
				sess.Close()
				fmt.Fprintln(os.Stderr, "doubledip:", err)
				return 2
			}
			rep.Error = err.Error()
		}
		reps = append(reps, rep)
		if !*jsonOut {
			rep.Text(os.Stdout, color)
		}
		if ctx.Err() != nil {
			break
		}
	}
	sess.Close()

	if *jsonOut {
		hunt.WriteJSON(os.Stdout, version, reps)
	} else {
		hunt.Summary(os.Stdout, reps, color)
	}
	if *reportPath != "" {
		if f, err := os.Create(*reportPath); err != nil {
			fmt.Fprintln(os.Stderr, "doubledip:", err)
		} else {
			hunt.WriteJSON(f, version, reps)
			f.Close()
		}
	}
	if *markdown != "" {
		if err := writeMarkdown(*markdown, reps); err != nil {
			fmt.Fprintln(os.Stderr, "doubledip:", err)
		}
	}
	confirmed, failed := 0, 0
	for _, r := range reps {
		confirmed += len(r.Confirmed())
		if r.Error != "" {
			failed++
		}
	}
	switch {
	case confirmed > 0:
		return 1
	case failed > 0:
		return 2
	}
	return 0
}

func writeMarkdown(path string, reps []*hunt.Report) error {
	var w io.Writer = os.Stdout
	if path != "-" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	hunt.WriteMarkdown(w, reps)
	return nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}
