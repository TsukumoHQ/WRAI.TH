package main

import (
	"errors"
	"net"
	"syscall"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":    true,
		"localhost":    true,
		"::1":          true,
		"[::1]":        true,
		"127.0.0.5":    true, // whole 127/8 is loopback
		"0.0.0.0":      false,
		"192.168.1.10": false,
		"10.0.0.1":     false,
		"example.com":  false,
	}
	for host, want := range cases {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestIsAddrInUse(t *testing.T) {
	// Real EADDRINUSE: bind a port twice.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	l2, err2 := net.Listen("tcp", l.Addr().String())
	if err2 == nil {
		_ = l2.Close()
		t.Fatal("expected second listen to fail")
	}
	if !isAddrInUse(err2) {
		t.Errorf("isAddrInUse(%v) = false, want true", err2)
	}

	// Unrelated errors must not match.
	if isAddrInUse(errors.New("boom")) {
		t.Error("isAddrInUse(generic) = true, want false")
	}
	if isAddrInUse(syscall.ECONNREFUSED) {
		t.Error("isAddrInUse(ECONNREFUSED) = true, want false")
	}
}
