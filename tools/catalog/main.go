// Command catalog maintains the signed OpsPanel plugin catalog.
//
//	go run ./tools/catalog keygen -key ~/.config/opspanel-plugins/minisign.key
//	go run ./tools/catalog sign   -key ~/.config/opspanel-plugins/minisign.key
//	go run ./tools/catalog verify
//
// sign reads plugins/*/server.json, signs each file's exact bytes and writes
// index.json; verify checks every entry of index.json against minisign.pub.
// The private key never enters this repository.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"aead.dev/minisign"
)

// Entry is one plugin in index.json: the manifest's exact text and its
// minisign signature over those bytes.
type Entry struct {
	Manifest  string `json:"manifest"`
	Signature string `json:"signature"`
}

// Index is the catalog document OpsPanel reads.
type Index struct {
	Version int     `json:"version"`
	Plugins []Entry `json:"plugins"`
}

type manifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

var (
	nameRE    = regexp.MustCompile(`^[a-zA-Z0-9.-]+/[a-zA-Z0-9._-]+$`)
	versionRE = regexp.MustCompile(`^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$`)
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: catalog keygen|sign|verify [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	key := fs.String("key", "", "private key file (keygen writes it, sign reads it)")
	root := fs.String("root", ".", "catalog repository root")
	_ = fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(*key, *root)
	case "sign":
		err = sign(*key, *root)
	case "verify":
		err = verify(*root)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalog:", err)
		os.Exit(1)
	}
}

func keygen(keyPath, root string) error {
	if keyPath == "" {
		return errors.New("-key is required")
	}
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("%s exists; refusing to replace a signing key", keyPath)
	}
	pub, priv, err := minisign.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privText, err := priv.MarshalText()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, privText, 0o600); err != nil {
		return err
	}
	pubText, err := pub.MarshalText()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "minisign.pub"), append(pubText, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("key id %016X\n%s\n", pub.ID(), pubText)
	return nil
}

func manifests(root string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(root, "plugins", "*", "server.json"))
	sort.Strings(paths)
	return paths, err
}

func check(raw []byte) (manifest, error) {
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if !nameRE.MatchString(m.Name) || !versionRE.MatchString(m.Version) {
		return m, fmt.Errorf("bad name or version: %q %q", m.Name, m.Version)
	}
	return m, nil
}

func sign(keyPath, root string) error {
	if keyPath == "" {
		return errors.New("-key is required")
	}
	text, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	var priv minisign.PrivateKey
	if err := priv.UnmarshalText(bytes.TrimSpace(text)); err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	paths, err := manifests(root)
	if err != nil {
		return err
	}
	idx := Index{Version: 1, Plugins: []Entry{}}
	seen := map[string]string{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		m, err := check(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if prev, dup := seen[m.Name]; dup {
			return fmt.Errorf("%s and %s both declare %s", prev, p, m.Name)
		}
		seen[m.Name] = p
		sig := minisign.SignWithComments(priv, raw, fmt.Sprintf("file:%s@%s", m.Name, m.Version), "signature from the OpsPanel plugin catalog key")
		idx.Plugins = append(idx.Plugins, Entry{Manifest: string(raw), Signature: string(sig)})
		fmt.Printf("signed %s %s\n", m.Name, m.Version)
	}
	out, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), append(out, '\n'), 0o644); err != nil {
		return err
	}
	return verify(root)
}

func verify(root string) error {
	pubText, err := os.ReadFile(filepath.Join(root, "minisign.pub"))
	if err != nil {
		return err
	}
	var pub minisign.PublicKey
	if err := pub.UnmarshalText(bytes.TrimSpace(pubText)); err != nil {
		return fmt.Errorf("read minisign.pub: %w", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "index.json"))
	if err != nil {
		return err
	}
	var idx Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return err
	}
	for i, e := range idx.Plugins {
		m, err := check([]byte(e.Manifest))
		if err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if !minisign.Verify(pub, []byte(e.Manifest), []byte(e.Signature)) {
			return fmt.Errorf("entry %d (%s): signature does not verify", i, m.Name)
		}
	}
	fmt.Printf("index.json: %d plugins verified\n", len(idx.Plugins))
	return nil
}
