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

// Config holds the options shared by every scenario of a session.
type Config struct {
	Upstream       string // postgres:// URL of the development database
	Listen         string // proxy listen address
	App            string // shell command that starts the app (optional)
	Ready          string // URL that answers once the app is up (optional)
	ReadyTimeout   time.Duration
	BarrierTimeout time.Duration
	MaxCandidates  int
	FreeRuns       int       // times a confirmed race is run again without the barrier; <0 skips it
	RestartApp     bool      // restart the app after every database restore
	AllowRemote    bool      // allow a database that is not on this machine
	Log            io.Writer // progress lines
	AppLog         io.Writer // the app's own output
	Verbose        bool

	// Requests and Ignore describe a single scenario for Run.
	Requests []string
	Ignore   []string
}

// Scenario is one request (or pair of requests) to race.
type Scenario struct {
	Name     string
	Requests []string // one or two shell commands, each sending one request
	Ignore   []string // table.column to leave out of comparisons
}

// Outcome of one candidate.
type Outcome string

const (
	Confirmed    Outcome = "confirmed"    // the concurrent run left a state no serial order leaves
	Held         Outcome = "safe"         // both requests were held at the write, result matched a serial run
	NotReached   Outcome = "serialized"   // the second request never reached the write in time
	Inconclusive Outcome = "inconclusive" // something failed while testing
	Deadlock     Outcome = "deadlock"     // reproduced only while held; run freely, the database aborts one request
)

// Finding is a candidate with the result of testing it.
type Finding struct {
	analyze.Candidate
	Outcome Outcome
	Arrived int
	Diff    []dbstate.TableDiff // concurrent state vs the closest serial outcome
	Outputs [2]RunResult        // what the two concurrent requests printed
	Serial  [2]RunResult        // what the two serial requests printed
	Trace   []proxy.Exec        // every statement of the concurrent run
	Started time.Time           // when the concurrent run started
	Note    string
	// Blocked is set when the second request waited on a lock held by the
	// first, so the barrier opened without waiting for the timeout.
	Blocked bool
	// FreeRuns is how many times a confirmed race was run again without the
	// barrier; FreeHits how many of those runs reproduced it and
	// FreeDeadlocks how many ended with a deadlock (40P01).
	FreeRuns, FreeHits, FreeDeadlocks int
	// SameAs is the 1-based index of an earlier finding that left exactly
	// the same database state: the same race seen from another statement.
	SameAs int
}

// RunResult is the result of one request command.
type RunResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

// Report is the result of one scenario.
type Report struct {
	Name     string
	Requests []string
	Steps    []analyze.Step
	Findings []Finding
	Unstable []string // tables that differ between two identical serial runs
	Untested int      // candidates left out by MaxCandidates
	Error    string   // set when the scenario could not run
}

// Confirmed returns the races that reproduced, each once: a finding with
// the same result as an earlier one is left out.
func (r *Report) Confirmed() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Outcome == Confirmed && f.SameAs == 0 {
			out = append(out, f)
		}
	}
	return out
}

// Session is a running proxy (and app) with the saved database contents.
type Session struct {
	cfg      Config
	db       *dbstate.DB
	p        *proxy.Proxy
	app      *app
	base     *dbstate.Snapshot
	ProxyURL string // redacted
	logf     func(string, ...any)
	appURL   string // DATABASE_URL given to the app
	appAddr  string
}

// Run performs a single-scenario hunt.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	s, err := Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Hunt(ctx, Scenario{Requests: cfg.Requests, Ignore: cfg.Ignore})
}

