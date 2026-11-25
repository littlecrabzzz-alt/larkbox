package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

func clientAddress(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err == nil && (host == "" || net.ParseIP(host).IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return address
}

func writeReadyFile(path, address string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".larkbox-ready-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := fmt.Fprintln(file, address)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
