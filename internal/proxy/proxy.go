// Package proxy is a transparent PostgreSQL proxy. It records every
// statement that goes through it, with its transaction, and can hold a
// chosen statement at a barrier until a second connection reaches it too.
// That barrier is what forces two requests into the interleaving that
// breaks them.
package proxy

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AndreaAcanfora/doubledip/internal/pgwire"
	"github.com/AndreaAcanfora/doubledip/internal/sqlinfo"
)

// Exec is one statement the database finished executing.
type Exec struct {
	Seq              int64     // global completion order
	Conn             int       // proxy connection number
	Tx               int64     // transaction id; statements of one transaction share it
	SQL              string    // statement text
	Params           []string  // bind parameters, rendered as text (nil entries become "NULL")
	Tag              string    // CommandComplete tag, e.g. "UPDATE 1"
	Err              string    // SQLSTATE and message when the statement failed
	SessionIsolation string    // session default isolation if the app changed it, else ""
	Start            time.Time // when the app sent it
	At               time.Time // when the database finished it
}

// Proxy forwards connections from a listener to an upstream server.
type Proxy struct {
	Upstream string // host:port of the real database
	Logf     func(format string, args ...any)

	ln      net.Listener
	connSeq atomic.Int64
	txSeq   atomic.Int64
	execSeq atomic.Int64

	mu        sync.Mutex
	recording bool
	execs     []Exec
	gate      *Gate
	conns     map[int]net.Conn
}

// Listen starts accepting connections on addr. It returns the bound address.
func (p *Proxy) Listen(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	p.ln = ln
	p.conns = map[int]net.Conn{}
	go p.accept()
	return ln.Addr().String(), nil
}

// Close stops the listener and drops every proxied connection.
func (p *Proxy) Close() error {
	if p.ln == nil {
		return nil
	}
	err := p.ln.Close()
	p.mu.Lock()
	for _, c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
	return err
}

// StartRecording clears the log and starts recording statements.
func (p *Proxy) StartRecording() {
	p.mu.Lock()
	p.execs = nil
	p.recording = true
	p.mu.Unlock()
}

// StopRecording stops recording and returns what was recorded, in
// completion order.
func (p *Proxy) StopRecording() []Exec {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recording = false
	out := p.execs
	p.execs = nil
	return out
}

// SetGate installs a barrier; nil removes it.
func (p *Proxy) SetGate(g *Gate) {
	p.mu.Lock()
	old := p.gate
	p.gate = g
	p.mu.Unlock()
	if old != nil && old != g {
		old.Release()
	}
}

func (p *Proxy) currentGate() *Gate {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gate
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

func (p *Proxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.serve(c)
	}
}

type pendingExec struct {
	sql    string
	params []string
	start  time.Time
}

type conn struct {
	p   *Proxy
	id  int
	tx  atomic.Int64
	iso atomic.Value // string

	mu      sync.Mutex
	pending []pendingExec
	stmts   map[string]string
	portals map[string]pendingExec
}

func (p *Proxy) serve(client net.Conn) {
	defer client.Close()
	cr := bufio.NewReader(client)

	// Startup phase: decline TLS and GSS encryption, pass cancels through.
	var startup []byte
	for {
		pkt, code, err := pgwire.ReadStartup(cr)
		if err != nil {
			return
		}
		switch code {
		case pgwire.SSLRequestCode, pgwire.GSSENCRequestCode:
			if _, err := client.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		case pgwire.CancelRequestCode:
			if up, err := net.Dial("tcp", p.Upstream); err == nil {
				up.Write(pkt)
				up.Close()
			}
			return
		}
		startup = pkt
		break
	}

	up, err := net.Dial("tcp", p.Upstream)
	if err != nil {
		p.logf("proxy: cannot reach upstream %s: %v", p.Upstream, err)
		return
	}
	defer up.Close()
	if _, err := up.Write(startup); err != nil {
		return
	}

	c := &conn{p: p, id: int(p.connSeq.Add(1)), stmts: map[string]string{}, portals: map[string]pendingExec{}}
	c.tx.Store(p.txSeq.Add(1))
	c.iso.Store(startupIsolation(startup))
	p.mu.Lock()
	p.conns[c.id] = client
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.conns, c.id)
		p.mu.Unlock()
	}()

	done := make(chan struct{}, 2)
	go func() { c.clientToServer(cr, up); up.Close(); done <- struct{}{} }()
	go func() { c.serverToClient(bufio.NewReader(up), client); client.Close(); done <- struct{}{} }()
	<-done
	<-done
}