// Open starts the proxy and the app and saves the database contents.
func Open(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if cfg.AppLog == nil {
		cfg.AppLog = io.Discard
	}
	if cfg.BarrierTimeout == 0 {
		cfg.BarrierTimeout = 3 * time.Second
	}
	if cfg.MaxCandidates == 0 {
		cfg.MaxCandidates = 8
	}
	if cfg.FreeRuns == 0 {
		cfg.FreeRuns = 3
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 60 * time.Second
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:6543"
	}
	s := &Session{cfg: cfg}
	s.logf = func(format string, a ...any) { fmt.Fprintf(cfg.Log, format+"\n", a...) }

	up, err := url.Parse(cfg.Upstream)
	if err != nil || (up.Scheme != "postgres" && up.Scheme != "postgresql") {
		return nil, fmt.Errorf("the database must be a postgres:// URL")
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

	if s.db, err = dbstate.Open(ctx, cfg.Upstream); err != nil {
		return nil, fmt.Errorf("connect to %s: %w", redact(up), err)
	}
	s.p = &proxy.Proxy{Upstream: net.JoinHostPort(host, port)}
	if cfg.Verbose {
		s.p.Logf = s.logf
	}
	addr, err := s.p.Listen(cfg.Listen)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("proxy: %w", err)
	}
	pu := *up
	pu.Host = addr
	s.ProxyURL = redact(&pu)
	s.logf("proxy  %s -> %s", addr, s.p.Upstream)

	if cfg.App != "" && cfg.Ready != "" {
		// Refuse to send requests to whatever already answers there.
		if hp := hostPort(cfg.Ready); hp != "" {
			if c, err := net.DialTimeout("tcp", hp, time.Second); err == nil {
				c.Close()
				s.Close()
				return nil, fmt.Errorf("something is already listening on %s before the app started: stop it or use another port", hp)
			}
		}
	}
	s.appURL, s.appAddr = pu.String(), addr
	if cfg.App != "" {
		if err := s.startApp(ctx); err != nil {
			s.Close()
			return nil, err
		}
		s.logf("app    %s", cfg.App)
	}

	if s.base, err = s.db.Save(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("save database state: %w", err)
	}
	return s, nil
}

// startApp starts the app and waits until it is ready.
func (s *Session) startApp(ctx context.Context) error {
	var err error
	if s.app, err = startApp(s.cfg.App, s.appURL, s.appAddr, s.cfg.AppLog); err != nil {
		return err
	}
	if s.cfg.Ready != "" {
		return waitReady(ctx, s.cfg.Ready, s.cfg.ReadyTimeout, s.app)
	}
	time.Sleep(2 * time.Second)
	return s.app.exited()
}

// reset puts the database back to its saved contents and, with
// RestartApp, restarts the app so that nothing it kept in memory from the
// previous run (a cache, a counter) changes the next one.
func (s *Session) reset(ctx context.Context) error {
	if err := s.db.Restore(ctx, s.base); err != nil {
		return err
	}
	if s.cfg.RestartApp && s.app != nil {
		s.app.stop()
		if err := s.startApp(ctx); err != nil {
			return fmt.Errorf("restart app: %w", err)
		}
	}
	return nil
}

// Close restores the database and stops the app and the proxy.
func (s *Session) Close() {
	if s.base != nil {
		if err := s.db.Restore(context.Background(), s.base); err != nil {
			s.logf("warning: could not restore the database: %v", err)
		}
	}
	if s.app != nil {
		s.app.stop()
	}
	if s.p != nil {
		s.p.Close()
	}
	if s.db != nil {
		s.db.Close()
	}
}

