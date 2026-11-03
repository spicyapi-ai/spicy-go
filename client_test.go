package spicy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Each of these tests pins a property that can be disproved: delete the corresponding fix and it
// must go red. They assert externally observable behaviour - error text, error identity, headers
// that went out, poll counts - rather than implementation details, so rewriting the implementation
// will not turn them red for no reason.

// -- 1. A presigned upload URL must never reach error text ------------------

// These two values appear only in the ticket URL's query string. Finding either in any error text
// is a leak.
const (
	testUploadSignature  = "DEADBEEFcafef00d0123456789abcdef0123456789abcdef0123456789abcdef"
	testUploadCredential = "AKIAIOSFODNN7EXAMPLE%2F20260920%2Fauto%2Fs3%2Faws4_request"
)

// bodyOf wraps a JSON string as a readable response body.
func bodyOf(payload string) io.ReadCloser {
	return io.NopCloser(strings.NewReader(payload))
}

// TestUploadFailureKeepsThePresignedURLOutOfItsMessage proves that a failed upload does not print
// an object-storage write authorisation into the error.
//
// Why this deserves its own test: that URL is a write credential valid for roughly 20 minutes, with
// the signature and access key in the query string, and the *url.Error returned when
// http.Client.Do fails prints the whole thing by default. That line usually goes straight into the
// application log, so the credential's lifetime becomes the log's retention period. The failure
// emits no signal at all: an upload error is about as ordinary as log entries get.
func TestUploadFailureKeepsThePresignedURLOutOfItsMessage(t *testing.T) {
	// A listener closed immediately: its address is guaranteed to be unreachable, which reliably
	// produces a *url.Error.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	presigned := deadURL + "/spicy-uploads/fil_abc123.png" +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=" + testUploadCredential +
		"&X-Amz-Date=20260920T000000Z&X-Amz-Expires=1200&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=" + testUploadSignature

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":200,"msg":"ok","request_id":"req_1","data":{
			"fileId":"fil_abc123","uploadUrl":%q,"method":"PUT",
			"headers":{"Content-Type":"image/png"},
			"expiresAt":"2030-01-01T00:00:00Z","maxBytes":10485760}}`, presigned)
	}))
	defer api.Close()

	client, err := New(Options{APIKey: "sk-test", BaseURL: api.URL})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	_, err = client.Upload(context.Background(), []byte("not really a png"), "image/png")
	if err == nil {
		t.Fatal("uploading to a dead address should fail")
	}
	message := err.Error()

	for _, secret := range []string{
		testUploadSignature,
		testUploadCredential,
		"X-Amz-Signature",
		"X-Amz-Credential",
		presigned,
	} {
		if strings.Contains(message, secret) {
			t.Errorf("the upload error leaks %q.\n  message: %s", secret, message)
		}
	}

	// The counter-assertion: the check above must not be satisfied by emptying the error. It still
	// has to say what failed, or a timeout, a refused connection and a TLS handshake failure all
	// look identical in production.
	if !strings.Contains(message, "uploading to object storage") {
		t.Errorf("the upload error stopped saying what failed: %s", message)
	}
	if !strings.Contains(message, "connection refused") {
		t.Errorf("the upload error stopped saying why it failed: %s", message)
	}

	// The cause must stay in the error chain, or callers cannot tell "the upload timed out" from
	// "the upload was refused".
	if errors.Unwrap(err) == nil {
		t.Error("the upload error has no cause left in its chain")
	}
}

// TestRedactedUploadErrorStillReportsATimeout pins that redaction did not sever the error chain:
// when a ten-minute upload budget runs out, the caller must still recognise a timeout via
// errors.Is.
func TestRedactedUploadErrorStillReportsATimeout(t *testing.T) {
	signed := "https://storage.example/put?X-Amz-Signature=" + testUploadSignature
	wrapped := withoutSignedURL(&urlErrorStub{op: "Put", url: signed, err: context.DeadlineExceeded}, signed)

	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Errorf("redaction broke errors.Is for a timeout: %v", wrapped)
	}
	if strings.Contains(wrapped.Error(), testUploadSignature) {
		t.Errorf("redaction let the signature through: %s", wrapped.Error())
	}
}

// urlErrorStub reproduces how *url.Error renders, so the test above need not open a real
// connection.
type urlErrorStub struct {
	op  string
	url string
	err error
}

func (e *urlErrorStub) Error() string {
	return e.op + " " + strconv.Quote(e.url) + ": " + e.err.Error()
}
func (e *urlErrorStub) Unwrap() error { return e.err }

// -- 2. Verify the signature first, judge freshness second ------------------

func signWebhook(t *testing.T, secret, taskID, timestamp string, body []byte) string {
	t.Helper()
	digest := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(taskID + "." + timestamp + "." + hex.EncodeToString(digest[:])))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func webhookHeader(version, timestamp, signature string) http.Header {
	header := http.Header{}
	header.Set("X-Webhook-Payload-Version", version)
	header.Set("X-Webhook-Timestamp", timestamp)
	header.Set("X-Webhook-Signature", signature)
	return header
}

// TestWebhookRejectsABadSignatureBeforeCallingItStale proves that a delivery which is both forged
// and stale is reported as a signature failure rather than as staleness.
//
// Neither order would let the delivery through, so this is not about detection. The damage is that
// the alert points the wrong way: a forger writes their own timestamp, and an old one gets reported
// as "stale". Faced with a screen full of staleness, the reasonable operational response is to
// widen the tolerance - which is precisely the knob that moves those deliveries from rejected to
// accepted. An attack thus disguises itself as a clock problem and then gets waved through by us.
func TestWebhookRejectsABadSignatureBeforeCallingItStale(t *testing.T) {
	const secret = "whsec_test"
	body := []byte(`{"task_id":"task_real","state":"succeeded"}`)
	stale := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)

	header := webhookHeader("1", stale, "dGhpcyBpcyBub3QgYSByZWFsIHNpZ25hdHVyZQ==")
	_, err := VerifyWebhook(secret, header, body, 5*time.Minute)

	if !errors.Is(err, ErrWebhookSignature) {
		t.Errorf("a forged delivery must be reported as a signature failure, got %v", err)
	}
	if errors.Is(err, ErrWebhookStale) {
		t.Errorf("a forged delivery was reported as merely stale, which is the alert that "+
			"makes an operator widen the tolerance window: %v", err)
	}
}

// TestWebhookStillRejectsAnAuthenticButStaleDelivery is the converse: reordering the checks is not
// the same as deleting the freshness check. A valid signature with an old timestamp is still stale,
// and replay protection remains in force.
func TestWebhookStillRejectsAnAuthenticButStaleDelivery(t *testing.T) {
	const secret = "whsec_test"
	body := []byte(`{"task_id":"task_real","state":"succeeded"}`)
	stale := strconv.FormatInt(time.Now().Add(-2*time.Hour).Unix(), 10)

	header := webhookHeader("1", stale, signWebhook(t, secret, "task_real", stale, body))
	_, err := VerifyWebhook(secret, header, body, 5*time.Minute)

	if !errors.Is(err, ErrWebhookStale) {
		t.Errorf("a genuine but old delivery must still be rejected as stale, got %v", err)
	}
}

// TestWebhookAcceptsAFreshAuthenticDelivery holds the floor: both checks still do something.
func TestWebhookAcceptsAFreshAuthenticDelivery(t *testing.T) {
	const secret = "whsec_test"
	body := []byte(`{"task_id":"task_real","state":"succeeded"}`)
	now := strconv.FormatInt(time.Now().Unix(), 10)

	header := webhookHeader("1", now, signWebhook(t, secret, "task_real", now, body))
	taskID, err := VerifyWebhook(secret, header, body, 5*time.Minute)

	if err != nil {
		t.Fatalf("a fresh authentic delivery must verify: %v", err)
	}
	if taskID != "task_real" {
		t.Errorf("verified task id = %q, want task_real", taskID)
	}
}

// -- 3. Every request identifies its version --------------------------------

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// TestEveryRequestIdentifiesItselfWithAVersionedUserAgent proves requests carry a version.
//
// Without it net/http sends Go-http-client/1.1, which says only that the caller is a Go program.
// A question like "which clients are affected by this bug" then turns from a log query into a
// guess.
func TestEveryRequestIdentifiesItselfWithAVersionedUserAgent(t *testing.T) {
	var seen string
	client, err := New(Options{
		APIKey:  "sk-test",
		BaseURL: "https://api.example.invalid/api/v1",
		HTTPClient: doerFunc(func(req *http.Request) (*http.Response, error) {
			seen = req.Header.Get("User-Agent")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       bodyOf(`{"code":200,"msg":"ok","request_id":"r","data":{"taskId":"t"}}`),
				Request:    req,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	if _, err := client.GetTask(context.Background(), "task_1"); err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	if seen == "" {
		t.Fatal("the request sent no User-Agent, so net/http substituted Go-http-client/1.1")
	}
	if strings.HasPrefix(seen, "Go-http-client") {
		t.Fatalf("the request went out as the net/http default %q", seen)
	}
	if want := "SpicyAPI-Go/" + Version; seen != want {
		t.Errorf("User-Agent = %q, want %q", seen, want)
	}
	// The version has to be a real version: an empty string would satisfy the equality above too.
	if strings.TrimSpace(Version) == "" || !strings.Contains(seen, ".") {
		t.Errorf("User-Agent %q carries no version number", seen)
	}
}

// TestCallerHeadersStillWin pins that the User-Agent is a default rather than a hard-coded value:
// anyone embedding this client in their own SDK must be able to replace it.
func TestCallerHeadersStillWin(t *testing.T) {
	var seen string
	client, err := New(Options{
		APIKey:  "sk-test",
		BaseURL: "https://api.example.invalid/api/v1",
		HTTPClient: doerFunc(func(req *http.Request) (*http.Response, error) {
			seen = req.Header.Get("Idempotency-Key")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       bodyOf(`{"code":200,"msg":"ok","request_id":"r","data":{"taskId":"t","state":"queued"}}`),
				Request:    req,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	_, err = client.CreateTask(context.Background(), CreateTaskInput{
		Model: "m", Input: map[string]any{"prompt": "p"},
	}, "spicy-deadbeef", CreateTaskOptions{})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if seen != "spicy-deadbeef" {
		t.Errorf("Idempotency-Key = %q, want spicy-deadbeef", seen)
	}
}

// -- 5. One polling timeout does not end the whole wait ---------------------

// hangingDoer simulates a server that accepts a request and never answers: every attempt stalls
// until its own request timeout.
type hangingDoer struct{ calls atomic.Int64 }

func (d *hangingDoer) Do(req *http.Request) (*http.Response, error) {
	d.calls.Add(1)
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// TestWaitForTerminalKeepsPollingWhenOnePollTimesOut proves that one polling timeout does not kill
// the whole wait, and that the error reported when the budget genuinely runs out carries the taskID.
//
// Both halves matter. A ten-minute budget must not be abandoned because the first attempt stalled
// for thirty seconds - the task is still running and still being billed. And the error has to be a
// *WaitTimeoutError: the documentation tells callers that a local timeout is not a verdict and to
// keep the task ID for reconciliation, yet a bare transport error has no TaskID field, losing the
// id at the very moment it is most needed.
func TestWaitForTerminalKeepsPollingWhenOnePollTimesOut(t *testing.T) {
	doer := &hangingDoer{}
	client, err := New(Options{
		APIKey:     "sk-test",
		BaseURL:    "https://api.example.invalid/api/v1",
		HTTPClient: doer,
		// Compressed timings so a wait with a realistic budget finishes within half a second.
		// The semantics are unchanged: every attempt fails on its own request timeout, while the
		// overall budget far exceeds a single attempt.
		RequestTimeout: 20 * time.Millisecond,
		MaxRetries:     -1, // disable do()'s own retries so the poll count reflects only this loop
	})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	started := time.Now()
	_, waitErr := client.WaitForTerminal(context.Background(), "task_abc123", WaitOptions{
		Timeout:     400 * time.Millisecond,
		Interval:    5 * time.Millisecond,
		MaxInterval: 5 * time.Millisecond,
	})
	elapsed := time.Since(started)

	if polls := doer.calls.Load(); polls < 4 {
		t.Errorf("only %d poll(s) in a 400ms budget of 20ms requests: one timed-out poll "+
			"ended the whole wait", polls)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("the wait gave up after %v of a 400ms budget", elapsed.Round(time.Millisecond))
	}

	var timeout *WaitTimeoutError
	if !errors.As(waitErr, &timeout) {
		t.Fatalf("a spent wait budget must report *WaitTimeoutError, got %T: %v", waitErr, waitErr)
	}
	if timeout.TaskID != "task_abc123" {
		t.Errorf("the wait timeout lost the task id: %+v", timeout)
	}
}

// TestWaitForTerminalStopsOnAnAnswerFromTheServer is the converse: only timeouts may be swallowed.
// A verdict from the server - a 404 here - goes straight back to the caller; polling an explicit
// rejection over and over would merely stretch it into ten minutes of silence.
func TestWaitForTerminalStopsOnAnAnswerFromTheServer(t *testing.T) {
	var polls atomic.Int64
	client, err := New(Options{
		APIKey:  "sk-test",
		BaseURL: "https://api.example.invalid/api/v1",
		HTTPClient: doerFunc(func(req *http.Request) (*http.Response, error) {
			polls.Add(1)
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       bodyOf(`{"code":40401,"msg":"task not found","request_id":"r","data":null}`),
				Request:    req,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	_, waitErr := client.WaitForTerminal(context.Background(), "task_gone", WaitOptions{
		Timeout:     2 * time.Second,
		Interval:    5 * time.Millisecond,
		MaxInterval: 5 * time.Millisecond,
	})

	var apiErr *APIError
	if !errors.As(waitErr, &apiErr) {
		t.Fatalf("a 404 must end the wait as *APIError, got %T: %v", waitErr, waitErr)
	}
	if got := polls.Load(); got != 1 {
		t.Errorf("a 404 was polled %d times; an answer from the server is not a blip", got)
	}
}

// TestWaitForTerminalReturnsTheCallersOwnCancellation guards the third case against being absorbed
// by the two above: when the caller cancels, what comes back is their own error, not our local
// timeout.
func TestWaitForTerminalReturnsTheCallersOwnCancellation(t *testing.T) {
	doer := &hangingDoer{}
	client, err := New(Options{
		APIKey:         "sk-test",
		BaseURL:        "https://api.example.invalid/api/v1",
		HTTPClient:     doer,
		RequestTimeout: time.Second,
		MaxRetries:     -1,
	})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, waitErr := client.WaitForTerminal(ctx, "task_abc123", WaitOptions{Timeout: 10 * time.Second})

	if !errors.Is(waitErr, context.Canceled) {
		t.Errorf("a cancelled caller must get context.Canceled back, got %v", waitErr)
	}
	var timeout *WaitTimeoutError
	if errors.As(waitErr, &timeout) {
		t.Errorf("a cancelled caller was told our local budget ran out: %v", waitErr)
	}
}

// -- Newer endpoints: /chat/credit, /usage, /jobs ---------------------------

// recordingAPI stands up a fake API that records each method and path it receives (query string
// included) and replays the supplied response bodies in order.
type recordingAPI struct {
	server    *httptest.Server
	mu        sync.Mutex
	requests  []string
	responses []string
}

func newRecordingAPI(t *testing.T, responses ...string) *recordingAPI {
	t.Helper()
	api := &recordingAPI{responses: responses}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		api.requests = append(api.requests, r.Method+" "+r.URL.RequestURI())
		body := `{"code":200,"msg":"success","request_id":"r","data":{}}`
		if n := len(api.requests) - 1; n < len(api.responses) {
			body = api.responses[n]
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *recordingAPI) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func (a *recordingAPI) client(t *testing.T) *Client {
	t.Helper()
	client, err := New(Options{APIKey: "sk-test", BaseURL: a.server.URL})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	return client
}

// businessFailureAPI answers HTTP 200 with an envelope whose code is not 200.
//
// This is the easiest thing on the platform to get wrong: transport success is not call success.
// Treated as success, the caller receives a zero-value struct - an empty balance, zero usage, an
// empty task list - and every one of those zero values is a plausible-looking answer. Hence one
// test for each of the three newer endpoints.
func businessFailureAPI(t *testing.T, code int, msg string) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":%d,"msg":%q,"request_id":"req_x","data":null}`, code, msg)
	}))
	t.Cleanup(server.Close)
	client, err := New(Options{APIKey: "sk-test", BaseURL: server.URL, MaxRetries: -1})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	return client
}

