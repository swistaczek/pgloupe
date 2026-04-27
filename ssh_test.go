package main

import (
	"net"
	"testing"
	"time"
)

func TestPickFreePortReturnsValid(t *testing.T) {
	p, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	if p < 1024 || p > 65535 {
		t.Fatalf("port=%d out of expected range", p)
	}
}

func TestPickFreePortIsFree(t *testing.T) {
	p, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(p)))
	if err != nil {
		t.Fatalf("port %d not actually free: %v", p, err)
	}
	ln.Close()
}

func TestWaitForListenerReadyImmediately(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	err = waitForListener(t.Context(), addr, time.Second)
	if err != nil {
		t.Fatalf("waitForListener: %v", err)
	}
}

func TestWaitForListenerTimesOut(t *testing.T) {
	// Pick a port and don't listen on it.
	p, err := pickFreePort()
	if err != nil {
		t.Fatalf("pickFreePort: %v", err)
	}
	addr := net.JoinHostPort("127.0.0.1", itoa(p))
	start := time.Now()
	err = waitForListener(t.Context(), addr, 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("returned too fast: %v", elapsed)
	}
}

func TestValidateSSHTargetRejectsOptions(t *testing.T) {
	bad := []string{
		"",
		"-oProxyCommand=evil",
		"-J jump",
		"--",
		"user@host;rm -rf /",
		"user@host\nrm",
		"user@host`whoami`",
		"user@host$(whoami)",
		"user@ho st",
	}
	for _, s := range bad {
		if err := validateSSHTarget(s); err == nil {
			t.Errorf("validateSSHTarget(%q) = nil, want error", s)
		}
	}
	good := []string{
		"host",
		"user@host",
		"user@host:2222",
		"u_ser@host-1.example.com",
		"root@10.0.0.1",
	}
	for _, s := range good {
		if err := validateSSHTarget(s); err != nil {
			t.Errorf("validateSSHTarget(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidateDockerNameRejectsOptions(t *testing.T) {
	bad := []string{
		"",
		"-rm",
		"name with space",
		`name"injection`,
		"name}}injection",
		"name;rm",
	}
	for _, s := range bad {
		if err := validateDockerName("container", s); err == nil {
			t.Errorf("validateDockerName(%q) = nil, want error", s)
		}
	}
	good := []string{"postgres", "startupkit-db", "pg_17", "pg.1"}
	for _, s := range good {
		if err := validateDockerName("container", s); err != nil {
			t.Errorf("validateDockerName(%q) = %v, want nil", s, err)
		}
	}
}

// itoa avoids strconv import.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [11]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