// Hunt races one scenario. The database is back to its saved contents
// when it returns.
func (s *Session) Hunt(ctx context.Context, sc Scenario) (*Report, error) {
	if len(sc.Requests) == 0 || len(sc.Requests) > 2 {
		return nil, errors.New("give one request command, or two to race against each other")
	}
	db, p, base, logf := s.db, s.p, s.base, s.logf
	reset := func() error { return s.reset(ctx) }
	defer db.Restore(context.Background(), base)
	rep := &Report{Name: sc.Name, Requests: sc.Requests}
	ignore := map[string]bool{}
	for _, c := range sc.Ignore {
		ignore[strings.TrimSpace(c)] = true
	}
	capture := func() (dbstate.State, error) { return db.Capture(ctx, base, ignore) }
	reqA := sc.Requests[0]
	reqB := sc.Requests[len(sc.Requests)-1]
	if sc.Name != "" {
		logf("scenario %s", sc.Name)
	}

	// 1. Record one run of the request.
	if err := reset(); err != nil {
		return nil, err
	}
	p.StartRecording()
	rec := runCmd(ctx, reqA, "record", 1)
	execs := p.StopRecording()
	if s.cfg.Verbose {
		logf("record exit=%d %s", rec.ExitCode, oneLine(rec.Output))
	}
	if len(sc.Requests) == 2 {
		if err := reset(); err != nil {
			return nil, err
		}
		p.StartRecording()
		runCmd(ctx, reqB, "record", 2)
		execs = append(execs, p.StopRecording()...)
	}
	if len(execs) == 0 {
		return rep, fmt.Errorf("the request ran no SQL through the proxy: is the app connecting to %s? (exit %d: %s)", s.ProxyURL, rec.ExitCode, oneLine(rec.Output))
	}
	rep.Steps = analyze.Steps(execs, db.DefaultIsolation(ctx))
	cands := analyze.Find(rep.Steps)
	logf("record %d statements, %d candidate race(s)", len(execs), len(cands))
	if s.cfg.Verbose {
		for i, c := range cands {
			logf("cand   #%d %s on %s, read %s, held at %q", i+1, c.Pattern, c.Write.Stmt.Target, strings.Join(c.Read.Stmt.Reads, ","), oneLine(c.Gate.SQL))
		}
	}
	if len(cands) == 0 {
		return rep, nil
	}

	// 2. Serial baselines. Request i always runs with DOUBLEDIP_N=i, so
	// 1-then-2 and 2-then-1 can differ even for the same command: both are
	// correct outcomes. A second 1-then-2 run finds tables whose contents
	// vary on their own.
	reqs := [2]string{reqA, reqB}
	serial := func(first, second int) (dbstate.State, [2]RunResult, error) {
		if err := reset(); err != nil {
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
	serialState := func(st dbstate.State) bool {
		for _, s := range states {
			if len(dbstate.Diff(s, st, unstable)) == 0 {
				return true
			}
		}
		return false
	}

	// 3. Race each candidate.
	if n := len(cands) - s.cfg.MaxCandidates; n > 0 {
		rep.Untested = n
		cands = cands[:s.cfg.MaxCandidates]
		logf("max    testing %d of %d candidates; raise --max to test the other %d", len(cands), len(cands)+n, n)
	}
	seen := map[string]int{}
	for i, c := range cands {
		f := Finding{Candidate: c, Serial: serialOut}
		if err := reset(); err != nil {
			return nil, err
		}
		g := proxy.NewGate(c.GateFP, 2, s.cfg.BarrierTimeout)
		p.SetGate(g)
		p.StartRecording()
		f.Started = time.Now()
		stop := s.openOnLock(ctx, g, &f.Blocked)
		f.Outputs = concurrent(ctx, reqA, reqB)
		stop()
		f.Trace = p.StopRecording()
		p.SetGate(nil)
		arrived, timedOut := g.Result()
		f.Arrived = arrived
		st, err := capture()
		if err != nil {
			f.Outcome, f.Note = Inconclusive, err.Error()
			rep.Findings = append(rep.Findings, f)
			continue
		}
		switch {
		case !serialState(st):
			f.Outcome = Confirmed
			// Show the difference from the closest serial outcome.
			for _, s := range states {
				d := dbstate.Diff(s, st, unstable)
				if f.Diff == nil || size(d) < size(f.Diff) {
					f.Diff = d
				}
			}
			key := diffKey(f.Diff)
			if k, ok := seen[key]; ok {
				f.SameAs, f.Outcome = k, rep.Findings[k-1].Outcome
				break
			}
			seen[key] = len(rep.Findings) + 1
			if err := s.runFree(ctx, &f, reqA, reqB, capture, serialState); err != nil {
				return nil, err
			}
		case arrived >= 2 && !timedOut:
			f.Outcome = Held
		default:
			f.Outcome = NotReached
		}
		logf("race   #%d %s at %q: %s", i+1, c.Pattern, oneLine(c.Gate.SQL), f.Outcome)
		rep.Findings = append(rep.Findings, f)
	}
	return rep, nil
}

// openOnLock opens the barrier as soon as the request it holds keeps the
// other one waiting on a lock: the other can then never reach the barrier,
// and waiting for the timeout only slows the run down. The returned
// function stops watching.
func (s *Session) openOnLock(ctx context.Context, g *proxy.Gate, blocked *bool) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			if g.Waiting() != 1 {
				continue
			}
			if n, err := s.db.LockWaiters(ctx); err == nil && n > 0 {
				*blocked = true
				g.Release()
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}

// runFree runs a confirmed race again without the barrier, the way real
// clients would send it. When every one of those runs ends with the
// database aborting a request on a deadlock, the race is held back from
// "confirmed": the barrier forced an order real traffic cannot reach.
func (s *Session) runFree(ctx context.Context, f *Finding, reqA, reqB string, capture func() (dbstate.State, error), serial func(dbstate.State) bool) error {
	for f.FreeRuns < s.cfg.FreeRuns && f.FreeHits == 0 {
		if err := s.reset(ctx); err != nil {
			return err
		}
		s.p.StartRecording()
		concurrent(ctx, reqA, reqB)
		trace := s.p.StopRecording()
		f.FreeRuns++
		deadlock := false
		for _, e := range trace {
			deadlock = deadlock || strings.HasPrefix(e.Err, "40P01")
		}
		// A run where the database aborted one request did not let both
		// through; whatever state it leaves is not the race.
		if deadlock {
			f.FreeDeadlocks++
			continue
		}
		st, err := capture()
		if err != nil {
			return err
		}
		if !serial(st) {
			f.FreeHits++
		}
	}
	if f.FreeHits == 0 && f.FreeDeadlocks == f.FreeRuns && f.FreeRuns > 0 {
		f.Outcome = Deadlock
	}
	return nil
}

// diffKey identifies a database result, so that findings that end in the
// same state are reported as one race.
func diffKey(d []dbstate.TableDiff) string {
	var b strings.Builder
	for _, t := range d {
		fmt.Fprintf(&b, "%s\x00%q\x00%q\x01", t.Table, t.OnlyA, t.OnlyB)
	}
	return b.String()
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

type app struct {
	cmd  *exec.Cmd
	done chan struct{}
	tail *tailBuffer
}

func startApp(cmdline, dbURL, addr string, log io.Writer) (*app, error) {
	host, port, _ := net.SplitHostPort(addr)
	c := exec.Command("sh", "-c", cmdline)
	c.Env = append(os.Environ(), "DATABASE_URL="+dbURL, "DOUBLEDIP_DATABASE_URL="+dbURL, "PGHOST="+host, "PGPORT="+port)
	tail := &tailBuffer{max: 2000}
	c.Stdout = io.MultiWriter(log, tail)
	c.Stderr = c.Stdout
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("start app: %w", err)
	}
	a := &app{cmd: c, done: make(chan struct{}), tail: tail}
	go func() { c.Wait(); close(a.done) }()
	return a, nil
}

func (a *app) stop() {
	if a.cmd.Process == nil {
		return
	}
	syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-a.done:
	case <-time.After(5 * time.Second):
		syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
		<-a.done
	}
}

// exited returns an error describing the app's exit if it has stopped.
func (a *app) exited() error {
	if a == nil {
		return nil
	}
	select {
	case <-a.done:
		return fmt.Errorf("the app exited (%s) before it was ready. Its last output:\n%s", a.cmd.ProcessState, strings.TrimSpace(a.tail.String()))
	default:
		return nil
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

func waitReady(ctx context.Context, u string, timeout time.Duration, a *app) error {
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
		if err := a.exited(); err != nil {
			return err
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

func hostPort(u string) string {
	ru, err := url.Parse(u)
	if err != nil || ru.Host == "" {
		return ""
	}
	if ru.Port() != "" {
		return ru.Host
	}
	if ru.Scheme == "https" {
		return net.JoinHostPort(ru.Hostname(), "443")
	}
	return net.JoinHostPort(ru.Hostname(), "80")
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
