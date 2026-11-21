package plugintest

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CheckManifest checks a plugin's server.json against its live tools: every
// status card binds a read-only tool, its path and column keys exist in that
// tool's output schema, and every titleKey has an English translation.
func CheckManifest(t *testing.T, cs *mcp.ClientSession, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Meta map[string]struct {
			I18n map[string]map[string]string `json:"i18n"`
			UI   []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				TitleKey string `json:"titleKey"`
				Tool     string `json:"tool"`
				Path     string `json:"path"`
				Columns  []struct {
					Key      string `json:"key"`
					TitleKey string `json:"titleKey"`
				} `json:"columns"`
			} `json:"ui"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	ext := m.Meta["io.github.opspanel/plugin"]
	if len(ext.UI) == 0 {
		t.Fatal("no status cards")
	}
	tools := map[string]*mcp.Tool{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools[tool.Name] = tool
	}
	en := ext.I18n["en"]
	if en["description"] == "" {
		t.Error("i18n.en.description missing")
	}
	for _, c := range ext.UI {
		tool := tools[c.Tool]
		if tool == nil {
			t.Errorf("card %s: no tool %s", c.ID, c.Tool)
			continue
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("card %s: tool %s is not read-only", c.ID, c.Tool)
		}
		if c.TitleKey != "" && en[c.TitleKey] == "" {
			t.Errorf("card %s: i18n.en.%s missing", c.ID, c.TitleKey)
		}
		props := properties(tool.OutputSchema)
		key := strings.TrimPrefix(c.Path, "/")
		field, ok := props[key].(map[string]any)
		if !ok {
			t.Errorf("card %s: %s has no output field %q", c.ID, c.Tool, key)
			continue
		}
		if c.Type != "table" {
			continue
		}
		items, _ := field["items"].(map[string]any)
		cols := properties(items)
		for _, col := range c.Columns {
			if _, ok := cols[col.Key]; !ok {
				t.Errorf("card %s: rows of %s have no field %q", c.ID, key, col.Key)
			}
			if col.TitleKey != "" && en[col.TitleKey] == "" {
				t.Errorf("card %s: i18n.en.%s missing", c.ID, col.TitleKey)
			}
		}
	}
}

func properties(schema any) map[string]any {
	b, _ := json.Marshal(schema)
	var s struct {
		Properties map[string]any `json:"properties"`
	}
	_ = json.Unmarshal(b, &s)
	return s.Properties
}
