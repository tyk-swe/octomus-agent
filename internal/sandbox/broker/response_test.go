package broker

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIncompleteDockerResultsAreNotEvidence(t *testing.T) {
	for _, response := range []string{`{}`, `null`, `{"StatusCode":null}`, `{"StatusCode":0,"StatusCode":0}`, `{"Error":{"Message":"lost wait"}}`} {
		t.Run("wait "+response, func(t *testing.T) {
			client := newDockerClient("unused")
			client.http.Transport = probeTransport(func(*http.Request) (*http.Response, error) { return lifecycleResponse(http.StatusOK, response), nil })
			results, errs := client.containerWait(context.Background(), "fixture")
			select {
			case result := <-results:
				t.Fatalf("incomplete wait became an exit result: %+v", result)
			case err := <-errs:
				if err == nil {
					t.Fatal("missing wait must fail")
				}
			}
		})
	}
	for _, response := range []string{`{}`, `null`, `{"Id":"fixture","State":{}}`, `{"Id":"fixture","State":{"OOMKilled":null}}`, `{"Id":"fixture","State":null}`} {
		t.Run("inspect "+response, func(t *testing.T) {
			client := newDockerClient("unused")
			client.http.Transport = probeTransport(func(*http.Request) (*http.Response, error) { return lifecycleResponse(http.StatusOK, response), nil })
			if state, err := client.containerInspect(context.Background(), "fixture"); err == nil {
				t.Fatalf("incomplete inspection became known OOM evidence: %+v", state)
			}
		})
	}
}

func TestDockerAndBrokerDiagnosticsRedactProxyCredentials(t *testing.T) {
	const secret = "fixture-proxy-password"
	err := readError(&http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader(`{"message":"refused HTTP_PROXY=http://octomus:` + secret + `@gateway:3128"}`))})
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("daemon response exposed proxy credential: %v", err)
	}
	var logs bytes.Buffer
	b := newBroker(Config{Max: 1, Log: &logs})
	b.logf("Cleanup failed: %v; endpoint http://octomus:%s@gateway:3128", err, secret)
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), "Cleanup failed") {
		t.Fatalf("broker diagnostic was not useful and credential-safe: %s", logs.String())
	}
}