func TestGetBalance(t *testing.T) {
	api := newRecordingAPI(t, `{"code":200,"msg":"success","request_id":"r","data":{
		"available":"12.3456","held":"0.5000","total":"12.8456",
		"funding":{"balanceUsd":"10.0000","heldUsd":"0.5000",
			"prepaidAvailableUsd":"10.0000","grantAvailableUsd":"2.3456",
			"cashShortfallUsd":"0","grantsHasMore":false,
			"credit":{"enabled":true,"limitUsd":"100.0000","availableUsd":"100.0000",
				"usedUsd":"0","heldUsd":"0","status":"active","version":3,"expiresAt":null},
			"grants":[{"id":"gr_1","name":"Launch credit","amountUsd":"5.0000",
				"availableUsd":"2.3456","heldUsd":"0","spentUsd":"2.6544","status":"active",
				"startsAt":"2026-09-01T00:00:00Z","expiresAt":null,
				"modelSlugs":[],"customerMemo":"welcome","createdAt":"2026-09-01T00:00:00Z"}]}}}`)

	balance, err := api.client(t).GetBalance(context.Background())
	if err != nil {
		t.Fatalf("GetBalance: %v", err)
	}

	if got := api.seen(); len(got) != 1 || got[0] != "GET /chat/credit" {
		t.Errorf("requests = %v, want one GET /chat/credit", got)
	}
	if balance.Available != "12.3456" || balance.Held != "0.5000" || balance.Total != "12.8456" {
		t.Errorf("balance = %+v", balance)
	}
	if balance.Funding == nil {
		t.Fatal("the funding breakdown was dropped")
	}
	if balance.Funding.GrantAvailableUsd != "2.3456" {
		t.Errorf("grantAvailableUsd = %q", balance.Funding.GrantAvailableUsd)
	}
	if balance.Funding.Credit == nil || balance.Funding.Credit.LimitUsd != "100.0000" {
		t.Errorf("credit facility = %+v", balance.Funding.Credit)
	}
	if balance.Funding.Credit.Version != 3 {
		t.Errorf("credit version = %d, want 3", balance.Funding.Credit.Version)
	}
	if len(balance.Funding.Grants) != 1 || balance.Funding.Grants[0].ID != "gr_1" {
		t.Fatalf("grants = %+v", balance.Funding.Grants)
	}
	// An empty modelSlugs means "every model", not "no models". After decoding it must still be
	// empty rather than dropped as a missing field - callers rely on len()==0 to tell them this.
	if got := balance.Funding.Grants[0].ModelSlugs; len(got) != 0 {
		t.Errorf("modelSlugs = %v, want empty (which means every model)", got)
	}
	if balance.Funding.Grants[0].ExpiresAt != nil {
		t.Errorf("a null expiresAt must decode to nil, got %v", balance.Funding.Grants[0].ExpiresAt)
	}
}

