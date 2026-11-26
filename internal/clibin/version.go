package clibin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var versionPattern = regexp.MustCompile(`(?:^|[\s(])v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)(?:$|[\s)])`)

func StableVersion(raw string) (string, error) {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if len(match) != 2 || strings.Contains(strings.SplitN(match[1], "+", 2)[0], "-") {
		return "", fmt.Errorf("unrecognized stable CLI version %q", raw)
	}
	value := strings.SplitN(match[1], "+", 2)[0]
	for _, part := range strings.Split(value, ".") {
		if len(part) > 1 && part[0] == '0' {
			return "", fmt.Errorf("invalid CLI version %q", raw)
		}
		if _, err := strconv.ParseUint(part, 10, 64); err != nil {
			return "", fmt.Errorf("invalid CLI version %q", raw)
		}
	}
	return value, nil
}

func CompareVersions(a, b string) (int, error) {
	av, err := StableVersion(a)
	if err != nil {
		return 0, err
	}
	bv, err := StableVersion(b)
	if err != nil {
		return 0, err
	}
	aa, bb := strings.Split(av, "."), strings.Split(bv, ".")
	for i := range aa {
		x, _ := strconv.ParseUint(aa[i], 10, 64)
		y, _ := strconv.ParseUint(bb[i], 10, 64)
		if x < y {
			return -1, nil
		}
		if x > y {
			return 1, nil
		}
	}
	return 0, nil
}
