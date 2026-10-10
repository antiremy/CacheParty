// Package testutil holds helpers shared by tests in several packages.
package testutil

import (
	"encoding/binary"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// FakeDNS is a minimal UDP DNS server answering A queries from a table.
type FakeDNS struct {
	Addr    string // host:port to use as an upstream resolver
	Queries atomic.Int64

	mu      sync.RWMutex
	records map[string]net.IP
}

// NewFakeDNS starts a server on 127.0.0.1. Names are matched without the
// trailing dot. Unknown names get NXDOMAIN.
func NewFakeDNS(t testing.TB, records map[string]string) *FakeDNS {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &FakeDNS{Addr: pc.LocalAddr().String(), records: map[string]net.IP{}}
	for name, ip := range records {
		f.Set(name, ip)
	}
	go f.serve(pc)
	t.Cleanup(func() { pc.Close() })
	return f
}

// Set adds or replaces a record: an A record for an IPv4 address, an AAAA
// record for an IPv6 one. A name has one record.
func (f *FakeDNS) Set(name, ip string) {
	a := net.ParseIP(ip)
	if a4 := a.To4(); a4 != nil {
		a = a4
	}
	f.mu.Lock()
	f.records[name] = a
	f.mu.Unlock()
}

// Delete removes a record so the name answers NXDOMAIN.
func (f *FakeDNS) Delete(name string) {
	f.mu.Lock()
	delete(f.records, name)
	f.mu.Unlock()
}

func (f *FakeDNS) serve(pc net.PacketConn) {
	buf := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp := f.answer(buf[:n]); resp != nil {
			_, _ = pc.WriteTo(resp, from)
		}
	}
}

func (f *FakeDNS) answer(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	f.Queries.Add(1)
	// Walk the question name.
	i := 12
	var name []byte
	for i < len(q) && q[i] != 0 {
		l := int(q[i])
		if i+1+l > len(q) {
			return nil
		}
		if len(name) > 0 {
			name = append(name, '.')
		}
		name = append(name, q[i+1:i+1+l]...)
		i += 1 + l
	}
	end := i + 1 + 4 // root label + QTYPE + QCLASS
	if end > len(q) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(q[i+1:])

	f.mu.RLock()
	ip, ok := f.records[string(name)]
	f.mu.RUnlock()

	resp := make([]byte, 0, 64)
	resp = append(resp, q[0], q[1]) // ID
	flags := uint16(0x8180)         // response, RD, RA
	if !ok {
		flags |= 3 // NXDOMAIN
	}
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = append(resp, 0, 1) // QDCOUNT
	an := uint16(0)
	if ok && ((qtype == 1 && len(ip) == 4) || (qtype == 28 && len(ip) == 16)) {
		an = 1
	}
	resp = binary.BigEndian.AppendUint16(resp, an)
	resp = append(resp, 0, 0, 0, 0) // NSCOUNT, ARCOUNT
	resp = append(resp, q[12:end]...)
	if an == 1 {
		resp = append(resp, 0xC0, 0x0C, 0, byte(qtype), 0, 1, 0, 0, 0, 60, 0, byte(len(ip)))
		resp = append(resp, ip...)
	}
	return resp
}
