// Command doubledip finds race conditions in the database code of web
// apps: it sends the same request twice at the same moment, holds both
// copies right before they write, and shows what the database looks like
// afterwards.
package main

import (
	"context"
	"flag"
	"fmt"
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

REQUEST is a shell command that sends one request to your app, e.g.
  'curl -fsS -X POST localhost:3000/coupons/WELCOME/redeem'
With OTHER_REQUEST, the two different requests are raced against each other.
Each command gets DOUBLEDIP_N=1 or 2 in its environment.

The app must talk to Postgres through doubledip's proxy. Start it with --app
and doubledip sets DATABASE_URL (and PGHOST/PGPORT) for it.

Use a development database: doubledip saves its contents first, empties
and refills every table between runs, and puts everything back at the end.
It refuses non-local databases unless you pass --allow-remote.

Flags:
`

func main() {
	fs := flag.NewFlagSet("doubledip", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, "\nExit status: 0 no race reproduced, 1 race confirmed, 2 error.\n")
	}
	db := fs.String("db", os.Getenv("DOUBLEDIP_DB"), "postgres:// URL of the development database (or $DOUBLEDIP_DB)")
	listen := fs.String("listen", "127.0.0.1:6543", "address of the proxy the app connects to")
	app := fs.String("app", "", "shell command that starts the app; it gets DATABASE_URL pointing at the proxy")
	ready := fs.String("ready", "", "URL to poll until the app answers before starting")
	barrier := fs.Duration("barrier-timeout", 3*time.Second, "how long to hold the first request waiting for the second")
	maxC := fs.Int("max", 8, "maximum number of candidate races to test")
	ignore := fs.String("ignore", "", "comma-separated table.column list to leave out of comparisons")
	allowRemote := fs.Bool("allow-remote", false, "allow a database that is not on localhost (it must still be a disposable one)")
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	verbose := fs.Bool("v", false, "show the app's output and proxy details")
	showVersion := fs.Bool("version", false, "print the version")
	fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Println("doubledip", version)
		return
	}
	if fs.NArg() < 1 || fs.NArg() > 2 || *db == "" {
		fs.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	logw := os.Stderr
	cfg := hunt.Config{
		Upstream: *db, Listen: *listen, App: *app, Ready: *ready,
		Requests: fs.Args(), BarrierTimeout: *barrier, MaxCandidates: *maxC,
		Verbose: *verbose, AllowRemote: *allowRemote,
	}
	if *ignore != "" {
		cfg.Ignore = strings.Split(*ignore, ",")
	}
	cfg.Log = logw
	if *verbose {
		cfg.AppLog = logw
	}
	rep, err := hunt.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doubledip:", err)
		os.Exit(2)
	}
	if *jsonOut {
		rep.JSON(os.Stdout)
	} else {
		rep.Text(os.Stdout, isTerminal(os.Stdout))
	}
	if len(rep.Confirmed()) > 0 {
		os.Exit(1)
	}
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == ""
}
