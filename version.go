package main

import (
	"fmt"
	"strconv"
	"strings"
)

// version is the single source of truth for which build this is. It reaches
// three places: the -version flag, the update check that compares it against
// the newest release on GitHub, and the Windows file-version resource, which is
// generated from it by tools/mksyso.
//
// After changing it, regenerate the resource so Explorer agrees with the
// program:
//
//	go run ./tools/mksyso
const version = "0.3.0"

// updateRepo is where the update check looks. Owner and name of the GitHub
// repository these releases are published to.
const updateRepo = "WelFedTed/file-drop-server"

// parseVersion turns "v1.2.3" into its three numbers. Anything after the
// patch - "1.2.3-rc1" - is ignored for ordering, so a pre-release compares
// equal to its final version rather than sorting somewhere arbitrary.
func parseVersion(s string) ([3]int, error) {
	var out [3]int
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, fmt.Errorf("%q is not a version number", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("%q is not a version number", s)
		}
		out[i] = n
	}
	return out, nil
}

// newerVersion reports whether have is older than want.
func newerVersion(have, want string) (bool, error) {
	a, err := parseVersion(have)
	if err != nil {
		return false, err
	}
	b, err := parseVersion(want)
	if err != nil {
		return false, err
	}
	for i := 0; i < 3; i++ {
		if b[i] != a[i] {
			return b[i] > a[i], nil
		}
	}
	return false, nil
}
