// Package wirejson is the strict JSON boundary for saved records and API
// requests: unknown fields, trailing data and partial records fail instead
// of decoding loosely. Every durable type marshals and unmarshals through it.
package wirejson
