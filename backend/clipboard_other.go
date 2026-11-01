//go:build !windows

package main

// Servers have no clipboard. Hosted pages read the visitor's clipboard in the browser instead.
func watchClipboard(func(string)) {}
