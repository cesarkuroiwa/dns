// Copyright 2011 Miek Gieben. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dns

import (
	"net"
	"time"
)

// Envelope is used when doing a transfer with a remote server.
type Envelope struct {
	RR    []RR  // The set of RRs in the answer section of the AXFR reply message.
	Error error // If something went wrong, this contains the error.
}

// A Transfer defines parameters that are used during a zone transfer. 
type Transfer struct {
	*Conn
	DialTimeout    time.Duration // net.DialTimeout (ns), defaults to 2 * 1e9
	ReadTimeout    time.Duration // net.Conn.SetReadTimeout value for connections (ns), defaults to 2 * 1e9
	WriteTimeout   time.Duration // net.Conn.SetWriteTimeout value for connections (ns), defaults to 2 * 1e9
	TsigSecret     map[string]string // Secret(s) for Tsig map[<zonename>]<base64 secret>, zonename must be fully qualified
	tsigTimersOnly bool
}

// In performs an incoming transfer with the server in a.
func (t *Transfer) In(q *Msg, a string) (env chan *Envelope, err error) {
	t.Conn = new(Conn)
	timeout := dnsTimeout
	if t.DialTimeout != 0 {
		timeout = t.DialTimeout
	}
	t.Conn.Conn, err = net.DialTimeout("tcp", a, timeout)
	if err != nil {
		return nil, err
	}
	if err := t.WriteMsg(q); err != nil {
		return nil, err
	}
	env = make(chan *Envelope)
	go func() {
		if q.Question[0].Qtype == TypeAXFR {
			go t.inAxfr(q.Id, env)
			return
		}
		if q.Question[0].Qtype == TypeIXFR {
			go t.inIxfr(q.Id, env)
			return
		}
	}()
	return env, nil
}

func (t *Transfer) inAxfr(id uint16, c chan *Envelope) {
	first := true
	defer t.Close()
	defer close(c)
	timeout := dnsTimeout
	if t.ReadTimeout != 0 {
		timeout = t.ReadTimeout
	}
	for {
		t.Conn.SetReadDeadline(time.Now().Add(timeout))
		in, err := t.ReadMsg()
		if err != nil {
			c <- &Envelope{nil, err}
			return
		}
		if id != in.Id {
			c <- &Envelope{in.Answer, ErrId}
			return
		}
		if first {
			if !isSOAFirst(in) {
				c <- &Envelope{in.Answer, ErrSoa}
				return
			}
			first = !first
			// only one answer that is SOA, receive more
			if len(in.Answer) == 1 {
				t.tsigTimersOnly = true
				c <- &Envelope{in.Answer, nil}
				continue
			}
		}

		if !first {
			t.tsigTimersOnly = true // Subsequent envelopes use this.
			if isSOALast(in) {
				c <- &Envelope{in.Answer, nil}
				return
			}
			c <- &Envelope{in.Answer, nil}
		}
	}
	panic("dns: not reached")
}

func (t *Transfer) inIxfr(id uint16, c chan *Envelope) {
	serial := uint32(0) // The first serial seen is the current server serial
	first := true
	defer t.Close()
	defer close(c)
	timeout := dnsTimeout
	if t.ReadTimeout != 0 {
		timeout = t.ReadTimeout
	}
	for {
		t.SetReadDeadline(time.Now().Add(timeout))
		in, err := t.ReadMsg()
		if err != nil {
			c <- &Envelope{in.Answer, err}
			return
		}
		if id != in.Id {
			c <- &Envelope{in.Answer, ErrId}
			return
		}
		if first {
			// A single SOA RR signals "no changes"
			if len(in.Answer) == 1 && isSOAFirst(in) {
				c <- &Envelope{in.Answer, nil}
				return
			}

			// Check if the returned answer is ok
			if !isSOAFirst(in) {
				c <- &Envelope{in.Answer, ErrSoa}
				return
			}
			// This serial is important
			serial = in.Answer[0].(*SOA).Serial
			first = !first
		}

		// Now we need to check each message for SOA records, to see what we need to do
		if !first {
			t.tsigTimersOnly = true
			// If the last record in the IXFR contains the servers' SOA,  we should quit
			if v, ok := in.Answer[len(in.Answer)-1].(*SOA); ok {
				if v.Serial == serial {
					c <- &Envelope{in.Answer, nil}
					return
				}
			}
			c <- &Envelope{in.Answer, nil}
		}
	}
}



// Out performs an outgoing transfer with the client connecting in w.
// Basic use pattern:
//
//	ch := make(chan *dns.Envelope)
//	tr := new(dns.Transfer)
//	tr.Out(w, r, ch)
//	c <- &dns.Envelope{RR: []dns.RR{soa, rr1, rr2, rr3, soa}}
//	close(ch)
//	w.Hijack()
//	// w.Close() // Client closes connection
//
// The server is responsible for sending the correct sequence of RRs through the
// channel ch.
func (t *Transfer) Out(w ResponseWriter, q *Msg, ch chan *Envelope) error {
	r := new(Msg)
	// Compress?
	r.SetReply(q)
	r.Authoritative = true

	go func() {
		for x := range ch {
			// assume it fits TODO(miek): fix
			r.Answer = append(r.Answer, x.RR...)
			if err := w.WriteMsg(r); err != nil {
				return
			}
		}
		w.TsigTimersOnly(true)
		r.Answer = nil
	}()
	return nil
}

// ReadMsg reads a message from the transfer connection t.
func (t *Transfer) ReadMsg() (*Msg, error) {
	m := new(Msg)
	p := make([]byte, MaxMsgSize)
	n, err := t.Read(p)
	if err != nil && n == 0 {
		return nil, err
	}
	p = p[:n]
	if err := m.Unpack(p); err != nil {
		return nil, err
	}
	if ts := m.IsTsig(); ts != nil && t.TsigSecret != nil {
		if _, ok := t.TsigSecret[ts.Hdr.Name]; !ok {
			return m, ErrSecret
		}
		// Need to work on the original message p, as that was used to calculate the tsig.
		err = TsigVerify(p, t.TsigSecret[ts.Hdr.Name], t.tsigRequestMAC, t.tsigTimersOnly)
	}
	return m, err
}

// WriteMsg write a message throught the transfer connection t.
func (t *Transfer) WriteMsg(m *Msg) (err error) {
	var out []byte
	if ts := m.IsTsig(); ts != nil && t.TsigSecret != nil {
		if _, ok := t.TsigSecret[ts.Hdr.Name]; !ok {
			return ErrSecret
		}
		out, t.tsigRequestMAC, err = TsigGenerate(m, t.TsigSecret[ts.Hdr.Name], t.tsigRequestMAC, t.tsigTimersOnly)
	} else {
		out, err = m.Pack()
	}
	if err != nil {
		return err
	}
	if _, err = t.Write(out); err != nil {
		return err
	}
	return nil
}

func isSOAFirst(in *Msg) bool {
	if len(in.Answer) > 0 {
		return in.Answer[0].Header().Rrtype == TypeSOA
	}
	return false
}

func isSOALast(in *Msg) bool {
	if len(in.Answer) > 0 {
		return in.Answer[len(in.Answer)-1].Header().Rrtype == TypeSOA
	}
	return false
}
