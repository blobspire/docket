// Package config holds the per-machine paths docket uses.
//
// Everything lives under DOCKET_HOME (default ~/.docket):
//
//	spool/    append-only event segments the server pulls over SSH
//	mailbox/  answers and context the server pushes back
//	machine   this machine's name in docket (written by `docket init`)
package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Home is docket's per-machine directory.
func Home() string {
	if h := os.Getenv("DOCKET_HOME"); h != "" {
		return h
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		return ".docket"
	}
	return filepath.Join(dir, ".docket")
}

func SpoolDir() string   { return filepath.Join(Home(), "spool") }
func MailboxDir() string { return filepath.Join(Home(), "mailbox") }

// Machine is this machine's name in docket: the contents of
// $DOCKET_HOME/machine if present, else the short hostname.
func Machine() string {
	if b, err := os.ReadFile(filepath.Join(Home(), "machine")); err == nil {
		if m := strings.TrimSpace(string(b)); m != "" {
			return m
		}
	}
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}

// SetMachine records this machine's name.
func SetMachine(name string) error {
	if err := os.MkdirAll(Home(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(Home(), "machine"), []byte(name+"\n"), 0o600)
}
