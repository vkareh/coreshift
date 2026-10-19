package certgen

import (
	"strings"
	"testing"
)

func TestGenerateSessionSecrets(t *testing.T) {
	first, err := GenerateSessionSecrets()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"apiVersion: osin.config.openshift.io/v1",
		"kind: SessionSecrets",
	} {
		if !strings.Contains(string(first), line) {
			t.Fatalf("expected %q in:\n%s", line, first)
		}
	}
	// The oauth server refuses to start on a session secrets file with
	// no secrets, and reads the two fields of each entry.
	for _, field := range []string{"authentication: ", "encryption: "} {
		if !strings.Contains(string(first), field) {
			t.Fatalf("expected %q in:\n%s", field, first)
		}
	}

	second, err := GenerateSessionSecrets()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("session secrets are identical across runs")
	}
}
