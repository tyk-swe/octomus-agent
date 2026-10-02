package notifications

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/model"
	"github.com/tyk-swe/octomus-agent/internal/redact"
	"github.com/tyk-swe/octomus-agent/internal/store"
	"github.com/tyk-swe/octomus-agent/internal/wirejson"
)

const (
	maxPayloadBytes    = 8192
	maxRepositoryBytes = 256
	maxIDBytes         = 128
)

const (
	httpStatusCategory = "http_status"
	invalidPayload     = "invalid_payload"
	timeoutCategory    = "timeout"
	transportCategory  = "transport_error"
)

type attentionEvent struct {
	SchemaVersion uint32  `json:"schema_version"`
	EventID       string  `json:"event_id"`
	OccurredAt    string  `json:"occurred_at"`
	Repository    string  `json:"repository"`
	CycleID       *string `json:"cycle_id"`
	RunID         *string `json:"run_id"`
	TaskID        *string `json:"task_id"`
	Category      string  `json:"category"`
	Action        string  `json:"action"`
}

func payload(delivery *store.NotificationDelivery) ([]byte, error) {
	event := attentionEvent{
		SchemaVersion: 1,
		EventID:       delivery.EventID,
		OccurredAt:    delivery.CreatedAt,
		Repository:    delivery.Repository,
		CycleID:       delivery.CycleID,
		RunID:         delivery.RunID,
		TaskID:        delivery.TaskID,
		Category:      delivery.Category,
		Action:        delivery.Action,
	}
	if len(event.Repository) > maxRepositoryBytes ||
		len(event.EventID) > maxIDBytes ||
		len(deref(event.CycleID)) > maxIDBytes ||
		len(deref(event.RunID)) > maxIDBytes ||
		len(deref(event.TaskID)) > maxIDBytes ||
		len(event.Category) > maxIDBytes ||
		len(event.Action) > maxIDBytes {
		return nil, errors.New(invalidPayload)
	}
	encoded, err := wirejson.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", invalidPayload, err)
	}
	if len(encoded) > maxPayloadBytes {
		return nil, errors.New(invalidPayload)
	}
	return encoded, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func retryable(status uint16) bool {
	return status == 408 || status == 429 || status >= 500
}

type Worker struct {
	store       *store.Store
	url         string
	destID      string
	client      *http.Client
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	shutdown    sync.Once
	warnings    io.Writer
	lastWarning string
}

// webhookClient follows no redirects and ignores proxy environment variables.
func webhookClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	return &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func Start(parent context.Context, db *store.Store, configuredURL string) (*Worker, error) {
	return start(parent, db, configuredURL, os.Stderr)
}

func start(parent context.Context, db *store.Store, configuredURL string, warnings io.Writer) (*Worker, error) {
	raw := strings.TrimSpace(configuredURL)
	var normalized, destinationID string
	state := "disabled"
	var destination, errorText *string
	if raw != "" {
		destinationURL, id, err := model.NotificationDestination(raw)
		if err != nil {
			state = "invalid"
			message := err.Error()
			errorText = &message
		} else {
			normalized, destinationID = destinationURL, id
			state = "enabled"
			destination = &destinationID
		}
	}
	if err := db.ConfigureNotifications(destination, state, errorText); err != nil {
		return nil, err
	}
	if state != "enabled" {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(parent)
	worker := &Worker{
		store:    db,
		url:      normalized,
		destID:   destinationID,
		client:   webhookClient(),
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
		warnings: warnings,
	}
	go worker.run()
	return worker, nil
}

func (w *Worker) Stop() {
	w.shutdown.Do(func() {
		w.cancel()
		<-w.done
	})
}

func (w *Worker) run() {
	defer close(w.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-timer.C:
		}
		if w.ctx.Err() != nil {
			return
		}
		w.deliverNext()
		// A slow claim, delivery or write must not leave an expired timer that
		// permits the next request immediately after this attempt finishes.
		timer.Reset(time.Second)
	}
}

func (w *Worker) deliverNext() {
	delivery, err := w.store.ClaimNotification(w.destID, time.Now().UTC())
	if err != nil {
		w.warn(err)
		return
	}
	w.lastWarning = ""
	if delivery == nil {
		return
	}
	status, category := w.deliver(delivery)
	if w.ctx.Err() != nil {
		return
	}
	switch {
	case category != "":
		w.warn(w.store.FinishNotificationFailure(delivery.Seq, category, nil, category != invalidPayload))
	case status >= 200 && status < 300:
		w.warn(w.store.FinishNotificationDelivered(delivery.Seq, time.Now().UTC()))
	default:
		w.warn(w.store.FinishNotificationFailure(delivery.Seq, httpStatusCategory, &status, retryable(status)))
	}
}

func (w *Worker) warn(err error) {
	if err == nil {
		return
	}
	message := redact.Error(err)
	if message == w.lastWarning {
		return
	}
	w.lastWarning = message
	fmt.Fprintf(w.warnings, "WARN notifications: %s\n", message)
}

func (w *Worker) deliver(delivery *store.NotificationDelivery) (uint16, string) {
	body, err := payload(delivery)
	if err != nil {
		return 0, invalidPayload
	}
	req, err := http.NewRequestWithContext(w.ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return 0, transportCategory
	}
	req.Header.Set("content-type", "application/json")
	response, err := w.client.Do(req)
	if err != nil {
		if isTimeout(err) {
			return 0, timeoutCategory
		}
		return 0, transportCategory
	}
	defer response.Body.Close()
	return uint16(response.StatusCode), ""
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Timeout()
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
