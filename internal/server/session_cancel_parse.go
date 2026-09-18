package server

import (
	"iter"
	"strconv"
	"strings"
)

func splitLines(s string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, line := range strings.Split(s, "\n") {
			if !yield(strings.TrimSpace(line)) {
				return
			}
		}
	}
}

func twoInts(line string) (first, second int, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, 0, false
	}
	a, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false
	}
	b, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return a, b, true
}
