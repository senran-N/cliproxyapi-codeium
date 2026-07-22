//go:build !cgo

package main

import "fmt"

func callHost(method string, request []byte) ([]byte, error) {
	_ = request
	return nil, fmt.Errorf("codeium plugin: host callback %s requires cgo", method)
}
