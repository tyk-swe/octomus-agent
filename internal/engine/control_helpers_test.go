package engine

func (a *App) Resume() error {
	_, err := a.ControlAction("resume")
	return err
}

func (a *App) RunOnce() error {
	_, err := a.ControlAction("cycle")
	return err
}
