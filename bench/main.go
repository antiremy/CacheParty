// Command bench is the load-test harness. It has three modes:
//
//	bench origin   a fake CDN origin that counts the bytes it serves
//	bench dns      a DNS server that answers every A query with one address
//	bench load     N concurrent clients on a cold and then a warm cache
//
// The same scenario runs against CacheParty and lancachenet/monolithic; see
// run.sh and compose.yml.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "origin":
		err = runOrigin(os.Args[2:])
	case "dns":
		err = runDNS(os.Args[2:])
	case "load":
		err = runLoad(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: bench origin|dns|load [flags]")
	os.Exit(2)
}
