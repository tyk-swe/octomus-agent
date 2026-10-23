// Package git is the orchestrator's git and GitHub plumbing on the trusted
// checkout: clones, fetches, publication and the guarded maintenance merge.
// Work-tree commands run through WorkGit; workers never push, publish or merge.
package git
