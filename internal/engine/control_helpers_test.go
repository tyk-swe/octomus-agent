package engine

func (a *App) Pause() error {
	_, err := a.ControlAction("pause")
	return err
}

func (a *App) Resume() error {
	_, err := a.ControlAction("resume")
	return err
}

func (a *App) RunOnce() error {
	_, err := a.ControlAction("cycle")
	return err
}
