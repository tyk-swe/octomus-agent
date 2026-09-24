package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// These two components meet only at the command's service boundary. Recovery
// belongs here, before a listener exists; Run only schedules recovered state.
type serviceScheduler interface {
	Recover() error
	Run(context.Context) error
	Shutdown()
	Drained() bool
}

type serviceHTTPServer interface {
	Serve(net.Listener) error
	Shutdown(context.Context) error
	Close() error
}

type serviceComponents struct {
	scheduler   serviceScheduler
	http        serviceHTTPServer
	startWorker func() (stop func(), err error)
	listen      func(network, address string) (net.Listener, error)
}

func (c serviceComponents) run(ctx context.Context, address string, stderr io.Writer) error {
	// A recovery failure is a startup failure. In particular, /healthz is never
	// exposed for a scheduler that cannot read its durable state.
	if err := c.scheduler.Recover(); err != nil {
		c.scheduler.Shutdown()
		return err
	}
	stopWorker := func() {}
	if c.startWorker != nil {
		stop, err := c.startWorker()
		if err != nil {
			c.scheduler.Shutdown()
			return err
		}
		if stop != nil {
			stopWorker = stop
		}
	}
	listen := c.listen
	if listen == nil {
		listen = net.Listen
	}
	listener, err := listen("tcp", address)
	if err != nil {
		c.scheduler.Shutdown()
		stopWorker()
		return err
	}
	fmt.Fprintf(stderr, "Octomus listening on http://%s\n", listener.Addr())
	// The service owns the scheduler's lifetime. A server failure must stop a
	// scheduler waiting on its run context, even when the signal context is
	// still active.
	serviceCtx, cancelService := context.WithCancel(ctx)
	defer cancelService()
	runDone := make(chan error, 1)
	serveDone := make(chan error, 1)
	go func() { runDone <- c.scheduler.Run(serviceCtx) }()
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

	// Stop accepting health checks as soon as either core loop has exited.
	// Shutdown can wait for an in-flight handler, so let scheduler cancellation
	// progress alongside that graceful HTTP drain.
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
	c.scheduler.Shutdown()
	<-httpStopped
	if !runFinished {
		<-runDone
	}
	if !serveFinished {
		<-serveDone
	}
	stopWorker()
	// Bounded drain: every worker already stopped; this only waits for any
	// straggling handles the runtime still tracks.
	for range 100 {
		if c.scheduler.Drained() {
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
