// Package versionmeta bounds untrusted display-only version strings.
package versionmeta

const MaxBytes = 128

func Clean(value string) string {
	if len(value) > MaxBytes {
		return ""
	}
	for _, c := range value {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' || c == '_') {
			return ""
		}
	}
	return value
}