func TestGetBalanceDoesNotTreatABusinessFailureAsSuccess(t *testing.T) {
	_, err := businessFailureAPI(t, 40301, "key cannot read balances").
		GetBalance(context.Background())

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("HTTP 200 with a non-200 business code must be an *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != 40301 {
		t.Errorf("code = %d, want 40301", apiErr.Code)
	}
	if apiErr.RequestID != "req_x" {
		t.Errorf("request id = %q, want req_x", apiErr.RequestID)
	}
}

func TestGetUsage(t *testing.T) {
	api := newRecordingAPI(t, `{"code":200,"msg":"success","request_id":"r","data":{
		"from":"2026-09-01","to":"2026-09-08","currency":"USD",
		"totalCalls":42,"totalSpend":"3.1400",
		"days":[{"day":"2026-09-01","calls":2,"succeeded":1,"failed":1,"spend":"0.2000"}],
		"models":[{"model":"bytedance/seedance-2.5/text-to-video",
			"calls":40,"succeeded":40,"failed":0,"spend":"2.9400"}]}}`)

	usage, err := api.client(t).GetUsage(context.Background(), UsageOptions{
		From: "2026-09-01", To: "2026-09-08",
	})
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}

	if got := api.seen(); len(got) != 1 || got[0] != "GET /usage?from=2026-09-01&to=2026-09-08" {
		t.Errorf("requests = %v", got)
	}
	if usage.TotalCalls != 42 || usage.TotalSpend != "3.1400" || usage.Currency != "USD" {
		t.Errorf("usage = %+v", usage)
	}
	// The window the server chose must be readable: with From and To unset it picks its own, and
	// this is the only place that says what it picked.
	if usage.From != "2026-09-01" || usage.To != "2026-09-08" {
		t.Errorf("window = [%s,%s)", usage.From, usage.To)
	}
	if len(usage.Days) != 1 || usage.Days[0].Failed != 1 {
		t.Errorf("days = %+v", usage.Days)
	}
	if len(usage.Models) != 1 || usage.Models[0].Spend != "2.9400" {
		t.Errorf("models = %+v", usage.Models)
	}
}

