//go:build !darwin || !cgo

package main

import "fmt"

func readGranolaSyncKeychain(service, account string) ([]byte, error) {
	return nil, fmt.Errorf("Granola Sync Keychain storage requires macOS with cgo enabled")
}

func writeGranolaSyncKeychain(service, account string, payload []byte) error {
	return fmt.Errorf("Granola Sync Keychain storage requires macOS with cgo enabled")
}
