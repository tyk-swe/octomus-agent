// Package workspace owns the managed directories under the state directory:
// owned work trees, their sibling git metadata and the bounded measurement
// that storage admission checks. It never follows a symlink out of an
// owned root.
package workspace
