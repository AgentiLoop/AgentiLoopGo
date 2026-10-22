package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// change_dir: the model switches the session's working directory (the agent applies it
// after a successful call); /cd in the CLI uses the same ResolveDir.

const ChangeDirToolName = "change_dir"

// ResolveDir returns path as an existing directory: ~ is the home folder, a relative path is taken from cwd.
func ResolveDir(cwd, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("no directory given")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if path == "~" {
			path = home
		} else if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	full, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: No such file or directory (os error 2)", path)
		}
		return "", fmt.Errorf("%s: %v", path, err)
	}
	if full, err = filepath.Abs(full); err != nil {
		return "", err
	}
	if info, err := os.Stat(full); err != nil {
		return "", fmt.Errorf("%s: %v", full, err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", full)
	}
	return full, nil
}

type ChangeDir struct{}

func (ChangeDir) Name() string { return ChangeDirToolName }
func (ChangeDir) Description() string {
	return "Change the working directory for the rest of the session: later tool calls, relative paths and shell commands run there. Use it when the user asks to switch to another project or folder."
}
func (ChangeDir) InputSchema() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory (absolute, relative to cwd, or starting with ~)"},
		},
		"required": []string{"path"},
	}
}
func (ChangeDir) IsMutating() bool { return false }

func (ChangeDir) Call(_ context.Context, tc ToolContext, input json.RawMessage) (string, error) {
	var a struct {
		Path *string `json:"path"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return "", InvalidInput("%v", err)
	}
	if a.Path == nil {
		return "", InvalidInput("missing `path`")
	}
	dir, err := ResolveDir(tc.Cwd, *a.Path)
	if err != nil {
		return "", Failed("%v", err)
	}
	return "working directory is now " + dir, nil
}

// changeDirTarget is where a successful change_dir call with input moves cwd ("" if nowhere).
func changeDirTarget(cwd string, input json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(input, &a) != nil {
		return ""
	}
	dir, err := ResolveDir(cwd, a.Path)
	if err != nil {
		return ""
	}
	return dir
}