// TestGetUsageOmitsEmptyDates pins that "unset" does not become an empty parameter - the contract
// states plainly that empty query parameters are rejected.
func TestGetUsageOmitsEmptyDates(t *testing.T) {
	api := newRecordingAPI(t)
	if _, err := api.client(t).GetUsage(context.Background(), UsageOptions{}); err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if got := api.seen(); len(got) != 1 || got[0] != "GET /usage" {
		t.Errorf("requests = %v, want a bare GET /usage with no query at all", got)
	}
}

func TestGetUsageDoesNotTreatABusinessFailureAsSuccess(t *testing.T) {
	_, err := businessFailureAPI(t, 40001, "date range exceeds 92 days").
		GetUsage(context.Background(), UsageOptions{From: "2020-01-01", To: "2026-01-01"})

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("HTTP 200 with a non-200 business code must be an *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != 40001 {
		t.Errorf("code = %d, want 40001", apiErr.Code)
	}
}

func TestListTasks(t *testing.T) {
	api := newRecordingAPI(t, `{"code":200,"msg":"success","request_id":"r","data":{
		"items":[{"taskId":"task_1","model":"m","state":"succeeded","cost":"0.1200",
			"settled":true,"createdAt":"2026-09-20T10:00:00Z",
			"deadlineAt":"2026-09-20T10:30:00Z","completedAt":"2026-09-20T10:02:00Z"},
			{"taskId":"task_2","model":"m","state":"running","cost":"0.1200",
			"settled":false,"createdAt":"2026-09-20T11:00:00Z",
			"deadlineAt":"2026-09-20T11:30:00Z"}],
		"hasMore":true,"nextCursor":"cur_page2"}}`)

	list, err := api.client(t).ListTasks(context.Background(), ListTasksOptions{
		From: "2026-09-01", To: "2026-09-21", State: StateSucceeded, Model: "m", Limit: 50,
	})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}

	want := "GET /jobs?from=2026-09-01&limit=50&model=m&state=succeeded&to=2026-09-21"
	if got := api.seen(); len(got) != 1 || got[0] != want {
		t.Errorf("requests = %v\n  want %s", got, want)
	}
	if len(list.Items) != 2 {
		t.Fatalf("items = %+v", list.Items)
	}
	if !list.HasMore || list.NextCursor != "cur_page2" {
		t.Errorf("hasMore = %v, nextCursor = %q", list.HasMore, list.NextCursor)
	}
	if !list.Items[0].Settled || list.Items[0].CompletedAt == nil {
		t.Errorf("settled row = %+v", list.Items[0])
	}
	// An in-flight row has no completedAt. It must be nil rather than a zero time - the zero time
	// is 0001-01-01, which prints like a task completed two millennia ago.
	if list.Items[1].CompletedAt != nil {
		t.Errorf("an in-flight row must have a nil CompletedAt, got %v", list.Items[1].CompletedAt)
	}
}

// TestListTasksOmitsUnsetFilters pins that zero values do not become empty query parameters, and
// that a zero Limit means "unset" rather than "give me none".
func TestListTasksOmitsUnsetFilters(t *testing.T) {
	api := newRecordingAPI(t)
	if _, err := api.client(t).ListTasks(context.Background(), ListTasksOptions{}); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if got := api.seen(); len(got) != 1 || got[0] != "GET /jobs" {
		t.Errorf("requests = %v, want a bare GET /jobs", got)
	}
}

// TestListTasksDoesNotCapTheLimitLocally pins that the ceiling is not enforced locally. The day the
// server relaxes it to 200, this package must not reject a value that already works.
func TestListTasksDoesNotCapTheLimitLocally(t *testing.T) {
	api := newRecordingAPI(t)
	if _, err := api.client(t).ListTasks(context.Background(), ListTasksOptions{Limit: 500}); err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if got := api.seen(); len(got) != 1 || got[0] != "GET /jobs?limit=500" {
		t.Errorf("requests = %v; the limit must reach the server, which is the only "+
			"place that knows the real bound", got)
	}
}

func TestListTasksDoesNotTreatABusinessFailureAsSuccess(t *testing.T) {
	_, err := businessFailureAPI(t, 40001, "unknown query parameter").
		ListTasks(context.Background(), ListTasksOptions{Limit: 20})

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("HTTP 200 with a non-200 business code must be an *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != 40001 {
		t.Errorf("code = %d, want 40001", apiErr.Code)
	}
}

// TestEachTaskRepeatsEveryFilterAndOnlyMovesTheCursor is the heart of the paging semantics: a
// cursor only means anything to the query that issued it, so changing any filter mid-walk returns
// pages that belong to neither query - and nothing reports it.
func TestEachTaskRepeatsEveryFilterAndOnlyMovesTheCursor(t *testing.T) {
	api := newRecordingAPI(t,
		`{"code":200,"msg":"success","request_id":"r","data":{
			"items":[{"taskId":"task_1","model":"m","state":"succeeded","cost":"0.1",
				"settled":true,"createdAt":"2026-09-20T10:00:00Z","deadlineAt":"2026-09-20T10:30:00Z"}],
			"hasMore":true,"nextCursor":"cur_2"}}`,
		`{"code":200,"msg":"success","request_id":"r","data":{
			"items":[{"taskId":"task_2","model":"m","state":"succeeded","cost":"0.1",
				"settled":true,"createdAt":"2026-09-20T09:00:00Z","deadlineAt":"2026-09-20T09:30:00Z"}],
			"hasMore":false}}`,
	)

	var walked []string
	err := api.client(t).EachTask(context.Background(), ListTasksOptions{
		From: "2026-09-01", To: "2026-09-21", State: StateSucceeded, Model: "m", Limit: 1,
	}, func(item TaskListItem) bool {
		walked = append(walked, item.TaskID)
		return true
	})
	if err != nil {
		t.Fatalf("EachTask: %v", err)
	}

	if strings.Join(walked, ",") != "task_1,task_2" {
		t.Errorf("walked %v, want both pages in order", walked)
	}
	base := "from=2026-09-01&limit=1&model=m&state=succeeded&to=2026-09-21"
	want := []string{
		"GET /jobs?" + base,
		"GET /jobs?cursor=cur_2&" + base,
	}
	got := api.seen()
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request #%d = %s\n  want %s", i+1, got[i], want[i])
		}
	}
}

