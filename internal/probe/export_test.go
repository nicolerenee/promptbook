package probe

// export_test.go gives the external _test package access to the
// internal JSON-shape parser without exporting the helper to
// production callers. Standard "white-box reach-through" pattern.

// ParseFFProbeOutputForTest is the external alias for
// parseFFProbeOutput so probe_test.go can drive the parser directly
// against a fixture without shelling ffprobe.
func ParseFFProbeOutputForTest(jsonBytes []byte) (MediaInfo, error) {
	return parseFFProbeOutput(jsonBytes)
}

// ParseFrameRateForTest exposes parseFrameRate for the external test
// package.
func ParseFrameRateForTest(rate string) float64 {
	return parseFrameRate(rate)
}
