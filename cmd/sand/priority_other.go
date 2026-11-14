//go:build !unix

package main

// lowerPriority does nothing where there is no nice.
func lowerPriority() {}
