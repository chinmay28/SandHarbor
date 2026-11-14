package grindtest

import "runtime"

// unameArch is what `uname -m` says on the platform the tests run on.
func unameArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

// OtherArch is a `uname -m` for a platform other than this one, for the tests
// of a machine the vault cannot simply copy itself to.
func OtherArch() string {
	if runtime.GOARCH == "amd64" {
		return "aarch64"
	}
	return "x86_64"
}
