package proxy

import (
	"sync"
	"testing"
	"time"

	"github.com/AndreaAcanfora/doubledip/internal/sqlinfo"
)

func TestGateReleasesWhenBothArrive(t *testing.T) {
	g := NewGate("x", 2, 5*time.Second)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 1; i <= 2; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); g.Arrive(i) }(i)
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()
	if time.Since(start) > time.Second {
		t.Fatal("waited for the timeout")
	}
	if n, timedOut := g.Result(); n != 2 || timedOut {
		t.Fatalf("%d %v", n, timedOut)
	}
	g.Arrive(3) // after release everything passes
}

func TestGateTimesOut(t *testing.T) {
	g := NewGate("x", 2, 100*time.Millisecond)
	g.Arrive(1)
	if n, timedOut := g.Result(); n != 1 || !timedOut {
		t.Fatalf("%d %v", n, timedOut)
	}
}

func TestGateMatches(t *testing.T) {
	fp, _ := sqlinfo.Fingerprint("INSERT INTO users (email) VALUES ($1)")
	g := NewGate(fp, 2, time.Second)
	if !g.Matches("insert into users (email) values ('a@b.c')") {
		t.Fatal("literal form should match")
	}
	if !g.Matches("BEGIN; INSERT INTO users (email) VALUES ('x'); COMMIT") {
		t.Fatal("multi-statement query should match")
	}
	if g.Matches("INSERT INTO users_safe (email) VALUES ($1)") {
		t.Fatal("other table should not match")
	}
}

func TestStartupIsolation(t *testing.T) {
	pkt := []byte("\x00\x00\x00\x00\x00\x03\x00\x00user\x00app\x00options\x00-c default_transaction_isolation=serializable\x00\x00")
	if got := startupIsolation(pkt); got != "serializable" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderBinary(t *testing.T) {
	if renderBinary([]byte{0, 0, 0, 42}) != "42" || renderBinary([]byte{0xff, 0xff}) != "-1" || renderBinary([]byte{1, 2, 3}) != `\x010203` {
		t.Fatal("render")
	}
}
