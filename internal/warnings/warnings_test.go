package warnings

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// newHandler returns a handler printing to a buffer the test reads.
func newHandler(verbose bool) (*Handler, *bytes.Buffer) {
	var out bytes.Buffer
	return &Handler{verbose: verbose, out: &out}, &out
}

func TestHandlerHidesWarningsButCapturesThem(t *testing.T) {
	handler, out := newHandler(false)
	handler.HandleWarningHeader(warningCode, "kube-apiserver", "a duplicate port name")
	handler.HandleWarningHeader(404, "kube-apiserver", "not a warning")
	handler.HandleWarningHeader(warningCode, "kube-apiserver", "")
	if out.Len() != 0 {
		t.Fatalf("quiet handler printed %q", out.String())
	}
	report := handler.attach(errors.New("place failed"))
	if !strings.Contains(report.Error(), "Warning: a duplicate port name") {
		t.Fatalf("captured warning missing from %q", report)
	}
}

func TestHandlerPrintsLiveWhenVerbose(t *testing.T) {
	handler, out := newHandler(true)
	handler.HandleWarningHeader(warningCode, "kube-apiserver", "a deprecated selector")
	if got := out.String(); !strings.Contains(got, "Warning: a deprecated selector") {
		t.Fatalf("verbose handler printed %q", got)
	}
}

func TestAttachQuotesCapturedWarningsInTail(t *testing.T) {
	handler, _ := newHandler(false)
	clean := errors.New("nothing was captured")
	if got := handler.attach(clean); got != clean {
		t.Fatalf("clean error was wrapped: %v", got)
	}
	handler.HandleWarningHeader(warningCode, "kube-apiserver", "a duplicate port name")
	got := handler.attach(errors.New("place failed")).Error()
	for _, want := range []string{"place failed", "api server warnings:", "Warning: a duplicate port name"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report %q misses %q", got, want)
		}
	}
}
