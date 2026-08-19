package main

import (
	"strings"
	"testing"
)

func TestNewServerIDShapeAndAlphabet(t *testing.T) {
	identifier, err := newServerID()
	if err != nil {
		t.Fatalf("cannot generate identifier: %v", err)
	}
	if len(identifier) != serverIDLength {
		t.Fatalf("identifier = %q, want %d characters", identifier, serverIDLength)
	}
	for _, character := range identifier {
		if !strings.ContainsRune(serverIDAlphabet, character) {
			t.Fatalf("identifier %q contains %q, which is outside the alphabet", identifier, character)
		}
	}
}

// The host name used to be part of the identifier, which tied a permanent value
// to one a user can change at any time. Nothing but the alphabet is left, so a
// separator anywhere means a slug crept back in.
func TestNewServerIDCarriesNoHostName(t *testing.T) {
	identifier, err := newServerID()
	if err != nil {
		t.Fatalf("cannot generate identifier: %v", err)
	}
	if strings.ContainsAny(identifier, "_-.") {
		t.Fatalf("identifier = %q, want an opaque value with no embedded slug", identifier)
	}
}

func TestNewServerIDDoesNotRepeat(t *testing.T) {
	const draws = 2000
	seen := make(map[string]bool, draws)
	for attempt := 0; attempt < draws; attempt++ {
		identifier, err := newServerID()
		if err != nil {
			t.Fatalf("cannot generate identifier: %v", err)
		}
		if seen[identifier] {
			t.Fatalf("identifier %q was generated twice in %d draws", identifier, draws)
		}
		seen[identifier] = true
	}
}

// Rejection sampling is easy to get wrong in a way that only shows up as a
// skewed distribution, so every character of the alphabet has to be reachable.
func TestRandomBase62CoversTheWholeAlphabet(t *testing.T) {
	sample, err := randomBase62(20000)
	if err != nil {
		t.Fatalf("cannot generate sample: %v", err)
	}
	for _, character := range serverIDAlphabet {
		if !strings.ContainsRune(sample, character) {
			t.Fatalf("character %q never appeared in a 20000 character sample", character)
		}
	}
}

func TestRandomBase62RejectsEmptyLength(t *testing.T) {
	if _, err := randomBase62(0); err == nil {
		t.Fatal("expected a zero length to be rejected")
	}
}
