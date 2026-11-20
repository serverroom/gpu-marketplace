//go:build !windows

package sshtunnel

// ProtectKey is a no-op off Windows: ssh-keygen already writes the key 0600.
func ProtectKey(string) error { return nil }