// TestEachTaskStopsWhenTheCallerSaysSo proves that an early exit does not keep paging - every page
// is a real API call.
func TestEachTaskStopsWhenTheCallerSaysSo(t *testing.T) {
	api := newRecordingAPI(t, `{"code":200,"msg":"success","request_id":"r","data":{
		"items":[{"taskId":"task_1","model":"m","state":"queued","cost":"0.1",
			"settled":false,"createdAt":"2026-09-20T10:00:00Z","deadlineAt":"2026-09-20T10:30:00Z"},
			{"taskId":"task_2","model":"m","state":"queued","cost":"0.1",
			"settled":false,"createdAt":"2026-09-20T10:00:00Z","deadlineAt":"2026-09-20T10:30:00Z"}],
		"hasMore":true,"nextCursor":"cur_2"}}`)

	var seen int
	if err := api.client(t).EachTask(context.Background(), ListTasksOptions{},
		func(TaskListItem) bool { seen++; return false }); err != nil {
		t.Fatalf("EachTask: %v", err)
	}
	if seen != 1 {
		t.Errorf("fn ran %d times after returning false once", seen)
	}
	if got := api.seen(); len(got) != 1 {
		t.Errorf("EachTask kept paging after the caller stopped: %v", got)
	}
}

// TestEachTaskStopsOnAMissingCursor pins the || condition: when hasMore says there is more but
// nextCursor is empty, paging must stop. Trusting hasMore alone would page forever - calling the
// API endlessly, which looks like a hang.
func TestEachTaskStopsOnAMissingCursor(t *testing.T) {
	api := newRecordingAPI(t, `{"code":200,"msg":"success","request_id":"r","data":{
		"items":[],"hasMore":true}}`)

	if err := api.client(t).EachTask(context.Background(), ListTasksOptions{},
		func(TaskListItem) bool { return true }); err != nil {
		t.Fatalf("EachTask: %v", err)
	}
	if got := api.seen(); len(got) != 1 {
		t.Errorf("hasMore without a cursor must end the walk, made %d requests", len(got))
	}
}

func TestEachTaskReportsAFailedPage(t *testing.T) {
	err := businessFailureAPI(t, 50301, "no usable deployment").
		EachTask(context.Background(), ListTasksOptions{}, func(TaskListItem) bool { return true })

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("a failed page must surface as *APIError, got %T: %v", err, err)
	}
}

// -- X-Spicy-Retention ------------------------------------------------------

// TestRetentionHeaderIsSentVerbatim pins that the retention is sent verbatim, with no local ceiling.
// Only the server knows the platform limit, and copying it here buries a constant that will expire:
// once the server relaxes it, this package would reject a value the user could have used. Anything
// over the limit is clamped server-side, and the effective value is read back from Task.Retention.
func TestRetentionHeaderIsSentVerbatim(t *testing.T) {
	for _, seconds := range []int{0, 3600, 999999999} {
		var seen string
		client, err := New(Options{
			APIKey:  "sk-test",
			BaseURL: "https://api.example.invalid/api/v1",
			HTTPClient: doerFunc(func(req *http.Request) (*http.Response, error) {
				seen = req.Header.Get("X-Spicy-Retention")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       bodyOf(`{"code":200,"msg":"ok","request_id":"r","data":{"taskId":"t","state":"queued"}}`),
					Request:    req,
				}, nil
			}),
		})
		if err != nil {
			t.Fatalf("build a client: %v", err)
		}

		retention := seconds
		_, err = client.CreateTask(context.Background(), CreateTaskInput{
			Model: "m", Input: map[string]any{"prompt": "p"},
		}, "spicy-deadbeef", CreateTaskOptions{RetentionSeconds: &retention})
		if err != nil {
			t.Fatalf("CreateTask with retention %d: %v", seconds, err)
		}
		if want := strconv.Itoa(seconds); seen != want {
			t.Errorf("X-Spicy-Retention = %q, want %q", seen, want)
		}
	}
}

// TestRetentionHeaderIsAbsentWhenUnset pins that 0 is a meaningful value, so "unset" has to be
// expressed as nil - empty options must not quietly declare "destroy the output immediately".
func TestRetentionHeaderIsAbsentWhenUnset(t *testing.T) {
	sent := true
	client, err := New(Options{
		APIKey:  "sk-test",
		BaseURL: "https://api.example.invalid/api/v1",
		HTTPClient: doerFunc(func(req *http.Request) (*http.Response, error) {
			_, sent = req.Header["X-Spicy-Retention"]
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       bodyOf(`{"code":200,"msg":"ok","request_id":"r","data":{"taskId":"t","state":"queued"}}`),
				Request:    req,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}

	if _, err := client.CreateTask(context.Background(), CreateTaskInput{
		Model: "m", Input: map[string]any{"prompt": "p"},
	}, "spicy-deadbeef", CreateTaskOptions{}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if sent {
		t.Error("an empty CreateTaskOptions sent X-Spicy-Retention; 0 seconds means " +
			"'delete the outputs the moment the task ends', which is not a default")
	}
}

func TestNegativeRetentionIsRejectedLocally(t *testing.T) {
	client, err := New(Options{APIKey: "sk-test", BaseURL: "https://api.example.invalid/api/v1"})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	negative := -1
	_, err = client.CreateTask(context.Background(), CreateTaskInput{
		Model: "m", Input: map[string]any{"prompt": "p"},
	}, "spicy-deadbeef", CreateTaskOptions{RetentionSeconds: &negative})
	if err == nil {
		t.Fatal("a negative retention must be refused before it reaches the network")
	}
}

// -- the wait parameter on createTask ---------------------------------------

// waitAPI stands up a fake createTask: it records the query it received, holds the connection for
// hold, then answers according to status with either a 200 task record or a 202 acceptance receipt.
func waitAPI(t *testing.T, status int, hold time.Duration) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	queries := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RequestURI())
		mu.Unlock()
		if hold > 0 {
			time.Sleep(hold)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			// A 200 body is a complete task record, the same shape recordInfo returns.
			_, _ = w.Write([]byte(`{"code":200,"msg":"success","request_id":"r","data":{
				"taskId":"task_1","model":"m","state":"succeeded","cost":"0.1200","settled":true,
				"createdAt":"2026-09-20T10:00:00Z","completedAt":"2026-09-20T10:00:05Z",
				"output":{"assets":[{"key":"a","url":"https://cdn.example/a.mp4"}]}}}`))
			return
		}
		// A 202 body is an acceptance receipt with a quite different shape: it has estimatedCost
		// and no cost.
		_, _ = w.Write([]byte(`{"code":200,"msg":"success","request_id":"r","data":{
			"taskId":"task_1","state":"queued","estimatedCost":"0.1200",
			"deadlineAt":"2026-09-20T10:30:00Z"}}`))
	}))
	t.Cleanup(server.Close)
	return server, &queries
}

