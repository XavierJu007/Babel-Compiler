package main

import (
	"net/http"
	"time"
)

// A configured endpoint is the complete destination. Replaying prompts or keys
// at a provider-supplied Location crosses that boundary, even if Go strips some
// authentication headers. Callers handle the original 3xx response explicitly.
func newOutboundHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
