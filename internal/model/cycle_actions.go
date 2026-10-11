package model

// CycleActions selects persisted lifecycle eligibility. The service separately
// revalidates worker and cleanup ownership when the operator action arrives.
func CycleActions(status CycleStatus, lifecycle WorkspaceLifecycle) []string {
	if status == CycleRunning || lifecycle.DiscardedAt != nil {
		return []string{}
	}
	if lifecycle.ArchivedAt == nil {
		return []string{"archive"}
	}
	return []string{"discard"}
}
