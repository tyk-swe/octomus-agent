// Package model holds the durable records Octomus saves: tasks, cycles,
// proposals, pull requests and the control state, with the action, event
// and lifecycle contracts over them. Records decode strictly through
// wirejson, so a saved record either loads whole or fails visibly.
package model
