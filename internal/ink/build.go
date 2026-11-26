//go:build linux || darwin

package ink

// BuildRevision is injected by deployment builds through ldflags.
var BuildRevision = "development"

// BuildVersion is injected alongside the revision for tagged releases.
var BuildVersion = "development"
