package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	got := parseEnvFile("# c\n\nexport A=\"x y\"\nB = 2\n=nope\nC\n")
	want := [][2]string{{"A", "x y"}, {"B", "2"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v", got)
	}
}

func TestEnvFileRoundTripAndPrecedence(t *testing.T) {
	t.Setenv("AGENTILOOP_HOME", t.TempDir())
	t.Setenv("ENVTEST_SET", "shell")
	t.Setenv("ENVTEST_EMPTY", "")
	p, err := saveEnvFile([][2]string{{"ENVTEST_SET", "file"}, {"ENVTEST_EMPTY", "file"}})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	loadEnvFile()
	if os.Getenv("ENVTEST_SET") != "shell" || os.Getenv("ENVTEST_EMPTY") != "file" {
		t.Fatal(os.Getenv("ENVTEST_SET"), os.Getenv("ENVTEST_EMPTY"))
	}
}

func TestSetupRecordKeychainNeverNull(t *testing.T) {
	t.Setenv("AGENTILOOP_HOME", t.TempDir())
	s := loadSettings()
	if err := saveSettings(&s); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(settingsPath())
	if !strings.Contains(string(data), `"keychain": []`) {
		t.Fatalf("%s", data)
	}
	var back Settings
	if err := json.Unmarshal(data, &back); err != nil || back.Setup.CompletedAt != nil {
		t.Fatal(err, back.Setup)
	}
}
