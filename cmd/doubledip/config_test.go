package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "doubledip.yml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("TEST_DB", "postgres://u@localhost/x")
	c, err := loadConfig(write(t, `
db: ${TEST_DB}
app: npm start
ready: http://localhost:3000/health
barrier_timeout: 5s
scenarios:
  - name: coupon
    request: curl -X POST "localhost:3000/redeem?user=$DOUBLEDIP_N"
    ignore: [orders.note]
  - requests:
      - curl -X POST localhost:3000/withdraw
      - curl -X POST localhost:3000/transfer
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.DB != "postgres://u@localhost/x" || c.BarrierTimeout != 5*time.Second || len(c.Scenarios) != 2 {
		t.Fatalf("%+v", c)
	}
	s := c.Scenarios[0].scenario()
	if s.Name != "coupon" || !reflect.DeepEqual(s.Requests, []string{`curl -X POST "localhost:3000/redeem?user=$DOUBLEDIP_N"`}) || !reflect.DeepEqual(s.Ignore, []string{"orders.note"}) {
		t.Fatalf("request commands must not be expanded: %+v", s)
	}
	if c.Scenarios[1].Name != "scenario 2" || len(c.Scenarios[1].Requests) != 2 {
		t.Fatalf("%+v", c.Scenarios[1])
	}
}

func TestLoadConfigErrors(t *testing.T) {
	for body, want := range map[string]string{
		"db: x\n":                          "no scenarios",
		"db: x\nscenarios:\n  - name: a\n": "needs a request",
		"db: x\nscenarios:\n  - request: a\n    requests: [b]\n": "both request and requests",
		"db: x\nscenarios:\n  - requests: [a, b, c]\n":           "needs a request",
		"db: x\ntypo: 1\nscenarios:\n  - request: a\n":           "typo",
	} {
		_, err := loadConfig(write(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", body, err, want)
		}
	}
}
