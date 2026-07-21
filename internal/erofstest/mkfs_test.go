package erofstest

import "testing"

func TestParseVersionAcceptsGitSuffix(t *testing.T) {
	got, err := parseVersion("1.8.10-g17c8d394")
	if err != nil {
		t.Fatal(err)
	}
	if got != (semver{major: 1, minor: 8, patch: 10}) {
		t.Fatalf("version=%+v", got)
	}
}
