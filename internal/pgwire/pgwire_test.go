package pgwire

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	m := Msg{Type: 'Q', Body: append([]byte("SELECT 1"), 0)}
	got, err := ReadMsg(bytes.NewReader(m.Bytes()))
	if err != nil || got.Type != 'Q' || !bytes.Equal(got.Body, m.Body) {
		t.Fatalf("%v %+v", err, got)
	}
	q, _ := DecodeString(got.Body)
	if q != "SELECT 1" {
		t.Fatal(q)
	}
}

func TestBadLength(t *testing.T) {
	if _, err := ReadMsg(bytes.NewReader([]byte{'Q', 0, 0, 0, 2})); err == nil {
		t.Fatal("want error")
	}
}

func TestDecodeBind(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("p1\x00s1\x00")
	binary.Write(&b, binary.BigEndian, int16(2)) // two format codes
	binary.Write(&b, binary.BigEndian, int16(0))
	binary.Write(&b, binary.BigEndian, int16(1))
	binary.Write(&b, binary.BigEndian, int16(3)) // three params
	binary.Write(&b, binary.BigEndian, int32(3))
	b.WriteString("abc")
	binary.Write(&b, binary.BigEndian, int32(4))
	binary.Write(&b, binary.BigEndian, int32(42))
	binary.Write(&b, binary.BigEndian, int32(-1)) // NULL
	got, err := DecodeBind(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got.Portal != "p1" || got.Statement != "s1" || len(got.Params) != 3 || string(got.Params[0]) != "abc" || got.Params[2] != nil {
		t.Fatalf("%+v", got)
	}
	// Two format codes for three params is malformed per spec; we fall back to text.
	if !reflect.DeepEqual(got.Formats, []int{0, 0, 0}) {
		t.Fatalf("formats %v", got.Formats)
	}
}

func TestDecodeBindSingleFormat(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("\x00\x00")
	binary.Write(&b, binary.BigEndian, int16(1))
	binary.Write(&b, binary.BigEndian, int16(1))
	binary.Write(&b, binary.BigEndian, int16(1))
	binary.Write(&b, binary.BigEndian, int32(8))
	binary.Write(&b, binary.BigEndian, int64(7))
	got, err := DecodeBind(b.Bytes())
	if err != nil || !reflect.DeepEqual(got.Formats, []int{1}) {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestStartup(t *testing.T) {
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, int32(8))
	binary.Write(&b, binary.BigEndian, int32(SSLRequestCode))
	pkt, code, err := ReadStartup(&b)
	if err != nil || code != SSLRequestCode || len(pkt) != 8 {
		t.Fatalf("%v %d %d", err, code, len(pkt))
	}
}

func TestErrorFields(t *testing.T) {
	f := ErrorFields([]byte("SERROR\x00C23505\x00Mduplicate key\x00\x00"))
	if f['C'] != "23505" || f['M'] != "duplicate key" || f['S'] != "ERROR" {
		t.Fatalf("%v", f)
	}
}