func waitClient(t *testing.T, server *httptest.Server, requestTimeout time.Duration, maxRetries int) *Client {
	t.Helper()
	client, err := New(Options{
		APIKey: "sk-test", BaseURL: server.URL,
		RequestTimeout: requestTimeout, MaxRetries: maxRetries,
	})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	return client
}

func createWith(t *testing.T, client *Client, opts CreateTaskOptions) (*CreateTaskResult, error) {
	t.Helper()
	return client.CreateTask(context.Background(), CreateTaskInput{
		Model: "m", Input: map[string]any{"prompt": "p"},
	}, "spicy-deadbeef", opts)
}

// TestWaitSecondsIsSentVerbatim pins that the value goes out verbatim, with no local clamping. The
// server clamps anything over the limit to 60 and ignores non-positive integers rather than
// rejecting them - the contract's reasoning being that a typo in an optimisation parameter should
// not fail a generation that would otherwise have succeeded. Clamping again in the client would
// revoke that leniency.
func TestWaitSecondsIsSentVerbatim(t *testing.T) {
	for _, seconds := range []int{0, 5, 60, 3600} {
		server, queries := waitAPI(t, http.StatusAccepted, 0)
		wait := seconds
		if _, err := createWith(t, waitClient(t, server, time.Second, DefaultMaxRetries),
			CreateTaskOptions{WaitSeconds: &wait}); err != nil {
			t.Fatalf("CreateTask with wait=%d: %v", seconds, err)
		}
		want := "/jobs/createTask?wait=" + strconv.Itoa(seconds)
		if got := *queries; len(got) != 1 || got[0] != want {
			t.Errorf("wait=%d produced %v, want %s", seconds, got, want)
		}
	}
}

