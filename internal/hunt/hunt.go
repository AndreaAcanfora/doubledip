// Package hunt runs the whole doubledip procedure: record one request,
// find the reads it acts on, then send the request twice at the same time
// with both copies held right before their first write, and check whether
// the database ends up in a state that running them one after the other
// never produces.
package hunt

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/AndreaAcanfora/doubledip/internal/analyze"
	"github.com/AndreaAcanfora/doubledip/internal/dbstate"
	"github.com/AndreaAcanfora/doubledip/internal/proxy"
)

// Config holds the options of a hunt.
type Config struct {
	Upstream       string // postgres:// URL of the development database
	Listen         string // proxy listen address
	App            string // shell command that starts the app (optional)
	Ready          string // URL that answers once the app is up (optional)
	ReadyTimeout   time.Duration
	Requests       []string // one or two shell commands, each sending one request
	BarrierTimeout time.Duration
	MaxCandidates  int
	Ignore         []string  // table.column to leave out of comparisons
	AllowRemote    bool      // allow a database that is not on this machine
	Log            io.Writer // progress lines
	AppLog         io.Writer // the app's own output
	Verbose        bool
}

// Outcome of one candidate.
type Outcome string

const (
	Confirmed    Outcome = "confirmed"    // the concurrent run left a state no serial order leaves
	Held         Outcome = "safe"         // both requests were held at the write, result matched a serial run
	NotReached   Outcome = "serialized"   // the second request never reached the write in time
	Inconclusive Outcome = "inconclusive" // something failed while testing
)

// Finding is a candidate with the result of testing it.
type Finding struct {
	analyze.Candidate
	Outcome Outcome
	Arrived int
	Diff    []dbstate.TableDiff // concurrent state vs the closest serial outcome
	Outputs [2]RunResult        // what the two concurrent requests printed
	Serial  [2]RunResult        // what the two serial requests printed
	Note    string
}

// RunResult is the result of one request command.
type RunResult struct {
	ExitCode int
	Output   string
}

// Report is the result of a hunt.
type Report struct {
	Requests []string
	Steps    []analyze.Step
	Findings []Finding
	Unstable []string // tables that differ between two identical serial runs
	ProxyURL string
}

// Confirmed returns the findings that reproduced.
func (r *Report) Confirmed() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Outcome == Confirmed {
			out = append(out, f)
		}
	}
	return out
}

