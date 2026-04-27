package main

import "testing"

func TestParseCommandTagSelect(t *testing.T) {
	rows, ok := parseCommandTag([]byte("SELECT 5"))
	if !ok || rows != 5 {
		t.Fatalf("got rows=%d ok=%v, want 5 true", rows, ok)
	}
}

func TestParseCommandTagInsert(t *testing.T) {
	rows, ok := parseCommandTag([]byte("INSERT 0 1"))
	if !ok || rows != 1 {
		t.Fatalf("got rows=%d ok=%v, want 1 true", rows, ok)
	}
}

func TestParseCommandTagUpdate(t *testing.T) {
	rows, ok := parseCommandTag([]byte("UPDATE 3"))
	if !ok || rows != 3 {
		t.Fatalf("got rows=%d ok=%v, want 3 true", rows, ok)
	}
}

func TestParseCommandTagDelete(t *testing.T) {
	rows, ok := parseCommandTag([]byte("DELETE 12"))
	if !ok || rows != 12 {
		t.Fatalf("got rows=%d ok=%v, want 12 true", rows, ok)
	}
}

func TestParseCommandTagBegin(t *testing.T) {
	_, ok := parseCommandTag([]byte("BEGIN"))
	if ok {
		t.Fatal("BEGIN should report ok=false (no row count)")
	}
}

func TestParseCommandTagCreateTable(t *testing.T) {
	_, ok := parseCommandTag([]byte("CREATE TABLE"))
	if ok {
		t.Fatal("CREATE TABLE should report ok=false")
	}
}

func TestParseCommandTagEmpty(t *testing.T) {
	_, ok := parseCommandTag(nil)
	if ok {
		t.Fatal("empty tag should report ok=false")
	}
}
