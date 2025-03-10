/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package prober

import (
	"context"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
)

// Preparer is a way for the caller to modify the HTTP request before it goes out.
type Preparer func(r *http.Request) *http.Request

// Verifier is a way for the caller to validate the HTTP response after it comes back.
type Verifier func(r *http.Response, b []byte) (bool, error)

// WithHeader sets a header in the probe request.
func WithHeader(name, value string) Preparer {
	return func(r *http.Request) *http.Request {
		r.Header.Set(name, value)
		return r
	}
}

// WithHost sets the host in the probe request.
func WithHost(host string) Preparer {
	return func(r *http.Request) *http.Request {
		r.Host = host
		return r
	}
}

// ExpectsBody validates that the body of the probe response matches the provided string.
func ExpectsBody(body string) Verifier {
	return func(r *http.Response, b []byte) (bool, error) {
		return string(b) == body, nil
	}
}

// Do sends a single probe to given target, e.g. `http://revision.default.svc.cluster.local:81`.
// Do returns whether the probe was successful or not, or there was an error probing.
func Do(ctx context.Context, transport http.RoundTripper, target string, ops ...interface{}) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return false, errors.Wrapf(err, "%s is not a valid URL", target)
	}
	for _, op := range ops {
		if po, ok := op.(Preparer); ok {
			req = po(req)
		}
	}

	req = req.WithContext(ctx)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return false, errors.Wrapf(err, "error roundtripping %s", target)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return false, errors.Wrap(err, "error reading body")
	}

	for _, op := range ops {
		if vo, ok := op.(Verifier); ok {
			ok, err := vo(resp, body)
			if err != nil || !ok {
				return false, err
			}
		}
	}
	return resp.StatusCode == http.StatusOK, nil
}

// Done is a callback that is executed when the async probe has finished.
// `arg` is given by the caller at the offering time, while `success` and `err`
// are the return values of the `Do` call.
// It is assumed that the opaque arg is consistent for a given target and
// we will coalesce concurrent Offer invocations on target.
type Done func(arg interface{}, success bool, err error)

// Manager manages async probes and makes sure we run concurrently only a single
// probe for the same key.
type Manager struct {
	cb Done
	// NB: it is paramount to use a transport that will close the connection
	// after every request here. Otherwise the cached connections will prohibit
	// scaling to zero, due to unsuccessful probes to the Activator.
	transport http.RoundTripper

	// mu guards keys.
	mu   sync.Mutex
	keys sets.String
}

// New creates a new Manager, that will invoke the given callback when
// async probing is finished.
func New(cb Done, transport http.RoundTripper) *Manager {
	return &Manager{
		keys:      sets.NewString(),
		cb:        cb,
		transport: transport,
	}
}

// Offer executes asynchronous probe using `target` as the key.
// If a probe with the same key already exists, Offer will return false and the
// call is discarded. If the request is accepted, Offer returns true.
// Otherwise Offer starts a goroutine that periodically executes
// `Do`, until timeout is reached, the probe succeeds, or fails with an error.
// In the end the callback is invoked with the provided `arg` and probing results.
func (m *Manager) Offer(ctx context.Context, target string, arg interface{}, period, timeout time.Duration, ops ...interface{}) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.keys.Has(target) {
		return false
	}
	m.keys.Insert(target)
	m.doAsync(ctx, target, arg, period, timeout, ops...)
	return true
}

// doAsync starts a go routine that probes the target with given period.
func (m *Manager) doAsync(ctx context.Context, target string, arg interface{}, period, timeout time.Duration, ops ...interface{}) {
	go func() {
		defer func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.keys.Delete(target)
		}()
		var (
			result bool
			err    error
		)

		err = wait.PollImmediate(period, timeout, func() (bool, error) {
			result, err = Do(ctx, m.transport, target, ops...)
			return result, err
		})
		m.cb(arg, result, err)
	}()
}
