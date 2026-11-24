//go:build linux || darwin

package ink

// BuildRevision is injected by deployment builds through ldflags.
var BuildRevision = "development"
