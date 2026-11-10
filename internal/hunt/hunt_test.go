package hunt

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The end-to-end tests run the example apps against a real Postgres.
// Set DOUBLEDIP_TEST_DB to a postgres:// URL of a superuser; each run uses
// a fresh database.
func testDB(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("DOUBLEDIP_TEST_DB")
	if admin == "" {
		t.Skip("DOUBLEDIP_TEST_DB not set")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	name := fmt.Sprintf("doubledip_test_%d", time.Now().UnixNano())
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	cfg, _ := pgx.ParseConfig(admin)
	return fmt.Sprintf("postgres://%s@%s/%s", userinfo(cfg), net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)), name)
}

func userinfo(cfg *pgx.ConnConfig) string {
	if cfg.Password != "" {
		return cfg.User + ":" + cfg.Password
	}
	return cfg.User
}

func freePort(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

type tcase struct {
	path string
	want Outcome // "" = no candidate at all
}

func hunt(t *testing.T, db, app, addr string, c tcase) *Report {
	t.Helper()
	t.Setenv("ADDR", addr)
	rep, err := Run(context.Background(), Config{
		Upstream: db, Listen: "127.0.0.1:0", App: app, Ready: "http://" + addr + "/health",
		Requests:       []string{fmt.Sprintf(`curl -sS -X POST "http://%s/%s"`, addr, c.path)},
		BarrierTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("%s: %v", c.path, err)
	}
	return rep
}

func check(t *testing.T, rep *Report, c tcase) {
	t.Helper()
	if c.want == "" {
		if len(rep.Findings) != 0 {
			t.Errorf("%s: want no finding, got %+v", c.path, rep.Findings)
		}
		return
	}
	if len(rep.Findings) != 1 {
		var b strings.Builder
		rep.Text(&b, false)
		t.Fatalf("%s: want 1 finding, got %d\n%s", c.path, len(rep.Findings), b.String())
	}
	if got := rep.Findings[0].Outcome; got != c.want {
		var b strings.Builder
		rep.Text(&b, false)
		t.Errorf("%s: want %s, got %s\n%s", c.path, c.want, got, b.String())
	}
}

// TestShopGo runs every endpoint of the Go example in one session, the
// way a config file does.
func TestShopGo(t *testing.T) {
	db := testDB(t)
	bin := filepath.Join(t.TempDir(), "shop")
	build := exec.Command("go", "build", "-o", bin, "./examples/shop")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	addr := freePort(t)
	t.Setenv("ADDR", addr)
	s, err := Open(context.Background(), Config{
		Upstream: db, Listen: "127.0.0.1:0", App: bin, Ready: "http://" + addr + "/health",
		BarrierTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range []tcase{
		{"redeem?code=WELCOME&user=u$DOUBLEDIP_N", Confirmed},
		{"redeem-safe?code=SAFE&user=u$DOUBLEDIP_N", NotReached},
		{"withdraw?account=1&amount=80", Confirmed},
		{"withdraw-safe?account=2&amount=80", Held},
		{"signup?email=a@example.com", Confirmed},
		{"signup-safe?email=a@example.com", Held},
	} {
		t.Run(strings.SplitN(c.path, "?", 2)[0], func(t *testing.T) {
			rep, err := s.Hunt(context.Background(), Scenario{Name: c.path,
				Requests: []string{fmt.Sprintf(`curl -sS -X POST "http://%s/%s"`, addr, c.path)}})
			if err != nil {
				t.Fatal(err)
			}
			check(t, rep, c)
			// The concurrent run is traced, with the held statements marked.
			f := rep.Findings[0]
			switch f.Outcome {
			case Confirmed:
				// Confirmed races are run again without the barrier.
				if f.FreeRuns == 0 {
					t.Errorf("confirmed race not run freely: %+v", f)
				}
			case NotReached:
				// The second request waits on the first's row lock: the
				// barrier opens without waiting for its timeout.
				if !f.Blocked {
					t.Errorf("lock wait not noticed")
				}
			}
			if len(f.Trace) < 2 || f.Started.IsZero() {
				t.Fatalf("no trace: %+v", f.Trace)
			}
			var b strings.Builder
			if err := WriteJSON(&b, "test", []*Report{rep}); err != nil || !strings.Contains(b.String(), `"held": true`) {
				t.Errorf("JSON trace has no held statement: %v\n%s", err, b.String())
			}
			b.Reset()
			WriteMarkdown(&b, []*Report{rep})
			if !strings.Contains(b.String(), "| "+c.path+" |") {
				t.Errorf("markdown:\n%s", b.String())
			}
		})
	}
}

func TestAppExitsEarly(t *testing.T) {
	db := testDB(t)
	_, err := Open(context.Background(), Config{Upstream: db, Listen: "127.0.0.1:0", App: "echo boom; exit 3", Ready: "http://" + freePort(t) + "/"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got %v", err)
	}
}

func TestRefusesRemoteDatabase(t *testing.T) {
	_, err := Open(context.Background(), Config{Upstream: "postgres://u@db.example.com/x"})
	if err == nil || !strings.Contains(err.Error(), "not a local database") {
		t.Fatalf("got %v", err)
	}
}

func TestShopPython(t *testing.T) {
	if err := exec.Command("python3", "-c", "import psycopg2").Run(); err != nil {
		t.Skip("python3 with psycopg2 not available")
	}
	db := testDB(t)
	app := "python3 " + filepath.Join(repoRoot(), "examples", "shop-py", "app.py")
	for _, c := range []tcase{
		{"buy?product=1&qty=3", Confirmed},
		{"buy-safe?product=2&qty=3", Held},
	} {
		t.Run(strings.SplitN(c.path, "?", 2)[0], func(t *testing.T) {
			check(t, hunt(t, db, app, freePort(t), c), c)
		})
	}
}

// The database must be back to its starting contents after a hunt.
func TestRestoresDatabase(t *testing.T) {
	db := testDB(t)
	bin := filepath.Join(t.TempDir(), "shop")
	build := exec.Command("go", "build", "-o", bin, "./examples/shop")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	c := tcase{"withdraw?account=1&amount=80", Confirmed}
	check(t, hunt(t, db, bin, freePort(t), c), c)
	conn, err := pgx.Connect(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var balance, ledger int
	conn.QueryRow(context.Background(), "SELECT balance FROM accounts WHERE id = 1").Scan(&balance)
	conn.QueryRow(context.Background(), "SELECT count(*) FROM ledger").Scan(&ledger)
	if balance != 100 || ledger != 0 {
		t.Fatalf("balance %d, ledger rows %d: not restored", balance, ledger)
	}
}

// With RestartApp the app is started again after every restore, and the
// hunt still works.
func TestRestartApp(t *testing.T) {
	db := testDB(t)
	bin := filepath.Join(t.TempDir(), "shop")
	build := exec.Command("go", "build", "-o", bin, "./examples/shop")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	addr := freePort(t)
	t.Setenv("ADDR", addr)
	var log strings.Builder
	rep, err := Run(context.Background(), Config{
		Upstream: db, Listen: "127.0.0.1:0", App: bin, Ready: "http://" + addr + "/health",
		Requests:       []string{fmt.Sprintf(`curl -sS -X POST "http://%s/withdraw?account=1&amount=80"`, addr)},
		BarrierTimeout: 2 * time.Second, RestartApp: true, AppLog: &log,
	})
	if err != nil {
		t.Fatal(err)
	}
	check(t, rep, tcase{"withdraw", Confirmed})
	// record, three serial runs, one race and its free runs, plus the first start
	if n := strings.Count(log.String(), "listening"); n < 6 {
		t.Errorf("app started %d times:\n%s", n, log.String())
	}
}
