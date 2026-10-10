package main

import (
	"encoding/binary"
	"flag"
	"net"
)

func runDNS(args []string) error {
	fs := flag.NewFlagSet("dns", flag.ExitOnError)
	addr := fs.String("addr", ":53", "UDP listen address")
	ip := fs.String("ip", "", "IPv4 address returned for every A query")
	_ = fs.Parse(args)
	var a [4]byte
	copy(a[:], net.ParseIP(*ip).To4())
	pc, err := net.ListenPacket("udp4", *addr)
	if err != nil {
		return err
	}
	buf := make([]byte, 512)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			return err
		}
		if resp := dnsAnswer(buf[:n], a); resp != nil {
			_, _ = pc.WriteTo(resp, from)
		}
	}
}

// dnsAnswer builds a response to q: one A record for A queries, an empty
// NOERROR answer for any other type.
func dnsAnswer(q []byte, ip [4]byte) []byte {
	if len(q) < 12 {
		return nil
	}
	i := 12
	for i < len(q) && q[i] != 0 {
		i += 1 + int(q[i])
	}
	end := i + 1 + 4 // root label + QTYPE + QCLASS
	if end > len(q) {
		return nil
	}
	isA := binary.BigEndian.Uint16(q[i+1:]) == 1

	resp := append(make([]byte, 0, 64), q[0], q[1])
	resp = binary.BigEndian.AppendUint16(resp, 0x8180) // response, RD, RA
	resp = append(resp, 0, 1)                          // QDCOUNT
	if isA {
		resp = append(resp, 0, 1)
	} else {
		resp = append(resp, 0, 0)
	}
	resp = append(resp, 0, 0, 0, 0) // NSCOUNT, ARCOUNT
	resp = append(resp, q[12:end]...)
	if isA {
		resp = append(resp, 0xC0, 0x0C, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
		resp = append(resp, ip[:]...)
	}
	return resp
}
