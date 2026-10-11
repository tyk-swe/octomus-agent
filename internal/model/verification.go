package model

// VerificationHistory selects recorded evidence, without authorizing publication
// or merge. Latest follows append order, so a newer failure or wrong-revision
// result supersedes an older success even when its timestamp sorts earlier.
type VerificationHistory struct {
	Latest   *Verification
	Recorded int
}

func (t Task) VerificationFor(command string) VerificationHistory {
	var history VerificationHistory
	for i := range t.Verification {
		if t.Verification[i].Command == command {
			history.Latest = &t.Verification[i]
			history.Recorded++
		}
	}
	return history
}
