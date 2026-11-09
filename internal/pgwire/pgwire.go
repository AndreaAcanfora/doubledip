// Package pgwire reads and writes PostgreSQL wire protocol (v3) messages,
// just enough for a transparent proxy that watches what goes through it.
package pgwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Special request codes sent in place of a protocol version in a startup packet.
const (
	SSLRequestCode    = 80877103
	GSSENCRequestCode = 80877104
	CancelRequestCode = 80877102
)

// maxMessage guards against garbage on the wire; Postgres itself caps
// messages at 1 GB.
const maxMessage = 1 << 30

// Msg is one regular protocol message: a type byte and its body.
type Msg struct {
	Type byte
	Body []byte
}

// Bytes returns the message encoded for the wire.
func (m Msg) Bytes() []byte {
	b := make([]byte, 5+len(m.Body))
	b[0] = m.Type
	binary.BigEndian.PutUint32(b[1:5], uint32(4+len(m.Body)))
	copy(b[5:], m.Body)
	return b
}

// ReadMsg reads one regular message.
func ReadMsg(r io.Reader) (Msg, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Msg{}, err
	}
	n := int(binary.BigEndian.Uint32(hdr[1:5]))
	if n < 4 || n > maxMessage {
		return Msg{}, fmt.Errorf("pgwire: bad message length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return Msg{}, err
	}
	return Msg{Type: hdr[0], Body: body}, nil
}

// ReadStartup reads an untyped startup-phase packet (StartupMessage,
// SSLRequest, GSSENCRequest or CancelRequest) and returns it whole,
// length prefix included, along with its request code.
func ReadStartup(r io.Reader) (packet []byte, code uint32, err error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, 0, err
	}
	n := int(binary.BigEndian.Uint32(hdr[0:4]))
	if n < 8 || n > 10000 {
		return nil, 0, fmt.Errorf("pgwire: bad startup length %d", n)
	}
	packet = make([]byte, n)
	copy(packet, hdr[:])
	if _, err := io.ReadFull(r, packet[8:]); err != nil {
		return nil, 0, err
	}
	return packet, binary.BigEndian.Uint32(hdr[4:8]), nil
}

// Reader decodes the fields of a message body.
type Reader struct {
	b   []byte
	err error
}

func NewReader(b []byte) *Reader { return &Reader{b: b} }

var errShort = errors.New("pgwire: message too short")

// Err reports the first decoding error.
func (r *Reader) Err() error { return r.err }

func (r *Reader) String() string {
	if r.err != nil {
		return ""
	}
	for i, c := range r.b {
		if c == 0 {
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			return s
		}
	}
	r.err = errShort
	return ""
}

func (r *Reader) Int16() int {
	if r.err != nil || len(r.b) < 2 {
		r.err = errShort
		return 0
	}
	v := int(int16(binary.BigEndian.Uint16(r.b)))
	r.b = r.b[2:]
	return v
}

func (r *Reader) Int32() int {
	if r.err != nil || len(r.b) < 4 {
		r.err = errShort
		return 0
	}
	v := int(int32(binary.BigEndian.Uint32(r.b)))
	r.b = r.b[4:]
	return v
}

func (r *Reader) Bytes(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = errShort
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

// Parse is a decoded Parse ('P') message.
type Parse struct {
	Name  string
	Query string
}

func DecodeParse(body []byte) (Parse, error) {
	r := NewReader(body)
	p := Parse{Name: r.String(), Query: r.String()}
	return p, r.Err()
}

// Bind is a decoded Bind ('B') message. Params holds each parameter as
// sent; nil means SQL NULL. Formats has one entry per parameter (0 text,
// 1 binary).
type Bind struct {
	Portal    string
	Statement string
	Formats   []int
	Params    [][]byte
}

func DecodeBind(body []byte) (Bind, error) {
	r := NewReader(body)
	b := Bind{Portal: r.String(), Statement: r.String()}
	nf := r.Int16()
	fmts := make([]int, 0, max(nf, 0))
	for i := 0; i < nf; i++ {
		fmts = append(fmts, r.Int16())
	}
	np := r.Int16()
	for i := 0; i < np; i++ {
		n := r.Int32()
		if n < 0 {
			b.Params = append(b.Params, nil)
			continue
		}
		b.Params = append(b.Params, append([]byte(nil), r.Bytes(n)...))
	}
	b.Formats = make([]int, np)
	switch {
	case len(fmts) == 1:
		for i := range b.Formats {
			b.Formats[i] = fmts[0]
		}
	case len(fmts) == np:
		copy(b.Formats, fmts)
	}
	return b, r.Err()
}

// DecodeExecute returns the portal name of an Execute ('E') message.
func DecodeExecute(body []byte) (string, error) {
	r := NewReader(body)
	s := r.String()
	return s, r.Err()
}

// DecodeClose returns the kind ('S' statement or 'P' portal) and name of a
// Close ('C') message.
func DecodeClose(body []byte) (byte, string, error) {
	if len(body) < 2 {
		return 0, "", errShort
	}
	r := NewReader(body[1:])
	s := r.String()
	return body[0], s, r.Err()
}

// DecodeString returns the single string body of messages such as Query
// ('Q') and CommandComplete ('C').
func DecodeString(body []byte) (string, error) {
	r := NewReader(body)
	s := r.String()
	return s, r.Err()
}

// ErrorFields decodes an ErrorResponse or NoticeResponse body into its
// fields, keyed by field type ('S' severity, 'C' code, 'M' message, ...).
func ErrorFields(body []byte) map[byte]string {
	f := map[byte]string{}
	for len(body) > 1 && body[0] != 0 {
		t := body[0]
		r := NewReader(body[1:])
		v := r.String()
		if r.Err() != nil {
			break
		}
		f[t] = v
		body = r.b
	}
	return f
}
