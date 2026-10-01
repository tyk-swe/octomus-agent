package sandbox

// EgressRefusals exposes the probe's gateway check to tests that run it against the real egress gateway, which this
// package cannot import.
var EgressRefusals = egressRefusals
