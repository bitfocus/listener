//go:build !darwin

package main

// postDarwinMediaKey is a no-op on non-Darwin platforms.
func postDarwinMediaKey(int, []string) {}
