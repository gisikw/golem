package main

import "testing"

func TestValidateTCPAuthLoopbackPolicy(t *testing.T) {
	for _, address := range []string{"127.0.0.1:1", "[::1]:1", "localhost:1"} {
		if err := validateTCPAuth(address, nil); err != nil {
			t.Fatalf("loopback %q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:1", ":1", "192.0.2.1:1"} {
		if err := validateTCPAuth(address, nil); err == nil {
			t.Fatalf("non-loopback %q accepted without tokens", address)
		}
		if err := validateTCPAuth(address, []string{"token"}); err != nil {
			t.Fatalf("authenticated %q rejected: %v", address, err)
		}
	}
}
