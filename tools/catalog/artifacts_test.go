package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleManifest = `{
  "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
  "name": "io.github.michael-ltm/demo",
  "title": "演示",
  "version": "0.0.0",
  "packages": [
    {
      "registryType": "mcpb",
      "identifier": "https://example.com/old",
      "transport": {
        "type": "stdio"
      },
      "environmentVariables": [
        {
          "name": "TOKEN",
          "isSecret": true
        }
      ]
    }
  ],
  "_meta": {
    "io.github.opspanel/plugin": {
      "i18n": {
        "en": {
          "title": "Demo <&>"
        }
      },
      "ui": []
    }
  }
}
`

func TestSetArtifactsPinsEveryPlatformAndKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plugins", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "plugins", "demo", "server.json")
	if err := os.WriteFile(path, []byte(sampleManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	dist := t.TempDir()
	files := map[string]string{"demo-linux-amd64": "la", "demo-darwin-arm64": "da", "demo-windows-amd64.exe": "wa", "SHA256SUMS": "x", "other-linux-amd64": "no"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := setArtifacts(root, "demo", "1.2.3", dist, "https://dl.example/demo-v1.2.3"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

	var m struct {
		Version  string `json:"version"`
		Packages []struct {
			Identifier string `json:"identifier"`
			Version    string `json:"version"`
			FileSHA256 string `json:"fileSha256"`
		} `json:"packages"`
		Meta map[string]struct {
			Artifacts map[string]struct{ URL, SHA256 string } `json:"artifacts"`
		} `json:"_meta"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	arts := m.Meta["io.github.opspanel/plugin"].Artifacts
	if m.Version != "1.2.3" || len(arts) != 3 {
		t.Fatalf("manifest = %s", text)
	}
	if a := arts["windows/amd64"]; a.URL != "https://dl.example/demo-v1.2.3/demo-windows-amd64.exe" || a.SHA256 != sum("wa") {
		t.Fatalf("windows = %+v", a)
	}
	if p := m.Packages[0]; p.Identifier != "https://dl.example/demo-v1.2.3/demo-linux-amd64" || p.Version != "1.2.3" || p.FileSHA256 != sum("la") {
		t.Fatalf("package = %+v", p)
	}
	// Field order and the rest of the document survive; nothing is HTML-escaped.
	for _, want := range []string{`"title": "Demo <&>"`, `"name": "TOKEN"`} {
		if !strings.Contains(text, want) {
			t.Errorf("%q missing from\n%s", want, text)
		}
	}
	if strings.Index(text, `"$schema"`) > strings.Index(text, `"name"`) || strings.Index(text, `"packages"`) > strings.Index(text, `"_meta"`) {
		t.Errorf("field order changed:\n%s", text)
	}
	if strings.Index(text, `"artifacts"`) > strings.Index(text, `"i18n"`) {
		t.Errorf("artifacts should lead the extension:\n%s", text)
	}
}

func TestSetArtifactsNeedsBinaries(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "plugins", "demo"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "plugins", "demo", "server.json"), []byte(sampleManifest), 0o644)
	if err := setArtifacts(root, "demo", "1.2.3", t.TempDir(), "https://dl.example"); err == nil {
		t.Fatal("no binaries should be an error")
	}
}
