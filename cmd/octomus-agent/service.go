package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/engine"
)

type serviceComponents struct {
	app           *engine.App
	http          *http.Server
	prepareWorker func() error
	startWorker   func() (stop func(), err error)
}

func (c serviceComponents) run(ctx context.Context, address string, stderr io.Writer) error {
	if c.prepareWorker != nil {
		if err := c.prepareWorker(); err != nil {
			c.app.Shutdown()
			return err
		}
	}
	if err := c.app.Recover(); err != nil {
		c.app.Shutdown()
		return err
	}
	stopWorker := func() {}
	if c.startWorker != nil {
		stop, err := c.startWorker()
		if err != nil {
			c.app.Shutdown()
			return err
		}
		if stop != nil {
			stopWorker = stop
		}
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		c.app.Shutdown()
		stopWorker()
		return err
	}
	fmt.Fprintf(stderr, "Octomus listening on http://%s\n", listener.Addr())
	serviceCtx, cancelService := context.WithCancel(ctx)
	defer cancelService()
	runDone := make(chan error, 1)
	serveDone := make(chan error, 1)
	go func() { runDone <- c.app.Run(serviceCtx) }()
	go func() { serveDone <- c.http.Serve(listener) }()

	var cause error
	var runFinished, serveFinished bool
	select {
	case <-ctx.Done():
	case err := <-runDone:
		runFinished = true
		if ctx.Err() == nil {
			cause = unexpectedServiceExit("scheduler", err)
		}
	case err := <-serveDone:
		serveFinished = true
		if ctx.Err() == nil {
			cause = unexpectedServiceExit("HTTP server", err)
		}
	}

	cancelService()
	_ = listener.Close()
	httpStopped := make(chan struct{})
	go func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.http.Shutdown(shutdownCtx); err != nil {
			_ = c.http.Close()
		}
		close(httpStopped)
	}()
	c.app.Shutdown()
	<-httpStopped
	if !runFinished {
		<-runDone
	}
	if !serveFinished {
		<-serveDone
	}
	stopWorker()
	for range 100 {
		if c.app.Drained() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cause
}

func unexpectedServiceExit(component string, err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s stopped unexpectedly", component)
	}
	return fmt.Errorf("%s stopped: %w", component, err)
}