func (c *conn) clientToServer(r *bufio.Reader, w io.Writer) {
	bw := bufio.NewWriter(w)
	for {
		m, err := pgwire.ReadMsg(r)
		if err != nil {
			return
		}
		var hold string // statement to check against the gate before forwarding
		switch m.Type {
		case 'Q':
			q, _ := pgwire.DecodeString(m.Body)
			c.noteSession(q)
			c.mu.Lock()
			for _, s := range sqlinfo.Split(q) {
				c.pending = append(c.pending, pendingExec{sql: s, start: time.Now()})
			}
			c.mu.Unlock()
			hold = q
		case 'P':
			if ps, err := pgwire.DecodeParse(m.Body); err == nil {
				c.mu.Lock()
				c.stmts[ps.Name] = ps.Query
				c.mu.Unlock()
			}
		case 'B':
			if b, err := pgwire.DecodeBind(m.Body); err == nil {
				c.mu.Lock()
				q := c.stmts[b.Statement]
				c.portals[b.Portal] = pendingExec{sql: q, params: renderParams(b), start: time.Now()}
				c.mu.Unlock()
				c.noteSession(q)
				hold = q
			}
		case 'E':
			if portal, err := pgwire.DecodeExecute(m.Body); err == nil {
				c.mu.Lock()
				c.pending = append(c.pending, c.portals[portal])
				c.mu.Unlock()
			}
		case 'C':
			if kind, name, err := pgwire.DecodeClose(m.Body); err == nil {
				c.mu.Lock()
				if kind == 'S' {
					delete(c.stmts, name)
				} else {
					delete(c.portals, name)
				}
				c.mu.Unlock()
			}
		}
		if hold != "" {
			if g := c.p.currentGate(); g != nil && g.Matches(hold) {
				// Send what we already have, then wait at the barrier.
				if err := bw.Flush(); err != nil {
					return
				}
				g.Arrive(c.id)
			}
		}
		if _, err := bw.Write(m.Bytes()); err != nil {
			return
		}
		if r.Buffered() == 0 {
			if err := bw.Flush(); err != nil {
				return
			}
		}
	}
}

func (c *conn) serverToClient(r *bufio.Reader, w io.Writer) {
	bw := bufio.NewWriter(w)
	for {
		m, err := pgwire.ReadMsg(r)
		if err != nil {
			return
		}
		switch m.Type {
		case 'C': // CommandComplete
			tag, _ := pgwire.DecodeString(m.Body)
			c.complete(tag, "")
		case 'I': // EmptyQueryResponse
			c.complete("", "")
		case 's': // PortalSuspended
			c.complete("SUSPENDED", "")
		case 'E': // ErrorResponse
			f := pgwire.ErrorFields(m.Body)
			c.complete("", f['C']+" "+f['M'])
		case 'Z': // ReadyForQuery: anything still pending was skipped
			c.mu.Lock()
			c.pending = nil
			c.mu.Unlock()
			if len(m.Body) == 1 && m.Body[0] == 'I' {
				c.tx.Store(c.p.txSeq.Add(1))
			}
		}
		if _, err := bw.Write(m.Bytes()); err != nil {
			return
		}
		if r.Buffered() == 0 {
			if err := bw.Flush(); err != nil {
				return
			}
		}
	}
}

// complete pops the oldest pending statement and records it.
func (c *conn) complete(tag, errMsg string) {
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return
	}
	pe := c.pending[0]
	c.pending = c.pending[1:]
	c.mu.Unlock()

	p := c.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.recording || pe.sql == "" {
		return
	}
	p.execs = append(p.execs, Exec{
		Seq:              p.execSeq.Add(1),
		Conn:             c.id,
		Tx:               c.tx.Load(),
		SQL:              pe.sql,
		Params:           pe.params,
		Tag:              tag,
		Err:              strings.TrimSpace(errMsg),
		SessionIsolation: c.iso.Load().(string),
		Start:            pe.start,
		At:               time.Now(),
	})
}

