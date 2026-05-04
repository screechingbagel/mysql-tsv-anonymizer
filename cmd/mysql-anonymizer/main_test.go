package main

import (
	"strings"
	"testing"
)

func TestParseFlags_Required(t *testing.T) {
	_, err := parseFlags([]string{"--in", "x", "--out", "y", "-c", "z", "--seed", "42"})
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
}

func TestParseFlags_MissingSeed(t *testing.T) {
	_, err := parseFlags([]string{"--in", "x", "--out", "y", "-c", "z"})
	if err == nil || !strings.Contains(err.Error(), "seed") {
		t.Errorf("expected --seed required error, got %v", err)
	}
}

func TestParseFlags_MissingIn(t *testing.T) {
	_, err := parseFlags([]string{"--out", "y", "-c", "z", "--seed", "42"})
	if err == nil || !strings.Contains(err.Error(), "in") {
		t.Errorf("expected --in required error, got %v", err)
	}
}

func TestParseFlags_SeedZeroExplicit(t *testing.T) {
	o, err := parseFlags([]string{"--in", "x", "--out", "y", "-c", "z", "--seed", "0"})
	if err != nil {
		t.Errorf("expected --seed 0 to be accepted, got %v", err)
	}
	if o.Seed != 0 {
		t.Errorf("expected Seed=0, got %d", o.Seed)
	}
}
