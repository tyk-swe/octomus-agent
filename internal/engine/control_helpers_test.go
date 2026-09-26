package engine

// Test shorthands for the operator controls. Each goes through ControlAction,
// the one path the authenticated API uses, so tests that pause, resume or run
// once exercise exactly what an operator's request runs.

// Pause is ControlAction("pause").
func (a *App) Pause() error {
	_, err := a.ControlAction("pause")
	return err
}

// Resume is ControlAction("resume").
func (a *App) Resume() error {
	_, err := a.ControlAction("resume")
	return err
}

// RunOnce is ControlAction("cycle").
func (a *App) RunOnce() error {
	_, err := a.ControlAction("cycle")
	return err
}
