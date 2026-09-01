package main

import "sync"

// onceFunc is sync.OnceFunc, written out by hand.
//
// The standard library grew that function in Go 1.21, which is also the release
// that dropped Windows 7 and 8. The build for those machines therefore has to
// come from Go 1.20, the last toolchain that targets them, and cannot call it -
// so this stands in, and both builds use it rather than the two sources
// drifting apart over four lines. See releaseAsset in update.go.
func onceFunc(f func()) func() {
	var once sync.Once
	return func() { once.Do(f) }
}
