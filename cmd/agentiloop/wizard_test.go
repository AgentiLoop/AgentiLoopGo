package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfileForShellAndOS(t *testing.T) {
	if p, _ := profileFor("/bin/zsh", "/h", true); p != "/h/.zshrc" {
		t.Fatal(p)
	}
	if p, _ := profileFor("/bin/bash", "/h", true); p != "/h/.bash_profile" {
		t.Fatal(p)
	}
	if p, _ := profileFor("/usr/bin/bash", "/h", false); p != "/h/.bashrc" {
		t.Fatal(p)
	}
	if p, k := profileFor("/opt/fish", "/h", false); p != "/h/.config/fish/config.fish" || k != shellFish {
		t.Fatal(p, k)
	}
	if p, _ := profileFor("/bin/tcsh", "/h", false); p != "" {
		t.Fatal(p)
	}
}

func TestExportAndKeychainLines(t *testing.T) {
	if exportLine(shellPosix, "A", "b") != `export A="b"` || exportLine(shellFish, "A", "b") != `set -gx A "b"` {
		t.Fatal("export lines")
	}
	if !strings.HasPrefix(keychainLine(shellPosix, "K"), `export K="$(security find-generic-password`) {
		t.Fatal(keychainLine(shellPosix, "K"))
	}
}

func TestWriteBlockReplacesExisting(t *testing.T) {
	p := filepath.Join(t.TempDir(), "zshrc")
	os.WriteFile(p, []byte("alias a=b\n# >>> agentiloop >>>\nexport OLD=1\n# <<< agentiloop <<<\n"), 0o644)
	if err := writeBlock(p, []string{"export NEW=2"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "alias a=b\n\n# >>> agentiloop >>>\nexport NEW=2\n# <<< agentiloop <<<\n"
	if string(got) != want || len(strayLines(string(got))) != 0 {
		t.Fatalf("%q", got)
	}
}

func TestRunWizardScriptedAgainstFakeServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []map[string]string{{"id": "llama3"}, {"id": "qwen2.5-coder"}}})
	}))
	defer srv.Close()
	home := t.TempDir()
	profile := filepath.Join(t.TempDir(), "zshrc")
	t.Setenv("AGENTILOOP_HOME", home)
	t.Setenv("AGENTILOOP_SHELL_PROFILE", profile)
	for _, k := range credentialVars {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	// 3 = local server, URL, no key, model 2, save option 2 (env + profile), PATH: yes.
	in := strings.NewReader("3\n" + srv.URL + "/v1\n\n2\n2\ny\n")
	var out strings.Builder
	saved := loadSettings()
	if err := runWizard(context.Background(), &saved, in, &out); err != nil {
		t.Fatal(err, out.String())
	}
	if !strings.Contains(out.String(), "ok (2 model(s) available)") || !strings.Contains(out.String(), "All set: openai / qwen2.5-coder") {
		t.Fatal(out.String())
	}
	env, _ := os.ReadFile(envPath())
	if !strings.Contains(string(env), "OPENAI_BASE_URL="+srv.URL+"/v1") {
		t.Fatalf("%s", env)
	}
	prof, _ := os.ReadFile(profile)
	if !hasBlock(string(prof)) || !strings.Contains(string(prof), `export OPENAI_BASE_URL="`+srv.URL) {
		t.Fatalf("%s", prof)
	}
	back := loadSettings()
	if back.ModelFor("openai") != "qwen2.5-coder" || back.Last.Provider == nil || *back.Last.Provider != "openai" ||
		back.Setup.CompletedAt == nil || back.Setup.Profile == nil || *back.Setup.Profile != profile {
		t.Fatalf("%+v", back)
	}
}

func TestRunWizardCancelWritesNothing(t *testing.T) {
	t.Setenv("AGENTILOOP_HOME", t.TempDir())
	saved := loadSettings()
	var out strings.Builder
	if err := runWizard(context.Background(), &saved, strings.NewReader(""), &out); err != errCancelled {
		t.Fatal(err)
	}
	if _, err := os.Stat(settingsPath()); !os.IsNotExist(err) {
		t.Fatal("settings written")
	}
}
