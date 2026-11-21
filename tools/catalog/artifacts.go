package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const metaKey = "io.github.opspanel/plugin"

type artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// setArtifacts pins the binaries in dist (named <plugin>-<goos>-<goarch>[.exe])
// into plugins/<plugin>/server.json under base, sets the version, and points
// a binary-only packages entry at the linux/amd64 build.
func setArtifacts(root, plugin, version, dist, base string) error {
	path := filepath.Join(root, "plugins", plugin, "server.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(plugin) + `-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$`)
	entries, err := os.ReadDir(dist)
	if err != nil {
		return err
	}
	arts := map[string]artifact{}
	for _, e := range entries {
		m := re.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		sum, err := fileSHA256(filepath.Join(dist, e.Name()))
		if err != nil {
			return err
		}
		arts[m[1]+"/"+m[2]] = artifact{URL: strings.TrimSuffix(base, "/") + "/" + e.Name(), SHA256: sum}
	}
	if len(arts) == 0 {
		return fmt.Errorf("no %s-<goos>-<goarch> binaries in %s", plugin, dist)
	}

	var doc object
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	doc.set("version", version)

	var meta object
	if err := doc.get("_meta", &meta); err != nil {
		return err
	}
	var ext object
	if err := meta.get(metaKey, &ext); err != nil {
		return err
	}
	ext.setFirst("artifacts", arts)
	meta.set(metaKey, ext)
	doc.set("_meta", meta)

	if raw, ok := doc.vals["packages"]; ok {
		var pkgs []object
		if err := json.Unmarshal(raw, &pkgs); err != nil {
			return err
		}
		if lin, ok := arts["linux/amd64"]; ok && len(pkgs) > 0 && pkgs[0].string("registryType") == "mcpb" {
			pkgs[0].set("identifier", lin.URL)
			pkgs[0].set("version", version)
			pkgs[0].set("fileSha256", lin.SHA256)
		}
		doc.set("packages", pkgs)
	}

	out, err := marshal(doc)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, out, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	platforms := make([]string, 0, len(arts))
	for p := range arts {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	fmt.Printf("%s %s: %s\n", plugin, version, strings.Join(platforms, ", "))
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// object is a JSON object that keeps its key order, so rewriting a manifest
// changes only the fields that were set.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func (o *object) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("not a JSON object")
	}
	o.keys, o.vals = nil, map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		k := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return err
		}
		if _, dup := o.vals[k]; !dup {
			o.keys = append(o.keys, k)
		}
		o.vals[k] = v
	}
	return nil
}

func (o object) MarshalJSON() ([]byte, error) { return marshal(o) }

func (o *object) get(key string, v any) error {
	raw, ok := o.vals[key]
	if !ok {
		raw = []byte("{}")
	}
	return json.Unmarshal(raw, v)
}

func (o *object) string(key string) string {
	var s string
	_ = json.Unmarshal(o.vals[key], &s)
	return s
}

func (o *object) set(key string, v any) {
	if o.vals == nil {
		o.vals = map[string]json.RawMessage{}
	}
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = mustRaw(v)
}

func (o *object) setFirst(key string, v any) {
	o.set(key, v)
	keys := []string{key}
	for _, k := range o.keys {
		if k != key {
			keys = append(keys, k)
		}
	}
	o.keys = keys
}

func mustRaw(v any) json.RawMessage {
	b, err := marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// marshal encodes without HTML escaping (manifests hold <, > and & in text).
func marshal(v any) ([]byte, error) {
	if o, ok := v.(object); ok {
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range o.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			buf.Write(o.vals[k])
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
