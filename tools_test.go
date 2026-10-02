package main

import "testing"

func TestMaskInternalAddresses(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trailing compressed ULA", " 2  fd12:3456:789a::  0.545 ms", " 2  [internal]  0.545 ms"},
		{"ULA subnet address", "fd00::", "[internal]"},
		{"link local subnet address", "fe80::", "[internal]"},
		{"leading compressed loopback", "::1", "[internal]"},
		{"unspecified IPv6", "::", "[internal]"},
		{"mapped hexadecimal loopback", "::ffff:7f00:1", "[internal]"},
		{"mapped dotted private IPv4", "::ffff:10.0.0.1", "[internal]"},
		{"IPv4 compatible address", "::10.0.0.1", "[internal]"},
		{"scoped link local", "fe80::1%eth0", "[internal]"},
		{"scoped VLAN address", "fe80::1%eth0.100", "[internal]"},
		{"scoped interface suffix", "fe80::1%veth-test@if2", "[internal]"},
		{"scoped ping punctuation", "From fe80::1%eth0: Destination unreachable", "From [internal]: Destination unreachable"},
		{"IPv4 ping punctuation", "From 10.0.0.1: Destination unreachable", "From [internal]: Destination unreachable"},
		{"IPv6 ping punctuation", "From fd00::: Destination unreachable", "From [internal]: Destination unreachable"},
		{"brackets and port", "[fd00::1]:443", "[[internal]]:443"},
		{"IPv4 port", "socket 10.0.0.1:33434", "socket [internal]:33434"},
		{"sentence punctuation", "(::1), fd00::1. 192.168.1.1;", "([internal]), [internal]. [internal];"},
		{"mixed addresses", "10.0.0.1 1.1.1.1 fd00:: 2606:4700:4700::1111", "[internal] 1.1.1.1 [internal] 2606:4700:4700::1111"},
		{"public spellings", "1.1.1.1 ::ffff:1.1.1.1 2606:4700:4700::1111 2606:4700:4700:0000:0000:0000:0000:1111", "1.1.1.1 ::ffff:1.1.1.1 2606:4700:4700::1111 2606:4700:4700:0000:0000:0000:0000:1111"},
		{"public punctuation and port", "(1.1.1.1): 1.1.1.1:443 [2606:4700:4700::1111]:443", "(1.1.1.1): 1.1.1.1:443 [2606:4700:4700::1111]:443"},
		{"scoped public address", "2606:4700:4700::1111%eth0", "2606:4700:4700::1111%eth0"},
		{"ordinary output", "64 bytes: icmp_seq=1 ttl=64 time=0.545 ms; dead beef face", "64 bytes: icmp_seq=1 ttl=64 time=0.545 ms; dead beef face"},
		{"whole malformed tokens", "999.10.0.0.1 x10.0.0.1 10.0.0.1.example fd00::g", "999.10.0.0.1 x10.0.0.1 10.0.0.1.example fd00::g"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := maskInternal(c.in); got != c.want {
				t.Errorf("maskInternal(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
