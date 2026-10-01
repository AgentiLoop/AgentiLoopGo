package core

import (
	"context"
	"encoding/json"
	"strings"
)

// ToolPatterns are name patterns for --allow-tool / --deny-tool: an exact tool name, or a prefix ending
// in "*" (mcp_*); "*" alone matches every tool.
type ToolPatterns []string

// NewToolPatterns flattens comma-separated values and drops blanks.
func NewToolPatterns(values []string) ToolPatterns {
	var out ToolPatterns
	for _, v := range values {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func (t ToolPatterns) Matches(tool string) bool {
	for _, p := range t {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(tool, prefix) {
				return true
			}
		} else if p == tool {
			return true
		}
	}
	return false
}

// Rules applies --deny-tool (always refused) and --allow-tool (never asked) before the wrapped policy.
// Deny wins when a tool matches both.
type Rules struct {
	Allow, Deny ToolPatterns
	Inner       PermissionPolicy
}

func (r *Rules) Check(ctx context.Context, tool string, isMutating bool, input json.RawMessage) Permission {
	switch {
	case r.Deny.Matches(tool):
		return Deny
	case r.Allow.Matches(tool):
		return Allow
	}
	return r.Inner.Check(ctx, tool, isMutating, input)
}
