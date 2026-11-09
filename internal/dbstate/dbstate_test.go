package dbstate

import (
	"reflect"
	"testing"
)

func TestDiff(t *testing.T) {
	a := State{"users": {"(a)"}, "orders": {"(1,3)", "(1,3)"}, "same": {"x"}}
	b := State{"users": {"(a)", "(a)"}, "orders": {"(1,3)"}, "same": {"x"}, "new": {"y"}}
	d := Diff(a, b, map[string]bool{"new": true})
	want := []TableDiff{
		{Table: "orders", Count: [2]int{2, 1}, OnlyA: []string{"(1,3)"}},
		{Table: "users", Count: [2]int{1, 2}, OnlyB: []string{"(a)"}},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("%+v", d)
	}
}

func TestTopo(t *testing.T) {
	ts := topo([]Table{
		{Schema: "public", Name: "redemptions", DependsOn: []string{"coupons", "users"}},
		{Schema: "public", Name: "users"},
		{Schema: "public", Name: "coupons"},
	})
	pos := map[string]int{}
	for i, t := range ts {
		pos[t.Name] = i
	}
	if len(ts) != 3 || pos["redemptions"] < pos["coupons"] || pos["redemptions"] < pos["users"] {
		t.Fatalf("%v", ts)
	}
}

func TestClockTypes(t *testing.T) {
	for typ, want := range map[string]bool{
		"timestamp with time zone": true, "timestamp(3) without time zone": true,
		"time without time zone": true, "interval": true, "date": false, "text": false,
	} {
		if isClockType(typ) != want {
			t.Errorf("%s", typ)
		}
	}
}
