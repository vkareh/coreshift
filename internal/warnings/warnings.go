// Package warnings gives the warnings the API server sends the tool's
// clients the tool's output discipline: captured and hidden by default,
// printed live when the verbose flag is set, and quoted in a failing
// command's error report — the same treatment the image build's output
// gets. The warnings are advisory notes about the objects the tool
// applies; the objects are the operator's to own, so nothing is
// filtered and nothing is patched.
package warnings

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// warningCode is the status code warning headers carry; anything else
// the library surfaces nowhere.
const warningCode = 299

// handler is the process's one warning handler, so a failing command
// can quote what every client of the run captured.
var handler = &Handler{out: os.Stderr}

// SetVerbose turns live printing of warnings on, for a command run
// under the verbose flag. It is called before the first client is
// built.
func SetVerbose(verbose bool) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.verbose = verbose
}

// Config loads the kubeconfig at path into the client configuration the
// tool's API clients run on, whose warnings land in the process's
// handler instead of the client library's raw standard-error lines.
func Config(path string) (*rest.Config, error) {
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		return nil, err
	}
	config.WarningHandler = handler
	return config, nil
}

// Attach returns err with the warnings captured so far quoted in its
// tail, so a failed command still shows what the server tried to say.
// Without captured warnings it returns err unchanged.
func Attach(err error) error {
	return handler.attach(err)
}

// Handler is the client library's warning handler with the tool's
// output discipline: every warning is captured for a failing command's
// report, and printed live only when verbose was asked for.
type Handler struct {
	verbose bool
	out     io.Writer
	mu      sync.Mutex
	caught  strings.Builder
}

// HandleWarningHeader records one warning, skipping what the library's
// own handlers skip: other codes, and empty text.
func (h *Handler) HandleWarningHeader(code int, agent string, text string) {
	if code != warningCode || text == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(&h.caught, "Warning: %s\n", text)
	if h.verbose {
		fmt.Fprintf(h.out, "Warning: %s\n", text)
	}
}

// attach returns err with the captured warnings quoted in its tail, in
// the recent-log-output style the tool's failure reports use.
func (h *Handler) attach(err error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.caught.Len() == 0 {
		return err
	}
	return fmt.Errorf("%w\n\napi server warnings:\n%s", err, h.caught.String())
}
