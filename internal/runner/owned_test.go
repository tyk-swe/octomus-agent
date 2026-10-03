package runner

import (
	"os"
	"testing"
)

const (
	cutPhraseEnv = "RUNNER_TEST_PASSWORD"
	cutPhrase    = "correct horse battery staple"
	cutLinesEnv  = "RUNNER_TEST_SECRET"
	cutLines     = "first-line-of-key\nsecond-line-of-key\nthird-line"
)

func TestMain(m *testing.M) {
	for name, value := range map[string]string{cutPhraseEnv: cutPhrase, cutLinesEnv: cutLines} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
