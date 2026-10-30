package dsp

import (
	"strconv"
	"strings"
)

// trimFloat renders a float without trailing zeros, for stage descriptions.
func trimFloat(v float64, precision int) string {
	s := strconv.FormatFloat(v, 'f', precision, 64)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}