// noteSession tracks session-level changes to the default isolation level.
func (c *conn) noteSession(q string) {
	l := strings.ToLower(q)
	if !strings.Contains(l, "isolation") {
		return
	}
	if strings.Contains(l, "session characteristics") || strings.Contains(l, "default_transaction_isolation") {
		if lvl := isolationIn(l); lvl != "" {
			c.iso.Store(lvl)
		}
	}
}

func isolationIn(l string) string {
	for _, lvl := range []string{"serializable", "repeatable read", "read committed", "read uncommitted"} {
		if strings.Contains(l, lvl) {
			return lvl
		}
	}
	if strings.Contains(l, "repeatable") {
		return "repeatable read"
	}
	return ""
}

// startupIsolation finds default_transaction_isolation in the startup
// packet's "options" parameter, as set by e.g. PGOPTIONS.
func startupIsolation(pkt []byte) string {
	parts := strings.Split(string(pkt[8:]), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		if parts[i] == "options" || parts[i] == "default_transaction_isolation" {
			if lvl := isolationIn(strings.ToLower(strings.ReplaceAll(parts[i+1], "\\ ", " "))); lvl != "" {
				return lvl
			}
		}
	}
	return ""
}

func renderParams(b pgwire.Bind) []string {
	out := make([]string, len(b.Params))
	for i, v := range b.Params {
		switch {
		case v == nil:
			out[i] = "NULL"
		case b.Formats[i] == 1:
			out[i] = renderBinary(v)
		default:
			out[i] = string(v)
		}
	}
	return out
}

// renderBinary shows binary-format integers as numbers and anything else
// as hex.
func renderBinary(v []byte) string {
	switch len(v) {
	case 2:
		return fmt.Sprint(int16(uint16(v[0])<<8 | uint16(v[1])))
	case 4:
		return fmt.Sprint(int32(uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])))
	case 8:
		var n uint64
		for _, b := range v {
			n = n<<8 | uint64(b)
		}
		return fmt.Sprint(int64(n))
	}
	return `\x` + hex.EncodeToString(v)
}

// Gate is a barrier for one statement shape. The first connection to run a
// matching statement waits until a second connection runs it too, or until
// the timeout. Then everything passes.
type Gate struct {
	Fingerprint string
	Parties     int
	Timeout     time.Duration

	mu       sync.Mutex
	arrived  map[int]bool
	released bool
	timedOut bool
	ch       chan struct{}
	cache    map[string]bool
}

// NewGate creates a barrier for statements with the given fingerprint.
func NewGate(fingerprint string, parties int, timeout time.Duration) *Gate {
	return &Gate{Fingerprint: fingerprint, Parties: parties, Timeout: timeout,
		arrived: map[int]bool{}, ch: make(chan struct{}), cache: map[string]bool{}}
}

// Matches reports whether a statement (or any statement of a
// multi-statement query) has the gate's fingerprint.
func (g *Gate) Matches(q string) bool {
	g.mu.Lock()
	if m, ok := g.cache[q]; ok {
		g.mu.Unlock()
		return m
	}
	g.mu.Unlock()
	m := false
	for _, s := range sqlinfo.Split(q) {
		if fp, err := sqlinfo.Fingerprint(s); err == nil && fp == g.Fingerprint {
			m = true
			break
		}
	}
	g.mu.Lock()
	g.cache[q] = m
	g.mu.Unlock()
	return m
}

// Arrive blocks the calling connection at the barrier.
func (g *Gate) Arrive(conn int) {
	g.mu.Lock()
	if g.released {
		g.mu.Unlock()
		return
	}
	g.arrived[conn] = true
	if len(g.arrived) >= g.Parties {
		g.release(false)
		g.mu.Unlock()
		return
	}
	if len(g.arrived) == 1 {
		time.AfterFunc(g.Timeout, func() {
			g.mu.Lock()
			g.release(true)
			g.mu.Unlock()
		})
	}
	ch := g.ch
	g.mu.Unlock()
	<-ch
}

func (g *Gate) release(timedOut bool) {
	if g.released {
		return
	}
	g.released = true
	g.timedOut = timedOut
	close(g.ch)
}

// Release opens the barrier.
func (g *Gate) Release() {
	g.mu.Lock()
	g.release(false)
	g.mu.Unlock()
}

// Result reports how many connections reached the barrier and whether it
// opened because of the timeout.
func (g *Gate) Result() (arrived int, timedOut bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.arrived), g.timedOut
}