// TestWaitSecondsIsAbsentWhenUnset pins that leaving it unset omits the parameter entirely - the
// same reasoning as /jobs and /usage: the contract rejects empty query parameters, so "unset" has
// to be expressed as "no such parameter".
func TestWaitSecondsIsAbsentWhenUnset(t *testing.T) {
	server, queries := waitAPI(t, http.StatusAccepted, 0)
	if _, err := createWith(t, waitClient(t, server, time.Second, DefaultMaxRetries), CreateTaskOptions{}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if got := *queries; len(got) != 1 || got[0] != "/jobs/createTask" {
		t.Errorf("requests = %v, want a bare /jobs/createTask with no query at all", got)
	}
}

func TestNegativeWaitSecondsIsRejectedLocally(t *testing.T) {
	server, queries := waitAPI(t, http.StatusAccepted, 0)
	negative := -1
	if _, err := createWith(t, waitClient(t, server, time.Second, DefaultMaxRetries),
		CreateTaskOptions{WaitSeconds: &negative}); err == nil {
		t.Fatal("a negative wait must be refused before it reaches the network")
	}
	if got := *queries; len(got) != 0 {
		t.Errorf("the request went out anyway: %v", got)
	}
}

// TestWaitReturnsTheTerminalRecordOn200 covers the path wait pays for.
func TestWaitReturnsTheTerminalRecordOn200(t *testing.T) {
	server, _ := waitAPI(t, http.StatusOK, 0)
	wait := 30
	result, err := createWith(t, waitClient(t, server, time.Second, DefaultMaxRetries),
		CreateTaskOptions{WaitSeconds: &wait})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if result.Task == nil {
		t.Fatal("a 200 means the task reached a terminal state; Task must carry the record")
	}
	if result.Task.State != StateSucceeded || result.Task.Cost != "0.1200" || !result.Task.Settled {
		t.Errorf("task = %+v", result.Task)
	}
	if assets := result.Task.ReadyAssets(); len(assets) != 1 {
		t.Errorf("the terminal record lost its assets: %+v", assets)
	}
	// The acceptance-receipt fields must be decoded from the same record too, not half filled in.
	if result.TaskID != "task_1" || result.State != StateSucceeded {
		t.Errorf("result = %+v", result)
	}
	if result.InFlight() {
		t.Error("InFlight() is true on a terminal result")
	}
	// estimatedCost does not exist in a task record at all (there it is called cost), so it is
	// necessarily empty. What this pins is that the emptiness is the contract rather than a bug -
	// the real number is on Task.Cost, as documented, and nobody should conclude the quote was
	// lost.
	if result.EstimatedCost != "" {
		t.Errorf("EstimatedCost = %q; a finished task reports a real cost, not an estimate",
			result.EstimatedCost)
	}
}

// TestWaitFallsBackToAcceptanceOn202 covers the easiest thing to get wrong: passing wait does not
// guarantee a terminal record. Once the budget is spent it falls back to an ordinary 202 acceptance
// receipt, with the task still running.
func TestWaitFallsBackToAcceptanceOn202(t *testing.T) {
	server, _ := waitAPI(t, http.StatusAccepted, 0)
	wait := 1
	result, err := createWith(t, waitClient(t, server, time.Second, DefaultMaxRetries),
		CreateTaskOptions{WaitSeconds: &wait})
	if err != nil {
		t.Fatalf("a wait that runs out is the ordinary outcome, not an error: %v", err)
	}

	if result.Task != nil {
		t.Fatal("a 202 means the wait ran out; Task must stay nil so the caller cannot " +
			"mistake an in-flight task for a finished one")
	}
	if result.TaskID != "task_1" || result.State != StateQueued {
		t.Errorf("result = %+v", result)
	}
	if result.EstimatedCost != "0.1200" {
		t.Errorf("EstimatedCost = %q, want the held estimate on the 202 path", result.EstimatedCost)
	}
	if !result.InFlight() {
		t.Error("InFlight() is false on a queued result")
	}
}

// TestWaitLiftsThisRequestsOwnDeadline is the precondition for the parameter being usable at all.
//
// Without it, any wait longer than RequestTimeout (30 seconds by default, against a server limit of
// 60) is bound to time out in our own hands, and then be retried three more times because the call
// is replayable. Measured with compressed timings, it degrades into 4 requests over 3.9 seconds
// ending in "context deadline exceeded" - a phrase that points at the network when the truth is
// that this client hung up on a response that was on its way.
func TestWaitLiftsThisRequestsOwnDeadline(t *testing.T) {
	// The server holds for 200ms while an ordinary request budget is only 50ms - without the lift
	// it is bound to time out on our side.
	server, queries := waitAPI(t, http.StatusOK, 200*time.Millisecond)
	wait := 1
	result, err := createWith(t, waitClient(t, server, 50*time.Millisecond, DefaultMaxRetries),
		CreateTaskOptions{WaitSeconds: &wait})
	if err != nil {
		t.Fatalf("the wait budget did not lift this request's deadline: %v", err)
	}
	if result.Task == nil {
		t.Fatal("the held-open response did not arrive")
	}
	if got := *queries; len(got) != 1 {
		t.Errorf("the request was retried %d times; a deliberate hold is not a fault", len(got))
	}
}

// TestWaitDoesNotChangeTheOrdinaryRequestDeadline is the converse: the lift applies to that one
// call and must not widen the caller's RequestTimeout globally.
func TestWaitDoesNotChangeTheOrdinaryRequestDeadline(t *testing.T) {
	server, _ := waitAPI(t, http.StatusAccepted, 200*time.Millisecond)
	client := waitClient(t, server, 50*time.Millisecond, -1)

	wait := 1
	if _, err := createWith(t, client, CreateTaskOptions{WaitSeconds: &wait}); err != nil {
		t.Fatalf("the waiting call should still succeed: %v", err)
	}
	// A call without wait on the same client still times out at 50ms.
	if _, err := createWith(t, client, CreateTaskOptions{}); err == nil {
		t.Error("a call without WaitSeconds kept the lifted deadline; the override must " +
			"be scoped to the one request that asked for it")
	}
}

// TestWaitKeepsTaskNilWhenA200CarriesAnInFlightRecord holds Task to its promise.
//
// The contract says a 200 is necessarily terminal, so this path should be unreachable. It exists
// because Task means "a complete terminal record" to the outside world, and callers use exactly
// that to decide whether to keep polling: attaching a queued record would hand over an in-flight
// task as a finished one. Leaving it nil merely costs one extra poll.
func TestWaitKeepsTaskNilWhenA200CarriesAnInFlightRecord(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":200,"msg":"success","request_id":"r","data":{
			"taskId":"task_1","model":"m","state":"running","cost":"0.1200",
			"createdAt":"2026-09-20T10:00:00Z"}}`))
	}))
	t.Cleanup(server.Close)

	wait := 30
	result, err := createWith(t, waitClient(t, server, time.Second, DefaultMaxRetries),
		CreateTaskOptions{WaitSeconds: &wait})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if result.Task != nil {
		t.Errorf("a still-running record was published as a terminal one: %+v", result.Task)
	}
	if !result.InFlight() {
		t.Error("InFlight() must still report the task as running")
	}
}

// -- Contract additions of 2026-10: documents, transcripts, layers ----------

// TestTaskRecordDecodesTranscriptAndLayers pins the two result shapes that arrived with
// speech-to-text and layer decomposition. Without typed fields they decode into nothing at all -
// encoding/json drops unknown keys without a word - so the caller sees an empty transcript and a
// pile of PNGs with no way to put them back together.
func TestTaskRecordDecodesTranscriptAndLayers(t *testing.T) {
	api := newRecordingAPI(t,
		`{"code":200,"msg":"success","request_id":"r","data":{
			"taskId":"task_stt","model":"m","state":"succeeded","cost":"0.0100","settled":true,
			"createdAt":"2026-10-05T10:00:00Z",
			"output":{"text":"left\nright","transcript":{
				"text":"left right","language":"en","languageCode":"eng",
				"languageProbability":0.98,"durationSeconds":4.2,
				"words":[{"text":"left","start":0.1,"end":0.4,"type":"word","speakerId":"speaker_0"}],
				"segments":[{"text":"left right","start":0.1,"end":4.0,"speakerId":"speaker_0"}],
				"channels":[{"index":0,"text":"left","words":[{"text":"left","start":0.1,"end":0.4}]},
					{"index":1,"text":"right"}]}}}}`,
		`{"code":200,"msg":"success","request_id":"r","data":{
			"taskId":"task_layers","model":"m","state":"succeeded","cost":"0.0300","settled":true,
			"createdAt":"2026-10-05T10:00:00Z",
			"output":{"assets":[
				{"key":"base","url":"https://cdn.example/base.png","mime":"image/png","layer":{"zIndex":0}},
				{"key":"l1","url":"https://cdn.example/l1.png","mime":"image/png",
					"layer":{"zIndex":2,"name":"hat","boundingBox":[10,20,110,220]}}]}}}`,
		`{"code":200,"msg":"success","request_id":"r","data":{
			"taskId":"task_plain","model":"m","state":"succeeded","cost":"0.0100","settled":true,
			"createdAt":"2026-10-05T10:00:00Z",
			"output":{"assets":[{"key":"a","url":"https://cdn.example/a.mp3","mime":"audio/mpeg",
				"durationSeconds":3.5}]}}}`)
	client := api.client(t)

	stt, err := client.GetTask(context.Background(), "task_stt")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	transcript := stt.Output.Transcript
	if transcript == nil {
		t.Fatal("the structured transcript was dropped")
	}
	if transcript.Text != "left right" || transcript.LanguageCode != "eng" || transcript.DurationSeconds != 4.2 {
		t.Errorf("transcript = %+v", transcript)
	}
	if transcript.LanguageProbability == nil || *transcript.LanguageProbability != 0.98 {
		t.Errorf("languageProbability = %v, want 0.98", transcript.LanguageProbability)
	}
	if len(transcript.Words) != 1 || transcript.Words[0].SpeakerID != "speaker_0" || transcript.Words[0].End != 0.4 {
		t.Errorf("words = %+v", transcript.Words)
	}
	if len(transcript.Segments) != 1 || transcript.Segments[0].Text != "left right" ||
		transcript.Segments[0].End != 4.0 || transcript.Segments[0].SpeakerID != "speaker_0" {
		t.Errorf("segments = %+v", transcript.Segments)
	}
	if len(transcript.Channels) != 2 || transcript.Channels[1].Index != 1 || len(transcript.Channels[0].Words) != 1 {
		t.Errorf("channels = %+v", transcript.Channels)
	}
	// The plain text stays where it always was; the structured result is an addition, not a move.
	if stt.Output.Text != "left\nright" {
		t.Errorf("text = %q", stt.Output.Text)
	}

	layers, err := client.GetTask(context.Background(), "task_layers")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	assets := layers.Output.Assets
	if len(assets) != 2 {
		t.Fatalf("assets = %+v", assets)
	}
	// The base image has zIndex 0. It must still decode to a non-nil Layer: nil is how a caller
	// tells "not a decomposition result" apart from "the bottom of the stack".
	if assets[0].Layer == nil || assets[0].Layer.ZIndex != 0 || assets[0].Layer.BoundingBox != nil {
		t.Errorf("base layer = %+v", assets[0].Layer)
	}
	if got := assets[1].Layer; got == nil || got.ZIndex != 2 || got.Name != "hat" ||
		len(got.BoundingBox) != 4 || got.BoundingBox[3] != 220 {
		t.Errorf("layer = %+v", got)
	}

	// The counter-case: an ordinary output carries neither, so neither may appear out of nowhere.
	plain, err := client.GetTask(context.Background(), "task_plain")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if plain.Output.Transcript != nil || plain.Output.Embeddings != nil || plain.Output.Assets[0].Layer != nil {
		t.Errorf("an ordinary audio result grew a transcript, embeddings or a layer: %+v", plain.Output)
	}
}

// TestTaskRecordDecodesEmbeddings pins the embedding result. Before this field existed the vectors
// decoded into nothing - encoding/json drops unknown keys silently - so a succeeded and charged
// embedding task looked empty.
func TestTaskRecordDecodesEmbeddings(t *testing.T) {
	api := newRecordingAPI(t, `{"code":200,"msg":"success","request_id":"r","data":{
		"taskId":"task_embedding","model":"m","state":"succeeded","cost":"0.000000012","settled":true,
		"createdAt":"2026-11-03T10:00:00Z",
		"output":{"embeddings":{"inputTokens":3,"data":[
			{"index":0,"embedding":[-0.0704345703125,-0.40625,0.353759765625]},
			{"index":1,"embedding":[0.5,0.25,-0.125]}]}}}}`)
	client := api.client(t)

	task, err := client.GetTask(context.Background(), "task_embedding")
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	embeddings := task.Output.Embeddings
	if embeddings == nil {
		t.Fatal("the embedding result was dropped")
	}
	if embeddings.InputTokens != 3 || len(embeddings.Data) != 2 {
		t.Fatalf("embeddings = %+v", embeddings)
	}
	if embeddings.Data[1].Index != 1 || len(embeddings.Data[1].Embedding) != 3 ||
		embeddings.Data[0].Embedding[0] != -0.0704345703125 {
		t.Errorf("vectors = %+v", embeddings.Data)
	}
	if len(task.Output.Assets) != 0 || task.Output.Transcript != nil {
		t.Errorf("an embedding result grew assets or a transcript: %+v", task.Output)
	}
}

// TestListTasksCarriesTheRequestID pins the field that ties a synchronous text call - made with
// an OpenAI, Anthropic or Gemini client, not with this package - back to the task that holds its
// charge.
func TestListTasksCarriesTheRequestID(t *testing.T) {
	api := newRecordingAPI(t, `{"code":200,"msg":"success","request_id":"r","data":{
		"items":[{"taskId":"job_1","requestId":"req_abc","model":"m","state":"succeeded",
			"cost":"0.0010","settled":true,"createdAt":"2026-10-05T10:00:00Z",
			"deadlineAt":"2026-10-05T10:05:00Z"},
			{"taskId":"job_2","model":"m","state":"succeeded","cost":"0.0010","settled":true,
			"createdAt":"2026-10-05T10:00:00Z","deadlineAt":"2026-10-05T10:05:00Z"}],
		"hasMore":false}}`)

	list, err := api.client(t).ListTasks(context.Background(), ListTasksOptions{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(list.Items) != 2 || list.Items[0].RequestID != "req_abc" || list.Items[1].RequestID != "" {
		t.Errorf("items = %+v", list.Items)
	}
}

// TestGetModelReadsTheToolkit pins the grouping field that the studio tools are listed under.
func TestGetModelReadsTheToolkit(t *testing.T) {
	api := newRecordingAPI(t, `{"code":200,"msg":"success","request_id":"r","data":{
		"model":"spicyapi/background-remover-v1/edit","family":"background-remover/v1",
		"modality":"image","tasks":["image-edit"],"toolkit":"studio-tools",
		"pricing":[{"variant":"","unit":"per_image","price":"0.0100","currency":"USD"}]}}`)

	model, err := api.client(t).GetModel(context.Background(), "spicyapi/background-remover-v1/edit")
	if err != nil {
		t.Fatalf("GetModel: %v", err)
	}
	if model.Toolkit != "studio-tools" || len(model.Tasks) != 1 || model.Tasks[0] != "image-edit" {
		t.Errorf("model = %+v", model)
	}
}

// uploadIntakeAPI answers the first upload-url call with a business failure, after recording its
// body. That is enough to see which content type the client asked for without a storage server.
func uploadIntakeAPI(t *testing.T) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.Method+" "+r.URL.Path+" "+string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":400,"msg":"stop here","request_id":"r","data":null}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(Options{APIKey: "sk-test", BaseURL: server.URL, MaxRetries: -1})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	return client, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// TestUploadFileInfersDocumentTypes pins that reference documents get past the local type check.
// Before the contract added them, a PDF headed for a reference-file model was refused here, by
// this package, with an error that read as though the platform did not take documents at all.
func TestUploadFileInfersDocumentTypes(t *testing.T) {
	cases := map[string]string{
		"brief.PDF":  "application/pdf",
		"deck.pptx":  "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"notes.md":   "text/markdown",
		"sheet.xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			client, seen := uploadIntakeAPI(t)
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte("document bytes"), 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}

			_, err := client.UploadFile(context.Background(), path, "")
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Code != 400 {
				t.Fatalf("expected the fake API's own rejection, got %v", err)
			}
			got := seen()
			if len(got) != 1 || !strings.HasPrefix(got[0], "POST /common/upload-url ") ||
				!strings.Contains(got[0], `"contentType":"`+want+`"`) {
				t.Errorf("requests = %v, want one upload-url call for %s", got, want)
			}
		})
	}
}

// TestUploadFileDoesNotGuessKeynoteFromDotKey pins the one extension left out of inference.
// ".key" is how PEM private keys are usually named; guessing Keynote from it would stream a key
// into object storage, and the server can only refuse the bytes after they have arrived.
func TestUploadFileDoesNotGuessKeynoteFromDotKey(t *testing.T) {
	client, seen := uploadIntakeAPI(t)
	path := filepath.Join(t.TempDir(), "server.key")
	if err := os.WriteFile(path, []byte("-----BEGIN PRIVATE KEY-----\nnot real\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}

	if _, err := client.UploadFile(context.Background(), path, ""); err == nil ||
		!strings.Contains(err.Error(), "cannot infer a content type") {
		t.Errorf("a .key file must not be uploaded on a guess, got %v", err)
	}
	if got := seen(); len(got) != 0 {
		t.Errorf("a .key file reached the API without an explicit content type: %v", got)
	}

	// The counter-case: Keynote itself is accepted once the caller says so, so the exclusion is
	// about guessing, not about the format.
	if _, err := client.UploadFile(context.Background(), path, "application/vnd.apple.keynote"); err == nil {
		t.Fatal("expected the fake API's own rejection")
	}
	if got := seen(); len(got) != 1 || !strings.Contains(got[0], `"contentType":"application/vnd.apple.keynote"`) {
		t.Errorf("an explicit Keynote upload did not reach the API: %v", got)
	}
}