// Run performs the hunt.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.AppLog == nil {
		cfg.AppLog = io.Discard
	}
	if len(cfg.Requests) == 0 || len(cfg.Requests) > 2 {
		return nil, errors.New("give one request command, or two to race against each other")
	}
	if cfg.BarrierTimeout == 0 {
		cfg.BarrierTimeout = 3 * time.Second
	}
	if cfg.MaxCandidates == 0 {
		cfg.MaxCandidates = 8
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 60 * time.Second
	}
	logf := func(format string, a ...any) { fmt.Fprintf(cfg.Log, format+"\n", a...) }

	up, err := url.Parse(cfg.Upstream)
	if err != nil || (up.Scheme != "postgres" && up.Scheme != "postgresql") {
		return nil, fmt.Errorf("--db must be a postgres:// URL")
	}
	host, port := up.Hostname(), up.Port()
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "5432"
	}

	if !cfg.AllowRemote && !isLocal(host) {
		return nil, fmt.Errorf("%s is not a local database. doubledip empties and refills every table between runs, so it only runs against a development database; pass --allow-remote if %s is one", host, host)
	}

	db, err := dbstate.Open(ctx, cfg.Upstream)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", redact(up), err)
	}
	defer db.Close()

	p := &proxy.Proxy{Upstream: net.JoinHostPort(host, port)}
	if cfg.Verbose {
		p.Logf = logf
	}
	addr, err := p.Listen(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	defer p.Close()
	pu := *up
	pu.Host = addr
	rep := &Report{Requests: cfg.Requests, ProxyURL: redact(&pu)}
	logf("proxy  %s -> %s", addr, p.Upstream)

	if cfg.App != "" && cfg.Ready != "" {
		// Refuse to send requests to whatever already answers there.
		if ru, err := url.Parse(cfg.Ready); err == nil {
			hp := ru.Host
			if ru.Port() == "" {
				hp = net.JoinHostPort(ru.Hostname(), map[string]string{"https": "443"}[ru.Scheme]+map[bool]string{true: "80"}[ru.Scheme != "https"])
			}
			if c, err := net.DialTimeout("tcp", hp, time.Second); err == nil {
				c.Close()
				return nil, fmt.Errorf("something is already listening on %s before the app started: stop it or use another port", hp)
			}
		}
	}
	if cfg.App != "" {
		app, err := startApp(cfg.App, pu.String(), addr, cfg.AppLog)
		if err != nil {
			return nil, err
		}
		defer app.stop()
		logf("app    %s", cfg.App)
	}
	if cfg.Ready != "" {
		if err := waitReady(ctx, cfg.Ready, cfg.ReadyTimeout); err != nil {
			return nil, err
		}
	} else if cfg.App != "" {
		time.Sleep(2 * time.Second)
	}

	base, err := db.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("save database state: %w", err)
	}
	defer func() {
		if err := db.Restore(context.Background(), base); err != nil {
			logf("warning: could not restore the database: %v", err)
		}
	}()
	ignore := map[string]bool{}
	for _, c := range cfg.Ignore {
		ignore[c] = true
	}
	capture := func() (dbstate.State, error) { return db.Capture(ctx, base, ignore) }
	reqB := cfg.Requests[len(cfg.Requests)-1]
	reqA := cfg.Requests[0]

	// 1. Record one run of the request.
	p.StartRecording()
	rec := runCmd(ctx, reqA, "record", 1)
	execs := p.StopRecording()
	if cfg.Verbose {
		logf("record exit=%d %s", rec.ExitCode, oneLine(rec.Output))
	}
	if len(cfg.Requests) == 2 {
		if err := db.Restore(ctx, base); err != nil {
			return nil, err
		}
		p.StartRecording()
		runCmd(ctx, reqB, "record", 2)
		execs = append(execs, p.StopRecording()...)
	}
	if len(execs) == 0 {
		return rep, fmt.Errorf("the request ran no SQL through the proxy: is the app connecting to %s? (exit %d: %s)", rep.ProxyURL, rec.ExitCode, oneLine(rec.Output))
	}
	rep.Steps = analyze.Steps(execs, db.DefaultIsolation(ctx))
	cands := analyze.Find(rep.Steps)
	logf("record %d statements, %d candidate race(s)", len(execs), len(cands))
	if len(cands) == 0 {
		return rep, nil
	}

	// 2. Serial baselines. Request i always runs with DOUBLEDIP_N=i, so
	// 1-then-2 and 2-then-1 can differ even for the same command: both are
	// correct outcomes. A second 1-then-2 run finds tables whose contents
	// vary on their own.
	reqs := [2]string{reqA, reqB}
	serial := func(first, second int) (dbstate.State, [2]RunResult, error) {
		if err := db.Restore(ctx, base); err != nil {
			return nil, [2]RunResult{}, err
		}
		var out [2]RunResult
		out[first] = runCmd(ctx, reqs[first], "serial", first+1)
		out[second] = runCmd(ctx, reqs[second], "serial", second+1)
		st, err := capture()
		return st, out, err
	}
	s1, serialOut, err := serial(0, 1)
	if err != nil {
		return nil, err
	}
	s1b, _, err := serial(0, 1)
	if err != nil {
		return nil, err
	}
	unstable := map[string]bool{}
	for _, d := range dbstate.Diff(s1, s1b, nil) {
		unstable[d.Table] = true
		rep.Unstable = append(rep.Unstable, d.Table)
	}
	s2, _, err := serial(1, 0)
	if err != nil {
		return nil, err
	}
	states := []dbstate.State{s1, s2}

	// 3. Race each candidate.
	for i, c := range cands {
		if i >= cfg.MaxCandidates {
			break
		}
		f := Finding{Candidate: c}
		if err := db.Restore(ctx, base); err != nil {
			return nil, err
		}
		g := proxy.NewGate(c.GateFP, 2, cfg.BarrierTimeout)
		p.SetGate(g)
		f.Outputs = concurrent(ctx, reqA, reqB)
		p.SetGate(nil)
		f.Serial = serialOut
		arrived, timedOut := g.Result()
		f.Arrived = arrived
		st, err := capture()
		if err != nil {
			f.Outcome, f.Note = Inconclusive, err.Error()
			rep.Findings = append(rep.Findings, f)
			continue
		}
		matches := false
		for _, s := range states {
			if len(dbstate.Diff(s, st, unstable)) == 0 {
				matches = true
				break
			}
		}
		switch {
		case !matches:
			f.Outcome = Confirmed
			// Show the difference from the closest serial outcome.
			for _, s := range states {
				d := dbstate.Diff(s, st, unstable)
				if f.Diff == nil || size(d) < size(f.Diff) {
					f.Diff = d
				}
			}
		case arrived >= 2 && !timedOut:
			f.Outcome = Held
		default:
			f.Outcome = NotReached
		}
		logf("race   #%d %s at %q: %s", i+1, c.Pattern, short(c.Gate.SQL), f.Outcome)
		rep.Findings = append(rep.Findings, f)
	}
	return rep, nil
}

func size(d []dbstate.TableDiff) int {
	n := 0
	for _, t := range d {
		n += len(t.OnlyA) + len(t.OnlyB)
	}
	return n
}

// concurrent starts both requests at the same moment and waits for both.
func concurrent(ctx context.Context, a, b string) [2]RunResult {
	var out [2]RunResult
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, cmd := range []string{a, b} {
		wg.Add(1)
		go func(i int, cmd string) {
			defer wg.Done()
			<-start
			out[i] = runCmd(ctx, cmd, "concurrent", i+1)
		}(i, cmd)
	}
	close(start)
	wg.Wait()
	return out
}

func runCmd(ctx context.Context, cmd, phase string, n int) RunResult {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Env = append(os.Environ(), "DOUBLEDIP_PHASE="+phase, fmt.Sprintf("DOUBLEDIP_N=%d", n))
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	code := 0
	if err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	out := buf.String()
	if len(out) > 2000 {
		out = out[:2000] + "..."
	}
	return RunResult{ExitCode: code, Output: out}
}

type app struct{ cmd *exec.Cmd }

func startApp(cmdline, dbURL, addr string, log io.Writer) (*app, error) {
	host, port, _ := net.SplitHostPort(addr)
	c := exec.Command("sh", "-c", cmdline)
	c.Env = append(os.Environ(), "DATABASE_URL="+dbURL, "DOUBLEDIP_DATABASE_URL="+dbURL, "PGHOST="+host, "PGPORT="+port)
	c.Stdout, c.Stderr = log, log
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("start app: %w", err)
	}
	return &app{cmd: c}, nil
}

func (a *app) stop() {
	if a.cmd.Process == nil {
		return
	}
	syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { a.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func waitReady(ctx context.Context, u string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the app did not answer at %s within %s", u, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func isLocal(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func redact(u *url.URL) string {
	c := *u
	if c.User != nil {
		c.User = url.User(c.User.Username())
	}
	return c.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

func short(s string) string { return oneLine(s) }
