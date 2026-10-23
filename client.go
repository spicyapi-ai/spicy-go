// Package spicy is the official SpicyAPI client for Go.
//
// SpicyAPI serves image, video and audio generation models behind one API. This
// package covers the asynchronous media workflow end to end: discover a model and
// its input schema, upload local reference material, quote a request, submit it,
// wait for a terminal state, and collect the result.
//
// Text models are deliberately out of scope. They speak the OpenAI, Anthropic and
// Gemini wire formats, so the established clients for those already work against
// this service; wrapping them here would add nothing. The one piece of this package
// they do need is Upload: a chat model that reads images, video or audio takes the
// spicy://f/ URI it returns in an image_url, video_url or audio_url message part.
//
// It depends on the standard library only.
//
// What it covers is the asynchronous generation pipeline: find a model and its
// input schema, quote it, create a task, poll that task to a terminal state,
// upload reference media, refresh an expired artefact link, and verify the
// webhook that announces a result. Files worth keeping go into the asset
// library (SaveAsset), where they get a stable spicy://a/ reference that does
// not expire. Text models are reached through the
// OpenAI, Anthropic and Gemini compatible layers instead, where the official
// libraries work by changing only the base URL.
//
// Two things decide most of the code below:
//
//   - Every /api/v1 response is the {code,msg,data,request_id} envelope, and a
//     successful HTTP status does not mean the call succeeded. Branch on the
//     business code, never on the message text.
//   - HTTP 202 from task creation means accepted, not finished. Success or
//     failure is reported by the task's state, read back with GetTask.
//
// The contract this file is written against is published at
// https://docs.spicyapi.ai/openapi.yaml.
package spicy

import (
	"bytes"
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Version is this package's released version, and the only place that number
// is written down. UserAgent derives from it, so bumping it here is the whole
// release edit — a second literal somewhere else is how a client ends up
// announcing a version it is not.
//
// Keep it equal to the git tag it ships under, without the leading v. Nothing
// enforces that from inside the package: there is no build metadata to read
// back, because a tagged module exposes its version only to the programs that
// depend on it, and never to itself.
const Version = "0.3.0"

// UserAgent identifies this client on every API request.
//
// Without it net/http sends Go-http-client/1.1, which says nothing beyond "a
// Go program". That is the difference between being able to answer "which
// clients are affected by this" from a log and having to guess: a bad release,
// a version-specific bug or a deprecation all become visible only if the
// requests carry a version.
//
// The shape matches the sibling clients (SpicyAPI-Java/…, SpicyAPI-PHP/…) so
// that one query covers every language.
const UserAgent = "SpicyAPI-Go/" + Version

// Defaults for a new Client. Every one of them can be overridden in Options.
const (
	// APIBaseURL is the only production API root.
	APIBaseURL = "https://api.spicyapi.ai/api/v1"

	// DefaultRequestTimeout bounds one JSON API attempt, not the whole call.
	// Give the context its own deadline to bound a call including retries.
	DefaultRequestTimeout = 30 * time.Second

	// DefaultUploadTimeout bounds the PUT that streams bytes into object
	// storage. It is deliberately far longer than DefaultRequestTimeout:
	// audio and video may be 90 MiB, and 30 seconds would demand a sustained
	// 25 Mbit/s upstream from the caller. A connection cut halfway through
	// reports a timeout, which reads like a network fault but is this client
	// hanging up on itself.
	DefaultUploadTimeout = 10 * time.Minute

	// DefaultWaitTimeout bounds WaitForTerminal. It is a local safety bound,
	// not a service level: reaching it says nothing about the remote task.
	DefaultWaitTimeout = 10 * time.Minute

	// DefaultPollInterval is the first gap between task polls; the gap grows
	// by half on every pass and stops at MaxPollInterval.
	DefaultPollInterval = 2 * time.Second
	// MaxPollInterval caps the growth of the polling gap.
	MaxPollInterval = 15 * time.Second

	// DefaultMaxRetries is how many extra attempts a replayable request gets.
	DefaultMaxRetries = 3

	// WebhookTolerance is the default clock skew VerifyWebhook accepts.
	WebhookTolerance = 5 * time.Minute

	baseRetryDelay   = 500 * time.Millisecond
	maxRetryDelay    = 8 * time.Second
	maxResponseBytes = 8 << 20
)

// Task states. The two in-flight states are the stable part of this set:
// treat anything else as terminal rather than listing the terminal values,
// so that a state added later ends a wait instead of extending it forever.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
	StateCanceled  = "canceled"
	StateExpired   = "expired"
)

// Failure identifiers reported on a terminal task. The set is closed; handle
// any value you do not recognise the way you handle ErrorCodeUpstreamFailed.
const (
	ErrorCodeInvalidRequest         = "invalid_request"
	ErrorCodeUnsupportedCombination = "unsupported_combination"
	ErrorCodeContentRejected        = "content_rejected"
	ErrorCodeRateLimited            = "rate_limited"
	ErrorCodeUpstreamUnavailable    = "upstream_unavailable"
	ErrorCodeGenerationFailed       = "generation_failed"
	ErrorCodeTimeout                = "timeout"
	ErrorCodeInvalidAsset           = "invalid_asset"
	ErrorCodeUpstreamFailed         = "upstream_failed"
)

// Business codes worth naming. They travel in the envelope's code field and
// are the thing to branch on: several of them share one HTTP status.
//
//	40003  the bytes that were uploaded do not match the ticket that was
//	       issued for them (size, media type or signature). Not retryable:
//	       request a fresh ticket with the real content type and size and
//	       upload again.
//	40004  the request is valid but no deployment serves this exact
//	       combination of settings right now. Not retryable: change the
//	       parameter named in the message, or pick another model. Waiting
//	       does not help, and nothing was created or held.
//	40310  the account has not verified its email address yet, so its keys
//	       cannot run models. Not retryable from code: open the verification
//	       link sent at sign-up, or send a new one from the Console. The same
//	       key works as soon as the address is verified.
//	40901  the quote expired, or the request or price changed since it was
//	       signed. Quote again, then resubmit.
//	503    a supporting dependency is briefly unavailable. Retryable; honour
//	       Retry-After when the response carries it.
//	50301  the model itself has no usable deployment or effective price.
//	       Retryable, but repetition will not fix an unlisted model.
//	50302  a synchronous generation failed at the model and the hold was
//	       released, so nothing was charged. The same request body is safe to
//	       submit again — under a new idempotency key, because the old key is
//	       already spent on the attempt that failed. This client therefore
//	       does not resend it for you.
//
// 503, 50301 and 50302 share HTTP 503 and mean three different things, which
// is the reason this client reads the business code first and the status
// second.
const (
	CodeSuccess                = 200
	CodeUploadMismatch         = 40003
	CodeUnsupportedCombination = 40004
	CodeInsufficientBalance    = 40201
	CodeSpendCapReached        = 40202
	CodeEmailNotVerified       = 40310
	CodeQuoteRejected          = 40901
	CodeDependencyUnavailable  = 503
	CodeModelUnavailable       = 50301
	CodeGenerationRefunded     = 50302
)

// retryableHTTP holds the statuses worth another attempt when the response
// body is not our envelope at all, for example an edge network error page.
var retryableHTTP = map[int]bool{408: true, 429: true, 500: true, 502: true, 503: true, 504: true}

// retryableCode holds the business codes worth another attempt with the exact
// same bytes. CodeGenerationRefunded is deliberately absent: it needs a new
// idempotency key, so resending it here would be a different request.
var retryableCode = map[int]bool{
	429:                       true,
	500:                       true,
	CodeDependencyUnavailable: true,
	CodeModelUnavailable:      true,
}

// envelope is the shape every /api/v1 endpoint returns. Data stays raw so each
// call site decodes it into whatever it actually expects.
type envelope struct {
	Code      int             `json:"code"`
	Msg       string          `json:"msg"`
	Data      json.RawMessage `json:"data"`
	RequestID string          `json:"request_id"`
}

// APIError is a rejected call: an HTTP status, the business code to branch on,
// and the request ID to quote to support. Recover it with errors.As.
type APIError struct {
	// Status is the HTTP status. It is 0 when no response was parsed.
	Status int
	// Code is the business code from the envelope, 0 when the body was not an
	// envelope. Branch on this, not on Message.
	Code int
	// Message is the server's explanation, in English unless the account or
	// the Accept-Language header selects another language. Never parse it.
	Message string
	// RequestID correlates this call with our logs.
	RequestID string
	// RetryAfter is the server's own backoff request, 0 when absent.
	RetryAfter time.Duration
	// Reason is the failure envelope's data.reason, a stable machine-readable
	// refinement of Code, and empty when the server sent none. Branch on it
	// (or on Code), never on Message. The asset library's values are the
	// Reason* constants; any other value is passed through exactly as
	// received, so keep a default branch and treat an unknown one like an
	// empty one.
	Reason string
}

func (e *APIError) Error() string {
	parts := []string{fmt.Sprintf("spicy: %s", e.Message)}
	if e.Code != 0 {
		parts = append(parts, fmt.Sprintf("code %d", e.Code))
	}
	if e.Reason != "" {
		parts = append(parts, fmt.Sprintf("reason %s", e.Reason))
	}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.RequestID != "" {
		parts = append(parts, fmt.Sprintf("request_id %s", e.RequestID))
	}
	return strings.Join(parts, ", ")
}

// Retryable reports whether sending the identical request again is worth it.
func (e *APIError) Retryable() bool {
	if e.Code != 0 {
		return retryableCode[e.Code]
	}
	return retryableHTTP[e.Status]
}

// Refunded reports code 50302: generation failed at the model and the hold was
// released, so this cost nothing. Submit the same input again under a new
// idempotency key.
func (e *APIError) Refunded() bool { return e.Code == CodeGenerationRefunded }

// WaitTimeoutError means a local polling deadline elapsed. It is not evidence
// that the task failed: the generation is still running somewhere, so store
// TaskID and reconcile it later instead of submitting the work twice.
type WaitTimeoutError struct {
	TaskID    string
	Timeout   time.Duration
	LastState string
}

func (e *WaitTimeoutError) Error() string {
	return fmt.Sprintf(
		"spicy: task %s did not finish within the local %s deadline (last state %q); its remote state is unknown",
		e.TaskID, e.Timeout, e.LastState,
	)
}

// Unwrap lets errors.Is(err, context.DeadlineExceeded) recognise this.
func (e *WaitTimeoutError) Unwrap() error { return context.DeadlineExceeded }

// Webhook rejection reasons. Every one of them wraps ErrWebhookRejected, so a
// handler that only wants to answer 401 can test for that alone.
var (
	ErrWebhookRejected  = errors.New("spicy: webhook rejected")
	ErrWebhookStale     = fmt.Errorf("%w: timestamp outside the tolerance window", ErrWebhookRejected)
	ErrWebhookSignature = fmt.Errorf("%w: signature mismatch", ErrWebhookRejected)
)

// ModelPrice is one price tier of a model.
type ModelPrice struct {
	// Variant identifies the tier, for example "720p" or
	// "duration=5;resolution=720p". Empty when the model has a single price.
	Variant string `json:"variant"`
	// Unit is per_image, per_second, per_request, per_1k_tokens or
	// per_1k_characters (text-to-speech priced on the length of the script).
	// Treat it as open: a unit added later must not break a client that only
	// displays it.
	Unit string `json:"unit"`
	// Price is a decimal USD string. Keep it a string end to end; binary
	// floating point cannot hold these amounts exactly.
	Price string `json:"price"`
	// RegularPrice is the undiscounted price, present only while an offer runs.
	RegularPrice string     `json:"regularPrice,omitempty"`
	OfferLabel   string     `json:"offerLabel,omitempty"`
	OfferPercent string     `json:"offerPercent,omitempty"`
	OfferEndsAt  *time.Time `json:"offerEndsAt,omitempty"`
	Currency     string     `json:"currency"`
}

// ModelExample is an input that validates against the model's current schema.
type ModelExample struct {
	ID         string         `json:"id"`
	Input      map[string]any `json:"input"`
	SortWeight int            `json:"sortWeight"`
}

// Model is one callable endpoint from the authenticated catalogue. The typed
// fields are the ones an integration usually reads; the response carries more.
type Model struct {
	// Model is the exact identifier to pass as CreateTaskInput.Model.
	Model  string `json:"model"`
	Family string `json:"family"`
	// DisplayName is for your UI, not for calls.
	DisplayName string `json:"displayName"`
	// Provider is the model's creator, the lab that built it.
	Provider string `json:"provider"`
	// Modality is image, video, audio or text.
	Modality string `json:"modality"`
	// Tasks holds exactly one internal classification and is not always the
	// suffix of Model: editing endpoints publish as .../edit and classify as
	// image-to-image, or as image-edit for single-purpose tools such as
	// background removal. Call with Model; group with Tasks. The set is open
	// and grows as models are added.
	Tasks []string `json:"tasks"`
	Async bool     `json:"async"`
	// Mature and PolicyTier are informational metadata about the model. They
	// take no part in authorization, availability or rejection.
	Mature     bool   `json:"mature"`
	PolicyTier string `json:"policyTier"`
	// Toolkit groups related endpoints for display: subject-swap for identity
	// swaps, studio-tools for single-purpose image and video tools such as
	// upscaling. Empty when the model belongs to no toolkit. It takes no part
	// in routing, pricing or authorization, and the set is open.
	Toolkit string `json:"toolkit,omitempty"`
	// TaskTimeoutSeconds is the platform execution deadline, not the duration
	// of the media that comes out.
	TaskTimeoutSeconds int `json:"taskTimeoutSeconds"`
	// Enabled and Available both have to be true before a model can be called.
	Enabled   bool `json:"enabled"`
	Available bool `json:"available"`
	// QuantityField names the input field that multiplies the price.
	QuantityField string       `json:"quantityField"`
	Pricing       []ModelPrice `json:"pricing"`
	StartingPrice *ModelPrice  `json:"startingPrice,omitempty"`
	// InputSchema is JSON Schema 2020-12 and stays raw on purpose: it is what
	// validates your input and what a form generator renders, and both jobs
	// want the document itself. Request it with ListModelsOptions.IncludeSchema
	// or read it from GetModel, which always includes it.
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// Version changes whenever the schema or metadata changes; cache on it.
	Version       string         `json:"version"`
	Availability  string         `json:"availability"`
	Badges        []string       `json:"badges"`
	RelatedModels []string       `json:"relatedModels"`
	Examples      []ModelExample `json:"examples,omitempty"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

// ModelList is the ListModels response.
type ModelList struct {
	Total int     `json:"total"`
	Items []Model `json:"items"`
}

// Quote is a signed price for one exact request. It reserves nothing and
// guarantees no capacity; it only fixes the price for five minutes.
type Quote struct {
	// QuoteID goes back to CreateTask together with ExpectedCost.
	QuoteID string `json:"quoteId"`
	Model   string `json:"model"`
	// EstimatedCost is what the request is expected to cost, MaxCharge the
	// ceiling. Both are decimal USD strings.
	EstimatedCost string `json:"estimatedCost"`
	MaxCharge     string `json:"maxCharge"`
	Currency      string `json:"currency"`
	// Quantity and Unit explain how the amount was reached.
	Quantity  string    `json:"quantity"`
	Unit      string    `json:"unit"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// CreateTaskInput is the body shared by Quote and CreateTask.
type CreateTaskInput struct {
	// Model is an exact identifier from the catalogue.
	Model string `json:"model"`
	// Input is validated against that model's input schema. Media fields take
	// a public HTTPS URL, a committed spicy:// URI, or a small Base64 data URI.
	// Image fields also take a persona image created in the Console, as
	// spicy://p/<assetId>, from keys created in your personal workspace.
	Input map[string]any `json:"input"`
	// CallBackURL is an optional public HTTPS endpoint for terminal delivery.
	// Verify every delivery with VerifyWebhook before acting on it.
	CallBackURL string `json:"callBackUrl,omitempty"`
	// QuoteID and ExpectedCost together confirm a price: if the price moved
	// since the quote was signed, the task is rejected with code 40901 instead
	// of being accepted at the new amount.
	QuoteID      string `json:"quoteId,omitempty"`
	ExpectedCost string `json:"expectedCost,omitempty"`
}

// CreateTaskResult is the acceptance record, not a result.
type CreateTaskResult struct {
	TaskID string `json:"taskId"`
	// State is queued for a new submission. An idempotent replay reports the
	// original task's current state instead, which may already be terminal.
	State string `json:"state"`
	// EstimatedCost is the amount held, and the ceiling on the final charge.
	// Unused funds are released; nothing above the hold is ever collected.
	//
	// It is **empty when Task is non-nil**. A task that already finished has a
	// real cost rather than an estimate, and the service sends that instead;
	// the number is Task.Cost.
	EstimatedCost string `json:"estimatedCost"`
	// DeadlineAt is the server's execution deadline, fixed at acceptance. It
	// is not a local wait bound and not an artefact expiry.
	DeadlineAt time.Time `json:"deadlineAt"`

	// Task is the complete terminal record, and is non-nil only when
	// CreateTaskOptions.WaitSeconds was set and the task finished inside that
	// budget. It is exactly what GetTask would return.
	//
	// Nil is the ordinary outcome and is not a failure: the wait ran out, the
	// task is still running, and you poll from here. **Setting WaitSeconds is
	// an optimisation, never a guarantee** — code that reads Task without
	// checking for nil works right up until the first slow generation.
	//
	// Note this is not the same question as "is it finished": State can
	// already be terminal on a nil Task, because an idempotent replay reports
	// the original task's current state. InFlight answers that one.
	Task *Task `json:"-"`
}

// InFlight reports whether the task was still queued or running when the
// service produced this answer. Everything else is terminal, including a state
// this client has never heard of — same rule as Task.InFlight.
func (r *CreateTaskResult) InFlight() bool {
	return r.State == StateQueued || r.State == StateRunning
}

// RetryTaskResult additionally names the task this one was created from.
type RetryTaskResult struct {
	CreateTaskResult
	SourceTaskID string `json:"sourceTaskId"`
}

// Asset is one generated artefact.
type Asset struct {
	// Key identifies this output for CreateDownloadURL.
	Key string `json:"key,omitempty"`
	// URL is a signed link that works on its own. Never attach your API key to
	// it: it is already a bearer grant, and sending the key leaks it to storage.
	// It is absent while Pending or Unavailable is true.
	URL string `json:"url,omitempty"`
	// ExpiresAt is this link's expiry, normally about twenty minutes out. It is
	// not the retention deadline of the result; poll again for a fresh link.
	ExpiresAt       *time.Time `json:"expiresAt,omitempty"`
	MIME            string     `json:"mime,omitempty"`
	Width           int        `json:"width,omitempty"`
	Height          int        `json:"height,omitempty"`
	DurationSeconds float64    `json:"durationSeconds,omitempty"`
	Bytes           int64      `json:"bytes,omitempty"`
	Role            string     `json:"role,omitempty"`
	NSFW            bool       `json:"nsfw,omitempty"`
	// Pending means the artefact is still being copied into our own storage.
	Pending bool `json:"pending,omitempty"`
	// Unavailable means it will not arrive; do not keep waiting for it.
	Unavailable bool `json:"unavailable,omitempty"`
	// Layer is set only on the outputs of a layer-decomposition task.
	Layer *AssetLayer `json:"layer,omitempty"`
}

// AssetLayer places one output of a layer-decomposition task.
//
// The first asset is the base image (the source with the separated elements
// removed, ZIndex 0) and every other asset is one transparent PNG layer. Stack
// them by ascending ZIndex to rebuild the picture, rather than relying on the
// order of Assets.
type AssetLayer struct {
	ZIndex int `json:"zIndex"`
	// Name is the model's own label for the layer, when it gives one.
	Name string `json:"name,omitempty"`
	// BoundingBox is [x1, y1, x2, y2] in base-image pixels: scale the layer
	// into that box to put it back in place. It is nil on the base image and on
	// models that do not report positions, where the layers still work as
	// standalone cut-outs.
	BoundingBox []int `json:"boundingBox,omitempty"`
}

// TaskOutput is the result payload of a finished task.
//
// Not every endpoint produces files. Transcription and other text endpoints
// answer in Text and return no assets at all, so check what you actually got
// before reaching for Assets[0].
type TaskOutput struct {
	// Text is the plain-text result. For a multichannel transcription it holds
	// one line per channel, in channel order.
	Text string `json:"text,omitempty"`
	// Transcript is the structured speech-to-text result, with timing and
	// language details. Nil on every other task, and on transcription models
	// that return plain text only.
	Transcript *TaskTranscript `json:"transcript,omitempty"`
	Assets     []Asset         `json:"assets,omitempty"`
}

// TaskTranscript is a structured transcription. It is erased together with the
// rest of the output on the prompt-retention schedule.
type TaskTranscript struct {
	Text string `json:"text,omitempty"`
	// Language is the detected language as the model reports it, for example
	// "en"; LanguageCode is the ISO 639-3 form, for example "eng", when known.
	Language     string `json:"language,omitempty"`
	LanguageCode string `json:"languageCode,omitempty"`
	// LanguageProbability is the confidence of that detection, 0 to 1. It is a
	// pointer because "not reported" and "zero confidence" are different.
	LanguageProbability *float64 `json:"languageProbability,omitempty"`
	// DurationSeconds is the length of the transcribed audio.
	DurationSeconds float64              `json:"durationSeconds,omitempty"`
	Words           []TaskTranscriptWord `json:"words,omitempty"`
	// Channels has one entry per audio channel when multichannel
	// transcription was requested, and is empty otherwise.
	Channels []TaskTranscriptChannel `json:"channels,omitempty"`
}

// TaskTranscriptWord is one timed segment of a transcript. Start and End are
// seconds from the beginning of the audio.
type TaskTranscriptWord struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	// Type is the segment type, for example "word", when the model reports one.
	Type string `json:"type,omitempty"`
	// SpeakerID is present only when the model separates speakers.
	SpeakerID string `json:"speakerId,omitempty"`
}

// TaskTranscriptChannel is the transcript of one audio channel.
type TaskTranscriptChannel struct {
	// Index is zero-based.
	Index        int                  `json:"index"`
	Text         string               `json:"text,omitempty"`
	Language     string               `json:"language,omitempty"`
	LanguageCode string               `json:"languageCode,omitempty"`
	Words        []TaskTranscriptWord `json:"words,omitempty"`
}

// TaskRetention says when this task's content disappears and which layer
// decided it: the per-request header, the account setting, or the platform
// maximum.
type TaskRetention struct {
	OutputsExpireAt time.Time `json:"outputsExpireAt"`
	PromptsExpireAt time.Time `json:"promptsExpireAt"`
	Source          string    `json:"source"`
}

// Task is the full record returned by GetTask. Terminal and non-terminal
// states share this shape.
type Task struct {
	TaskID string `json:"taskId"`
	// SourceTaskID is set when this task came from RetryTask.
	SourceTaskID string `json:"sourceTaskId,omitempty"`
	Model        string `json:"model"`
	State        string `json:"state"`
	// Input is the normalized input, omitted once retention has redacted it.
	Input  map[string]any `json:"input,omitempty"`
	Output *TaskOutput    `json:"output,omitempty"`
	// ErrorCode is one of the nine ErrorCode constants. Branch on it.
	ErrorCode string `json:"errorCode,omitempty"`
	// ErrorMessage explains the failure in English. It may carry the model
	// service's own wording, with identifiers removed. Do not parse it.
	ErrorMessage string `json:"errorMessage,omitempty"`
	// Cost is a decimal USD string: the final charge once Settled is true, and
	// the held estimate before that.
	//
	// A failed or expired task normally releases its hold in full, with one
	// exception: on a model
	// whose page states that content-policy refusals are billed, a task that
	// fails with ErrorCodeContentRejected settles at the accepted estimate.
	// Read Cost once Settled is true instead of assuming a failure was free.
	Cost    string `json:"cost"`
	Settled bool   `json:"settled"`

	CreatedAt        time.Time      `json:"createdAt"`
	DeadlineAt       *time.Time     `json:"deadlineAt,omitempty"`
	CompletedAt      *time.Time     `json:"completedAt,omitempty"`
	ContentState     string         `json:"contentState,omitempty"`
	ContentRemovedBy string         `json:"contentRemovedBy,omitempty"`
	PurgedAt         *time.Time     `json:"purgedAt,omitempty"`
	Retention        *TaskRetention `json:"retention,omitempty"`
}

// InFlight reports whether the task is still queued or running. Everything
// else is terminal, including a state this client has never heard of.
func (t *Task) InFlight() bool {
	return t.State == StateQueued || t.State == StateRunning
}

// ReadyAssets returns the artefacts that have a usable URL right now.
func (t *Task) ReadyAssets() []Asset {
	if t == nil || t.Output == nil {
		return nil
	}
	ready := make([]Asset, 0, len(t.Output.Assets))
	for _, asset := range t.Output.Assets {
		if asset.URL != "" && !asset.Unavailable {
			ready = append(ready, asset)
		}
	}
	return ready
}

// HasPendingAssets reports whether an artefact is still being copied. A task
// can be succeeded with its files not yet linkable.
func (t *Task) HasPendingAssets() bool {
	if t == nil || t.Output == nil {
		return false
	}
	for _, asset := range t.Output.Assets {
		if asset.Pending && !asset.Unavailable {
			return true
		}
	}
	return false
}

// PurgeResult reports what PurgeTask destroyed.
type PurgeResult struct {
	TaskID string `json:"taskId"`
	// ContentState is purged after a successful destroy and on every repeat.
	ContentState string `json:"contentState"`
	// PurgedAt is the time of the first destroy, not of this call.
	PurgedAt         *time.Time `json:"purgedAt,omitempty"`
	ContentRemovedBy string     `json:"contentRemovedBy,omitempty"`
	// BillingRetained is always true. Destroying content never touches the
	// ledger, the charged amount or anything else the spend record is made of.
	BillingRetained bool `json:"billingRetained"`
	// MediaDeletionPending is true while the background sweep is still
	// removing the stored objects, which takes about a minute.
	MediaDeletionPending bool `json:"mediaDeletionPending"`
}

// UploadTicket is a presigned slot for exactly one file of a declared size.
type UploadTicket struct {
	FileID string `json:"fileId"`
	// Key is a compatibility alias of the final URI and is unusable until the
	// commit succeeds. Put UploadedFile.URI in task input, never this.
	Key       string `json:"key"`
	UploadURL string `json:"uploadUrl"`
	Method    string `json:"method"`
	// Headers must be forwarded to the PUT exactly as given.
	Headers map[string]string `json:"headers"`
	// ExpiresAt is about twenty minutes out and covers the PUT and the commit
	// together. Ask for a ticket right before uploading, not in advance.
	ExpiresAt time.Time `json:"expiresAt"`
	// MaxBytes is the authoritative ceiling for this upload.
	MaxBytes int64 `json:"maxBytes"`
}

// UploadedFile is a committed file, ready to be referenced by a task.
type UploadedFile struct {
	FileID string `json:"fileId"`
	Status string `json:"status"`
	Bytes  int64  `json:"bytes"`
	// ContentType is what the server verified, which may differ from what you
	// declared; if it does, the commit fails with code 40003 instead.
	ContentType string `json:"contentType"`
	SHA256      string `json:"sha256"`
	// URI is the spicy://f/fil_... value to put in task input. Use this one,
	// not the upload URL, not a storage key, not the bare file ID.
	URI       string    `json:"uri"`
	ExpiresAt time.Time `json:"expiresAt"`
	// DurationSeconds is a decimal string, present for audio and video.
	DurationSeconds string `json:"durationSeconds,omitempty"`
	Width           int    `json:"width,omitempty"`
	Height          int    `json:"height,omitempty"`
}

// DownloadTicket is a fresh signed link to one output of a finished task.
type DownloadTicket struct {
	Key       string    `json:"key"`
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// HTTPDoer is the slice of *http.Client this package needs, so that tests can
// substitute a transport without a network.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Options configures New. Every zero value falls back to a documented default.
type Options struct {
	// APIKey defaults to the SPICY_API_KEY environment variable. Keep it
	// server-side; it is a bearer credential for your balance.
	APIKey string
	// BaseURL defaults to APIBaseURL.
	BaseURL string
	// HTTPClient defaults to a client with no timeout of its own, because
	// every request here already carries a context deadline.
	HTTPClient HTTPDoer

	RequestTimeout time.Duration
	UploadTimeout  time.Duration
	WaitTimeout    time.Duration
	// MaxRetries is the number of extra attempts a replayable request gets.
	// Set it to -1 to disable retries entirely.
	MaxRetries int

	// Now and Rand exist for deterministic tests.
	Now  func() time.Time
	Rand func() float64
	// Sleep must return the context error if the context ends first.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Client calls the SpicyAPI task surface. It is safe for concurrent use.
type Client struct {
	apiKey         string
	baseURL        string
	httpClient     HTTPDoer
	requestTimeout time.Duration
	uploadTimeout  time.Duration
	waitTimeout    time.Duration
	maxRetries     int
	now            func() time.Time
	rand           func() float64
	sleep          func(ctx context.Context, d time.Duration) error
}

// New builds a client. It fails when no API key is available, because every
// endpoint here needs one and discovering that at the first call is worse.
func New(opts Options) (*Client, error) {
	key := strings.TrimSpace(opts.APIKey)
	if key == "" {
		key = strings.TrimSpace(os.Getenv("SPICY_API_KEY"))
	}
	if key == "" {
		return nil, errors.New("spicy: SPICY_API_KEY is required")
	}

	base := opts.BaseURL
	if base == "" {
		base = APIBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("spicy: BaseURL must be an absolute URL, got %q", base)
	}
	// The API key is a bearer credential, and http hands it over in the clear. Loopback is the
	// exception: that is local development and never touches a network.
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return nil, fmt.Errorf("spicy: BaseURL must use HTTPS; HTTP is allowed only for loopback, got %q", base)
	}

	client := &Client{
		apiKey:         key,
		baseURL:        strings.TrimRight(base, "/"),
		httpClient:     opts.HTTPClient,
		requestTimeout: opts.RequestTimeout,
		uploadTimeout:  opts.UploadTimeout,
		waitTimeout:    opts.WaitTimeout,
		maxRetries:     opts.MaxRetries,
		now:            opts.Now,
		rand:           opts.Rand,
		sleep:          opts.Sleep,
	}
	if client.httpClient == nil {
		client.httpClient = &http.Client{}
	}
	if client.requestTimeout <= 0 {
		client.requestTimeout = DefaultRequestTimeout
	}
	if client.uploadTimeout <= 0 {
		client.uploadTimeout = DefaultUploadTimeout
	}
	if client.waitTimeout <= 0 {
		client.waitTimeout = DefaultWaitTimeout
	}
	if client.maxRetries == 0 {
		client.maxRetries = DefaultMaxRetries
	}
	if client.maxRetries < 0 {
		client.maxRetries = 0
	}
	if client.now == nil {
		client.now = time.Now
	}
	if client.rand == nil {
		// Used only to jitter back-off, never for anything security related.
		client.rand = rand.Float64
	}
	if client.sleep == nil {
		client.sleep = sleepWithContext
	}
	return client, nil
}

// ListModelsOptions filters the catalogue. A zero field means "do not filter".
type ListModelsOptions struct {
	// Modality is image, video, audio or text.
	Modality string
	// Provider is the model's creator.
	Provider string
	// Task is an exact task, for example text-to-image or text-to-speech.
	// "edit" is the one alias: it selects every endpoint published with the
	// .../edit suffix, which covers both image-to-image and the single-purpose
	// image-edit tools, while "image-to-image" selects only the former.
	Task string
	// Search matches names and descriptions.
	Search string
	// IncludeSchema adds each model's input JSON Schema, which is what you
	// need to build or validate an input. It makes the response much larger.
	IncludeSchema bool
	// IncludeExamples adds inputs that already validate against that schema —
	// the fastest way to get a first call working.
	IncludeExamples bool
}

// ListModels returns every model this API key may call, with the prices that
// apply to this account. Enabled and Available must both be true before a
// model is callable.
func (c *Client) ListModels(ctx context.Context, opts ListModelsOptions) (*ModelList, error) {
	query := url.Values{}
	for name, value := range map[string]string{
		"modality": opts.Modality,
		"provider": opts.Provider,
		"task":     opts.Task,
		"search":   opts.Search,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}
	if opts.IncludeSchema {
		query.Set("includeSchema", "1")
	}
	if opts.IncludeExamples {
		query.Set("includeExamples", "1")
	}

	path := "/models"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	out := &ModelList{}
	if err := c.do(ctx, request{method: http.MethodGet, path: path, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// GetModel reads one model, always including its input schema.
//
// A model identifier contains slashes, so it is escaped into a single path
// segment here; sending them raw would address a different resource.
func (c *Client) GetModel(ctx context.Context, model string) (*Model, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("spicy: model is required")
	}
	out := &Model{}
	path := "/models/" + url.PathEscape(model)
	if err := c.do(ctx, request{method: http.MethodGet, path: path, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// Quote prices one exact request for five minutes without creating a task,
// holding funds or starting anything. It runs the same validation and
// admission rules as CreateTask, so it is also the cheapest way to find out
// that an input is wrong.
//
// Pass the returned QuoteID and EstimatedCost to CreateTask to refuse a price
// that moved in the meantime.
func (c *Client) Quote(ctx context.Context, input CreateTaskInput) (*Quote, error) {
	if strings.TrimSpace(input.Model) == "" {
		return nil, errors.New("spicy: input.Model is required")
	}
	if len(input.Input) == 0 {
		return nil, errors.New("spicy: input.Input is required")
	}
	// Send only the three fields that are priced: passing the previous quoteId and expectedCost
	// back would be asking "quote me at this old price", while a quote exists precisely to discover
	// that the price moved.
	body := CreateTaskInput{Model: input.Model, Input: input.Input, CallBackURL: input.CallBackURL}
	out := &Quote{}
	// A quote creates no task and holds no funds, so retrying is safe.
	if err := c.do(ctx, request{method: http.MethodPost, path: "/jobs/quote", body: body, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateTaskOptions carries the optional header that accompanies a submission.
type CreateTaskOptions struct {
	// RetentionSeconds, when non-nil, shortens how long this task's outputs
	// and its stored request text are kept. It can only shorten: the effective
	// value is the smallest of this, the account setting and the platform
	// maximum, and the result is reported back in Task.Retention.
	//
	// It is a pointer because 0 is a meaningful value — it means the outputs
	// go as soon as the task reaches a terminal state — so it must not be what
	// an empty options struct sends.
	RetentionSeconds *int

	// WaitSeconds, when non-nil, asks the service to hold the connection open
	// for that many seconds waiting for the task to finish, so the common case
	// needs no polling loop at all. Finishing inside the budget fills
	// CreateTaskResult.Task with the complete record; running out of it
	// returns the ordinary acceptance and you poll from there.
	//
	// **It is an optimisation, not a guarantee.** Nothing about setting it
	// makes the result terminal, and code written as though it does breaks on
	// the first generation slower than the budget.
	//
	// The value is sent exactly as given. The service clamps anything above
	// its ceiling and **ignores** — rather than rejects — a value that is not
	// a positive integer, on the reasoning that a typo in an optimisation
	// parameter should not fail a generation that would otherwise succeed. So
	// no ceiling is enforced here: a copy of the platform's number would be a
	// constant that can go stale without anything saying so, exactly as with
	// RetentionSeconds.
	//
	// It is a pointer for the same reason RetentionSeconds is: so that an
	// empty options struct sends no parameter at all.
	//
	// Setting it also lifts this one request's own deadline to cover the wait,
	// because the ordinary per-request budget is shorter than the service's
	// ceiling — see CreateTask.
	WaitSeconds *int
}

// NewIdempotencyKey mints a key with enough entropy that two submissions never
// collide. Store it next to your own record of the job before you send it: the
// point of the key is to be available again after the crash that lost the
// response.
func NewIdempotencyKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", fmt.Errorf("spicy: generating an idempotency key: %w", err)
	}
	return "spicy-" + hex.EncodeToString(buf), nil
}

// CreateTask submits a generation and returns as soon as it is accepted.
//
// Acceptance is not completion. The returned state is queued; read the outcome
// back with GetTask or WaitForTerminal. EstimatedCost is held, not charged,
// and it is also the ceiling: the settled charge can only be lower.
//
// CreateTaskOptions.WaitSeconds asks the service to hold the connection open
// until the task finishes, which saves the first poll on quick models. When it
// pays off the complete record arrives in CreateTaskResult.Task; when it does
// not, Task is nil and nothing else changes. **Check for nil.** A wait that
// runs out is the ordinary case, not an error, and the task is still running.
//
// A wait also gets its own deadline for this one request, because the default
// per-request budget is shorter than the service's wait ceiling and would
// otherwise cut off a response that was on its way — turning a working
// optimisation into a retry storm that ends in a misleading timeout.
//
// idempotencyKey is required by this client even though the contract allows it
// to be omitted, because the automatic resend on a network fault is only sound
// when it exists. Without a key, a request that times out leaves you unable to
// tell an accepted task from a lost one, and the obvious reaction — send it
// again — is a second generation and a second charge. Reuse the same value for
// every attempt at the same logical submission: for 24 hours, and for this API
// key, a repeat returns the original task instead of creating another.
func (c *Client) CreateTask(
	ctx context.Context,
	input CreateTaskInput,
	idempotencyKey string,
	opts CreateTaskOptions,
) (*CreateTaskResult, error) {
	if strings.TrimSpace(input.Model) == "" {
		return nil, errors.New("spicy: input.Model is required")
	}
	if len(input.Input) == 0 {
		return nil, errors.New("spicy: input.Input is required")
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return nil, errors.New("spicy: an Idempotency-Key is required; see NewIdempotencyKey")
	}

	headers := map[string]string{"Idempotency-Key": key}
	if opts.RetentionSeconds != nil {
		// Reject negatives only. The upper bound is deliberately not enforced locally: that is
		// the platform's number, and copying it into the client buries a constant that will
		// expire, so once the server relaxes it we would reject a value the user could have used.
		// Anything over the limit is clamped server-side, and the effective value is read back
		// from Task.Retention. The TypeScript, Python, PHP and Java clients all agree.
		if *opts.RetentionSeconds < 0 {
			return nil, errors.New("spicy: RetentionSeconds must not be negative")
		}
		headers["X-Spicy-Retention"] = strconv.Itoa(*opts.RetentionSeconds)
	}

	path := "/jobs/createTask"
	// A synchronous wait gets a budget of its own, separate from the ordinary request budget.
	// Without this, any wait longer than requestTimeout (30 seconds by default, against a server
	// limit of 60) is bound to time out in our own hands: measured with a 50ms per-attempt timeout
	// against a server holding for 200ms, it degrades into 4 requests over 3.9 seconds ending in
	// "context deadline exceeded" - a phrase that points at the network when the truth is that this
	// client hung up on a response that was on its way, while the task keeps running and keeps
	// being billed. uploadTimeout is broken out for the same reason.
	timeout := time.Duration(0)
	if opts.WaitSeconds != nil {
		// Negatives only; no upper bound - see CreateTaskOptions.WaitSeconds for why.
		if *opts.WaitSeconds < 0 {
			return nil, errors.New("spicy: WaitSeconds must not be negative")
		}
		path += "?" + url.Values{"wait": []string{strconv.Itoa(*opts.WaitSeconds)}}.Encode()
		// Add one ordinary budget for the round trip itself, or a server that uses its full wait
		// leaves us sitting exactly on the boundary.
		timeout = time.Duration(*opts.WaitSeconds)*time.Second + c.requestTimeout
	}

	// The two status codes return two different shapes (200 a complete task record, 202 an
	// acceptance receipt), so take the raw bytes first and decide how to decode once the status is
	// known.
	var data json.RawMessage
	var status int
	if err := c.do(ctx, request{
		method:  http.MethodPost,
		path:    path,
		body:    input,
		headers: headers,
		// Automatic retry is only safe under an idempotency key: the server folds a repeated key
		// back onto the original task, so one piece of network turbulence does not become a second
		// generation and a second charge.
		replayable: true,
		out:        &data,
		timeout:    timeout,
		status:     &status,
	}); err != nil {
		return nil, err
	}

	out := &CreateTaskResult{}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("spicy: decoding the %s response: %w", path, err)
	}
	if status == http.StatusOK {
		// A 200 only appears when the wait reached a terminal state, and its body is the same task
		// record recordInfo returns. Note that terminal does not mean successful - failed, canceled
		// and expired are terminal too; read Task.State.
		task := &Task{}
		if err := json.Unmarshal(data, task); err != nil {
			return nil, fmt.Errorf("spicy: decoding the %s task record: %w", path, err)
		}
		// Do not attach a still-running record to Task. The contract says a 200 is necessarily
		// terminal, so this should never trigger; but Task promises "a complete terminal record"
		// to the outside world, and hanging a queued record on it makes that promise false - and
		// callers use exactly that field to decide whether to keep polling. Leaving it nil costs
		// one extra poll; getting it wrong hands over an in-flight task as a finished one.
		if !task.InFlight() {
			out.Task = task
		}
	}
	return out, nil
}

// GetTask reads one task created by this API key. An unknown task, another
// account's task and a sibling key's task are all a 404; the three cases are
// deliberately indistinguishable.
func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, errors.New("spicy: taskID is required")
	}
	out := &Task{}
	path := "/jobs/recordInfo?" + url.Values{"taskId": []string{taskID}}.Encode()
	if err := c.do(ctx, request{method: http.MethodGet, path: path, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// WaitOptions tunes WaitForTerminal. Every zero field takes its default.
type WaitOptions struct {
	// Timeout is the local bound on the whole wait, DefaultWaitTimeout by
	// default. It is not a service level.
	Timeout time.Duration
	// Interval is the first gap between polls, DefaultPollInterval by default.
	Interval time.Duration
	// MaxInterval caps how far that gap grows, MaxPollInterval by default.
	MaxInterval time.Duration
	// AcceptPendingAssets returns a terminal task even when its artefacts are
	// still being copied into our storage, so they have no URL yet. Leave it
	// false to get back a record you can actually download from.
	AcceptPendingAssets bool
}

// WaitForTerminal polls a task until it stops being queued or running, backing
// off between reads.
//
// Terminal is defined as "neither of the two in-flight states" rather than as
// a list of the four known endings. A client that enumerates the endings will
// either loop forever or reject a real answer the day a new one is added, and
// that failure arrives silently. The four endings today are "succeeded",
// "failed", "canceled" and "expired"; only "succeeded" carries a result.
//
// A local timeout is not a verdict: it returns *WaitTimeoutError, and the
// generation is still running on our side. Keep the task ID and reconcile it
// later, or use a callback URL instead of waiting.
func (c *Client) WaitForTerminal(ctx context.Context, taskID string, opts WaitOptions) (*Task, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, errors.New("spicy: taskID is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = c.waitTimeout
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	maxInterval := opts.MaxInterval
	if maxInterval <= 0 {
		maxInterval = MaxPollInterval
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastState := ""
	for {
		task, err := c.GetTask(waitCtx, taskID)
		if err != nil {
			if fatal := c.waitError(ctx, waitCtx, taskID, timeout, lastState, err); fatal != nil {
				return nil, fatal
			}
			// This attempt timed out, but budget remains - keep polling rather than treating it
			// as a verdict.
		} else {
			lastState = task.State
			if !task.InFlight() && (opts.AcceptPendingAssets || !task.HasPendingAssets()) {
				return task, nil
			}
		}
		if err := c.sleep(waitCtx, c.jitter(interval)); err != nil {
			// Failing to sleep means the wait context has ended, and continuing here would turn
			// this into a busy loop. On this path waitError can only hit the first two cases; if a
			// caller's custom Sleep somehow lands it in the third, report it as a local timeout
			// anyway, because that error at least carries the taskID.
			if fatal := c.waitError(ctx, waitCtx, taskID, timeout, lastState, err); fatal != nil {
				return nil, fatal
			}
			return nil, &WaitTimeoutError{TaskID: taskID, Timeout: timeout, LastState: lastState}
		}
		if interval = interval * 3 / 2; interval > maxInterval {
			interval = maxInterval
		}
	}
}

// waitError classifies a failed poll. It returns the error to give up with, or
// nil when the wait still has budget and should simply poll again.
//
// Three cases hide behind one failed GetTask, and they are not interchangeable:
//
//   - the caller cancelled. That is the caller's own error; hand it back
//     unchanged.
//   - our local wait budget ran out. That is *WaitTimeoutError, which carries
//     the task ID — the only thing that lets the caller reconcile a generation
//     that is still running and still being charged for.
//   - **one poll** timed out while the budget is still alive. This is not a
//     verdict on anything. Returning it would end a ten minute wait over a
//     thirty second blip, and it would end it with a bare transport error that
//     has no TaskID field on it, at the exact moment the caller most needs the
//     task ID. So it returns nil and the loop polls again; when the budget does
//     run out, the second case reports it properly.
//
// The third case is what Python and Java were missing on 2026-09-20. This
// client had the machinery to tell the first two apart and fell through to the
// bare error on the third.
func (c *Client) waitError(
	ctx context.Context,
	waitCtx context.Context,
	taskID string,
	timeout time.Duration,
	lastState string,
	err error,
) error {
	switch {
	case ctx.Err() != nil:
		return err
	case errors.Is(waitCtx.Err(), context.DeadlineExceeded):
		return &WaitTimeoutError{TaskID: taskID, Timeout: timeout, LastState: lastState}
	case errors.Is(err, context.DeadlineExceeded):
		// One polling attempt timed out. Budget remains, so this is not a verdict.
		//
		// Swallow timeouts only, never answers from the server: 404, 401 and 40004 are verdicts
		// from upstream, and polling them repeatedly would merely stretch an explicit rejection
		// into ten minutes of silence. The Python and Java clients agree.
		return nil
	default:
		return err
	}
}

// RetryTask creates a new task from a failed or expired one. The source task
// is never modified, and the current schema, price, permissions and balance
// are all evaluated again — a retry can cost a different amount, or be refused
// outright.
//
// The source must be terminal and settled; retrying too early returns 409, and
// retrying a succeeded task returns 404 with no way to tell it from a task
// that was never there. Retention is inherited from the source, so one family
// of tasks never ends up with two different expiry times.
func (c *Client) RetryTask(ctx context.Context, taskID, idempotencyKey string) (*RetryTaskResult, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, errors.New("spicy: taskID is required")
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return nil, errors.New("spicy: an Idempotency-Key is required; see NewIdempotencyKey")
	}
	out := &RetryTaskResult{}
	if err := c.do(ctx, request{
		method:     http.MethodPost,
		path:       "/jobs/retry",
		body:       map[string]string{"taskId": taskID},
		headers:    map[string]string{"Idempotency-Key": key},
		replayable: true,
		out:        out,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// PurgeTask destroys a finished task's stored content: the generated objects,
// the result payload and the request text including the prompt.
//
// It destroys content, not the spend record. The ledger, the charged amount,
// the model, the state, the timestamps and the request ID all remain, which is
// what BillingRetained says out loud on the way back.
//
// Unlike the other write endpoints this one reads no Idempotency-Key header:
// the task ID is the idempotency key, so a repeat after a timeout is safe and
// returns the original purgedAt. A queued or running task returns 400 — an
// accepted generation cannot be stopped, so wait for it to finish first.
func (c *Client) PurgeTask(ctx context.Context, taskID string) (*PurgeResult, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, errors.New("spicy: taskID is required")
	}
	out := &PurgeResult{}
	if err := c.do(ctx, request{
		method: http.MethodPost,
		path:   "/jobs/purge",
		body:   map[string]string{"taskId": taskID},
		// The server guarantees idempotency on taskId, so a transport-level retry returns the same
		// record.
		replayable: true,
		out:        out,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// TaskListItem is one row of task history: metadata only. There is no input,
// no output, no signed media URL and no processing detail here — read a
// selected task back with GetTask when you need any of that.
type TaskListItem struct {
	TaskID string `json:"taskId"`
	// RequestID is the X-Request-Id of the request that created the task, so a
	// synchronous text call made through the OpenAI, Anthropic or Gemini
	// compatible endpoints can be matched to its charge here. Empty when none
	// was recorded.
	RequestID string `json:"requestId,omitempty"`
	Model     string `json:"model"`
	State     string `json:"state"`
	// Cost is the final charged amount once Settled is true, and the current
	// held estimate before that. A decimal USD string, so an unsettled row is
	// a ceiling rather than a bill.
	Cost       string    `json:"cost"`
	Settled    bool      `json:"settled"`
	CreatedAt  time.Time `json:"createdAt"`
	DeadlineAt time.Time `json:"deadlineAt"`
	// CompletedAt is absent while the task is still in flight.
	CompletedAt *time.Time `json:"completedAt,omitempty"`
}

// TaskList is one page of task history.
type TaskList struct {
	Items   []TaskListItem `json:"items"`
	HasMore bool           `json:"hasMore"`
	// NextCursor is present only when HasMore is true. It is opaque: pass it
	// back unchanged and never build one yourself.
	NextCursor string `json:"nextCursor,omitempty"`
}

// ListTasksOptions filters task history. A zero field means "do not filter".
type ListTasksOptions struct {
	// From and To are UTC dates in YYYY-MM-DD form and bound a half-open
	// interval [From, To) of at most 92 days. They are strings, not time.Time,
	// because the contract's type here is a calendar date: turning one into an
	// instant forces a timezone that the range does not have.
	//
	// Left empty the server defaults To to tomorrow in UTC and From to seven
	// days before it — see EachTask for why that default and pagination do not
	// mix well.
	From string
	To   string
	// State is one of the six task states.
	State string
	// Model is an exact catalogue identifier or a declared alias.
	Model string
	// Limit is the page size, 1 to 100, defaulting to 20 on the server.
	//
	// The upper bound is deliberately **not** enforced here. It is the
	// platform's number, and a copy of it in this package is a constant that
	// can go stale without anything saying so — it would reject a page size
	// the service had already started accepting. A value the server dislikes
	// costs one round trip and comes back as a plain 400.
	Limit int
	// Cursor is the NextCursor of the previous page. Pass it through
	// unchanged; it is opaque and constructing one yourself is meaningless.
	Cursor string
}

// ListTasks returns one page of this API key's task history, newest first.
//
// The scope is this API key alone. Tasks created by sibling keys on the same
// account, and generations started in the web console (which use no key), are
// not here and their absence is not a fault.
//
// Pages read live state rather than a frozen snapshot, so a task can move
// between states, or settle, while you walk them.
func (c *Client) ListTasks(ctx context.Context, opts ListTasksOptions) (*TaskList, error) {
	if opts.Limit < 0 {
		return nil, errors.New("spicy: Limit must not be negative")
	}
	query := url.Values{}
	for name, value := range map[string]string{
		"from":   opts.From,
		"to":     opts.To,
		"state":  opts.State,
		"model":  opts.Model,
		"cursor": opts.Cursor,
	} {
		if value != "" {
			query.Set(name, value)
		}
	}
	// 0 is Go's zero value, meaning "unset" rather than "give me none" - the contract's floor is 1
	// anyway.
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}

	path := "/jobs"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	out := &TaskList{}
	// Read-only; a retry returns the same page.
	if err := c.do(ctx, request{method: http.MethodGet, path: path, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// EachTask walks every page of ListTasks and calls fn once per task. Return
// false from fn to stop early; the error is the first page that failed.
//
// Every filter is repeated unchanged on each request and only the cursor
// moves, because that is what the contract asks for: a cursor is only
// meaningful against the query it was issued for, and changing a filter
// mid-walk silently produces a page that belongs to neither query.
//
// **Pin From and To yourself for any walk that might cross midnight UTC.**
// With them empty the server recomputes its default window — To is tomorrow
// in UTC — on every page, so a walk that starts at 23:59 and continues at
// 00:01 asks two different questions and splices the answers together. Nothing
// reports this: the pages arrive, the loop ends, and the result is quietly
// wrong.
func (c *Client) EachTask(ctx context.Context, opts ListTasksOptions, fn func(TaskListItem) bool) error {
	if fn == nil {
		return errors.New("spicy: fn is required")
	}
	for {
		page, err := c.ListTasks(ctx, opts)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			if !fn(item) {
				return nil
			}
		}
		// Check both: the contract says nextCursor only appears when hasMore is true, so either
		// one saying "the end" means the end. Trusting just one leaves the other free to fail into
		// endless paging - which keeps billing for API calls and looks like a hang.
		if !page.HasMore || page.NextCursor == "" {
			return nil
		}
		opts.Cursor = page.NextCursor
	}
}

// uploadContentTypes maps an extension to one of the accepted media types.
//
// ".key" is left out on purpose although Keynote is accepted. It is also the
// usual extension of a PEM private key, and inferring a type from it would
// stream that key's bytes into object storage before the server's commit check
// got the chance to refuse them. Pass the content type explicitly to upload a
// Keynote file.
var uploadContentTypes = map[string]string{
	".gif":  "image/gif",
	".jpeg": "image/jpeg",
	".jpg":  "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",

	".pdf":     "application/pdf",
	".doc":     "application/msword",
	".docx":    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":     "application/vnd.ms-excel",
	".xlsx":    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":     "application/vnd.ms-powerpoint",
	".pptx":    "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".pages":   "application/vnd.apple.pages",
	".numbers": "application/vnd.apple.numbers",
	".txt":     "text/plain",
	".md":      "text/markdown",

	// LoRA weights for loras[].path. The contract names no narrower type for a .safetensors
	// file, so it travels as application/octet-stream, and commit checks that it is complete.
	".safetensors": "application/octet-stream",
}

// CreateUploadURL asks for a presigned slot for exactly one file of exactly
// the declared size. Images are capped at 10 MiB, audio and video at 90 MiB
// and 600 seconds, reference documents (PDF, Office, iWork, plain text and
// Markdown) at 90 MiB, and LoRA weights (application/octet-stream, a
// .safetensors file) at 2 GiB; the ticket's MaxBytes is the authoritative
// ceiling.
//
// The ticket lives about twenty minutes (two hours for LoRA weights) and that
// window covers both the PUT and the commit, so ask for it right before
// uploading rather than in advance.
func (c *Client) CreateUploadURL(ctx context.Context, contentType string, size int64) (*UploadTicket, error) {
	if !isAcceptedUploadType(contentType) {
		return nil, fmt.Errorf(
			"spicy: contentType %q is not accepted; use one of %s",
			contentType, strings.Join(acceptedUploadTypes, ", "),
		)
	}
	if size <= 0 {
		return nil, errors.New("spicy: size must be the exact number of bytes you are about to upload")
	}
	out := &UploadTicket{}
	if err := c.do(ctx, request{
		method: http.MethodPost,
		path:   "/common/upload-url",
		body:   map[string]any{"contentType": contentType, "bytes": size},
		// No automatic retry: each call allocates a fresh fileId and a fresh ticket, so retrying
		// only leaves behind tickets nobody commits.
		replayable: false,
		out:        out,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// CommitUpload verifies the bytes against the ticket — size, media type,
// signature — copies them to an immutable private object and returns the
// spicy:// URI to put in task input. Repeating a successful commit is
// idempotent.
//
// Code 40003 here means the stored file does not match what the ticket was
// issued for. Do not retry it: ask for a new ticket with the real content type
// and size and upload again.
func (c *Client) CommitUpload(ctx context.Context, fileID string) (*UploadedFile, error) {
	if strings.TrimSpace(fileID) == "" {
		return nil, errors.New("spicy: fileID is required")
	}
	out := &UploadedFile{}
	path := "/files/" + url.PathEscape(fileID) + "/commit"
	if err := c.do(ctx, request{method: http.MethodPost, path: path, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// Upload runs all three steps: take a ticket, PUT the bytes straight into
// object storage, then commit. Only the URI on the returned file belongs in
// task input — not the upload URL, not a storage key, not the bare file ID.
//
// Put the URI into the model input field whose x-ui.widget is "upload" (or
// inside the list for "multi-upload"); LoRA weights go into loras[].path. The
// URI lasts one day. To keep a file longer, save it to the asset library with
// SaveAsset (source AssetSourceUpload) or upload it with UploadAssetFile.
//
// Uploading is free; you pay for the task that consumes the file. A public
// HTTPS URL can be passed in the input field directly and skips all of this.
func (c *Client) Upload(ctx context.Context, data []byte, contentType string) (*UploadedFile, error) {
	if len(data) == 0 {
		return nil, errors.New("spicy: refusing to upload an empty file")
	}
	return c.upload(ctx, bytes.NewReader(data), int64(len(data)), contentType)
}

// upload takes a ticket for size bytes of contentType, streams them from body
// and commits the result.
func (c *Client) upload(ctx context.Context, body io.ReaderAt, size int64, contentType string) (*UploadedFile, error) {
	ticket, err := c.CreateUploadURL(ctx, contentType, size)
	if err != nil {
		return nil, err
	}
	if size > ticket.MaxBytes {
		return nil, fmt.Errorf(
			"spicy: file is %d bytes, above this ticket's ceiling of %d",
			size, ticket.MaxBytes,
		)
	}
	if err := c.putUpload(ctx, ticket, body, size); err != nil {
		return nil, err
	}
	return c.CommitUpload(ctx, ticket.FileID)
}

// UploadFile uploads a local file. An empty contentType is inferred from the
// extension, which only works for the accepted media types; ".safetensors"
// LoRA weights are sent as application/octet-stream.
//
// The file is streamed from disk rather than read into memory first, because
// LoRA weights may be 2 GiB. A file that large can outlast DefaultUploadTimeout
// on an ordinary uplink, so raise Options.UploadTimeout for big ones; the
// ticket itself stays valid for two hours.
func (c *Client) UploadFile(ctx context.Context, path, contentType string) (*UploadedFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("spicy: reading %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("spicy: reading %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("spicy: %s is not a regular file", path)
	}
	if info.Size() == 0 {
		return nil, errors.New("spicy: refusing to upload an empty file")
	}
	if contentType == "" {
		inferred, ok := uploadContentTypes[strings.ToLower(filepath.Ext(path))]
		if !ok {
			return nil, fmt.Errorf(
				"spicy: cannot infer a content type from %q; pass one of %s",
				filepath.Ext(path), strings.Join(acceptedUploadTypes, ", "),
			)
		}
		contentType = inferred
	}
	return c.upload(ctx, file, info.Size(), contentType)
}

// putUpload sends size bytes read from body to object storage.
func (c *Client) putUpload(ctx context.Context, ticket *UploadTicket, body io.ReaderAt, size int64) error {
	// Moving bytes uses its own upload timeout: 30 seconds is reasonable for a JSON call, but a
	// 90 MiB video in 30 seconds demands 25 Mbit/s upstream, so on a real network we would cut
	// ourselves off halfway through - and the error would look like the other side's fault.
	ctx, cancel := context.WithTimeout(ctx, c.uploadTimeout)
	defer cancel()

	method := ticket.Method
	if method == "" {
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(ctx, method, ticket.UploadURL, io.NewSectionReader(body, 0, size))
	if err != nil {
		return fmt.Errorf("spicy: building the upload request: %w", err)
	}
	// net/http only works out the length of a few in-memory body types by itself. Without this
	// line a file body would go out chunked, which presigned PUTs refuse, so the length is stated.
	req.ContentLength = size
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(body, 0, size)), nil
	}

	// Every header from the ticket is forwarded verbatim, except that net/http does not read two
	// of them from Header, so a naive loop would drop them silently.
	for name, value := range ticket.Headers {
		switch http.CanonicalHeaderKey(name) {
		case "Content-Length":
			// net/http computes this from the body and ignores whatever Header says. So this is
			// not forwarding but reconciliation: a mismatch means the ticket was issued for a
			// different file, and uploading anyway would only hit 40003 at commit.
			declared, parseErr := strconv.ParseInt(value, 10, 64)
			if parseErr == nil && declared != size {
				return fmt.Errorf(
					"spicy: this ticket was issued for %d bytes but the file is %d",
					declared, size,
				)
			}
		case "Host":
			req.Host = value
		default:
			req.Header.Set(name, value)
		}
	}

	// This hop carries no Authorization: the presigned URL is itself the credential, an extra auth
	// header makes some object stores refuse the request outright, and it would hand our API key to
	// a third party.
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Redaction is mandatory here; wrapping err directly with %w is exactly the shape of the
		// credential leak. The reasoning is on withoutSignedURL.
		return fmt.Errorf("spicy: uploading to object storage: %w", withoutSignedURL(err, ticket.UploadURL))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode/100 != 2 {
		return &APIError{
			Status:  resp.StatusCode,
			Message: "presigned upload was rejected by object storage",
		}
	}
	return nil
}

// redactedURLPlaceholder stands in for a presigned URL that was removed from an
// error message.
const redactedURLPlaceholder = "<presigned URL redacted>"

// redactedError renders a failure without the URL it failed against, while
// keeping the cause reachable for errors.Is and errors.As.
type redactedError struct {
	text  string
	cause error
}

func (e *redactedError) Error() string { return e.text }

// Unwrap exposes the cause from **below** the *url.Error rather than the
// *url.Error itself. errors.Is(err, context.DeadlineExceeded) keeps working —
// which is what a caller needs after a ten minute upload budget runs out — and
// the signed URL is gone from the chain instead of sitting one errors.As away
// from someone's log line.
func (e *redactedError) Unwrap() error { return e.cause }

// withoutSignedURL strips a presigned URL out of a transport error.
//
// http.Client.Do reports a transport failure as *url.Error, and its Error()
// prints the URL it failed against. On this path that URL is the upload ticket:
// a roughly twenty minute **write authorization** for our object storage, whose
// X-Amz-Credential and X-Amz-Signature travel in the query string. net/url
// masks only the password inside userinfo and never touches the query, so the
// default rendering hands the whole grant to whoever is holding the error —
// and this particular error is one an application logs.
//
// Anyone who can read that log line for the next twenty minutes can write into
// the slot. spicy-php refuses to put the URL in a transport error for the same
// reason; this brings Go in line with it.
//
// The rendering loses the URL. The failure itself does not: op plus cause is
// what tells a timeout apart from a refused connection or a TLS fault, and
// those three need different reactions.
func withoutSignedURL(err error, signed string) error {
	if err == nil {
		return nil
	}

	text, cause := err.Error(), err
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Strip structurally: *url.Error is the only type on this path that prints the URL into
		// its text.
		cause = urlErr.Err
		operation := strings.TrimSpace(urlErr.Op)
		if operation == "" {
			operation = "request"
		}
		text = operation + " to " + redactedURLPlaceholder + " failed"
		if cause != nil {
			text += ": " + cause.Error()
		}
	}

	// Then a literal sweep as a backstop. The layer above only guarantees that we do not print the
	// URL; it cannot guarantee the cause has not copied it into its own text - and since the ticket
	// URL is right here, removing it requires guessing no formats at all.
	return &redactedError{text: stripLiteral(text, signed), cause: cause}
}

// stripLiteral removes a secret, and separately its query string, from a
// message.
//
// It strips twice because the full string genuinely does fail to match sometimes: escaping,
// truncation, or some layer lifting the query string out on its own. The signature lives in the
// query, so that gets a pass of its own.
func stripLiteral(text, secret string) string {
	if secret == "" {
		return text
	}
	text = strings.ReplaceAll(text, secret, redactedURLPlaceholder)
	if at := strings.Index(secret, "?"); at >= 0 && at+1 < len(secret) {
		text = strings.ReplaceAll(text, secret[at+1:], redactedURLPlaceholder)
	}
	return text
}

// CreateDownloadURL issues a fresh signed link to one output of a finished
// task. A finished task already carries usable links, so this is only needed
// once those have expired — polling the task again does the same job.
//
// Leave key empty to select the first output. Do not attach your API key when
// fetching the link: it is a bearer grant on its own.
func (c *Client) CreateDownloadURL(ctx context.Context, taskID, key string) (*DownloadTicket, error) {
	if strings.TrimSpace(taskID) == "" {
		return nil, errors.New("spicy: taskID is required")
	}
	body := map[string]string{"taskId": taskID}
	if key != "" {
		body["key"] = key
	}
	out := &DownloadTicket{}
	if err := c.do(ctx, request{
		method: http.MethodPost, path: "/common/download-url", body: body, replayable: true, out: out,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// ── Account ─────────────────────────────────────────────────────────────

// Balance is what this account can spend right now. Every amount is a decimal
// USD string, never a float: cents do not survive binary floating point.
type Balance struct {
	// Funding breaks Available down by where the money came from. It is absent
	// on accounts with nothing but a plain wallet.
	Funding *FundingOverview `json:"funding,omitempty"`
	// Available is what a new task can draw on, Held what accepted tasks have
	// already reserved, and Total the two together.
	Available string `json:"available"`
	Held      string `json:"held"`
	Total     string `json:"total"`
}

// FundingOverview separates the sources behind a balance, because they do not
// behave alike: a grant can be restricted to certain models and credit is an
// approved ceiling rather than money on hand.
type FundingOverview struct {
	// BalanceUsd is the net wallet balance. It can be negative after credit is
	// consumed or after a payment is recovered.
	BalanceUsd string `json:"balanceUsd"`
	HeldUsd    string `json:"heldUsd"`
	// PrepaidAvailableUsd is unrestricted; GrantAvailableUsd may not pay for
	// every model, and which models it covers is decided when a task is
	// admitted, not here. So a non-zero grant balance is not a promise that
	// the next task will be affordable.
	PrepaidAvailableUsd string `json:"prepaidAvailableUsd"`
	GrantAvailableUsd   string `json:"grantAvailableUsd"`
	// CashShortfallUsd is a payment recovery gap that neither grants nor credit
	// can cover. Non-zero blocks new tasks.
	CashShortfallUsd string          `json:"cashShortfallUsd"`
	Credit           *CreditFacility `json:"credit,omitempty"`
	Grants           []FundingGrant  `json:"grants,omitempty"`
	// GrantsHasMore says Grants was truncated. This endpoint has no cursor, so
	// treat the list as a sample rather than a ledger when it is true.
	GrantsHasMore bool `json:"grantsHasMore"`
}

// CreditFacility is approved pay-later capacity. LimitUsd is a ceiling, not
// funds: it is not in the wallet and it is not yours to spend twice.
type CreditFacility struct {
	Enabled      bool   `json:"enabled"`
	LimitUsd     string `json:"limitUsd"`
	AvailableUsd string `json:"availableUsd"`
	// UsedUsd is outstanding settled principal; HeldUsd is credit reserved by
	// tasks that have been accepted but not yet settled. Both occupy the limit.
	UsedUsd   string     `json:"usedUsd"`
	HeldUsd   string     `json:"heldUsd"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Status    string     `json:"status"`
	Version   int64      `json:"version"`
}

// FundingGrant is one promotional grant.
type FundingGrant struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	AmountUsd    string     `json:"amountUsd"`
	AvailableUsd string     `json:"availableUsd"`
	HeldUsd      string     `json:"heldUsd"`
	SpentUsd     string     `json:"spentUsd"`
	Status       string     `json:"status"`
	StartsAt     time.Time  `json:"startsAt"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	// ModelSlugs restricts this grant. **Empty means every model**, not none.
	ModelSlugs   []string  `json:"modelSlugs,omitempty"`
	CustomerMemo string    `json:"customerMemo"`
	CreatedAt    time.Time `json:"createdAt"`
}

// GetBalance reads the net available, held and total balance for this account,
// with the funding sources behind them.
func (c *Client) GetBalance(ctx context.Context) (*Balance, error) {
	out := &Balance{}
	if err := c.do(ctx, request{
		method: http.MethodGet, path: "/chat/credit", replayable: true, out: out,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// UsageDay is one day of activity. Day is a UTC calendar date, YYYY-MM-DD.
type UsageDay struct {
	Day       string `json:"day"`
	Calls     int64  `json:"calls"`
	Succeeded int64  `json:"succeeded"`
	Failed    int64  `json:"failed"`
	Spend     string `json:"spend"`
}

// UsageModel is one model's share of the same window.
type UsageModel struct {
	Model     string `json:"model"`
	Calls     int64  `json:"calls"`
	Succeeded int64  `json:"succeeded"`
	Failed    int64  `json:"failed"`
	Spend     string `json:"spend"`
}

// UsageReport is settled spend and call counts over one window.
type UsageReport struct {
	// From and To echo the window actually used, as UTC calendar dates. Read
	// them rather than assuming your own: leaving the options empty lets the
	// server pick, and this is the only place it says what it picked.
	From     string `json:"from"`
	To       string `json:"to"`
	Currency string `json:"currency"`
	// TotalSpend counts settled charges only, never pending holds.
	TotalCalls int64  `json:"totalCalls"`
	TotalSpend string `json:"totalSpend"`
	// Days and Models are sparse: a day or a model with no activity is absent
	// rather than present with zeroes.
	Days   []UsageDay   `json:"days"`
	Models []UsageModel `json:"models"`
}

// UsageOptions bounds a usage query to a half-open UTC interval [From, To) of
// at most 92 days, both as YYYY-MM-DD. Left empty the server uses the last
// seven days.
type UsageOptions struct {
	From string
	To   string
}

// GetUsage reports this API key's settled spend and call counts.
//
// The scope is this API key alone, and the numbers are attributed to when a
// task was created. **Settlement can land late**, so a figure read today can
// still change tomorrow — this is a reconciliation view, not a running total.
//
// It is also the wrong way to follow a task. This endpoint sits behind an
// account-wide bucket of thirty requests a minute shared by every key on the
// account, and that limiter fails closed: polling it here can lock every other
// key out of its own reporting. Use GetTask or WaitForTerminal for progress.
func (c *Client) GetUsage(ctx context.Context, opts UsageOptions) (*UsageReport, error) {
	query := url.Values{}
	if opts.From != "" {
		query.Set("from", opts.From)
	}
	if opts.To != "" {
		query.Set("to", opts.To)
	}
	path := "/usage"
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	out := &UsageReport{}
	if err := c.do(ctx, request{method: http.MethodGet, path: path, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// webhookEvent is the smallest shape that locates the task ID in either
// payload version.
type webhookEvent struct {
	TaskID string `json:"task_id"`
	Data   struct {
		TaskID string `json:"taskId"`
	} `json:"data"`
}

// VerifyWebhook authenticates one callBackUrl delivery and returns the task ID
// the signature was computed over. Use that returned ID, not one you read out
// of the body yourself.
//
// This verifies the scheme used by the per-task callBackUrl. Naming it precisely
// matters because a second, account-level delivery scheme is planned; when that
// ships it gets its own verifier rather than extra arguments here, so code
// written against this function keeps working unchanged.
//
// The signature is
//
//	base64( HMAC-SHA256( secret, "<taskID>.<timestamp>.<sha256_hex(rawBody)>" ) )
//
// where the task ID comes from top-level task_id in payload version 1 and from
// data.taskId in version 2. Read X-Webhook-Payload-Version to choose; never
// infer the version from the shape of the JSON, because an old delivery can be
// retried long after the platform moved on.
//
// rawBody must be the exact bytes that arrived. Decoding and re-encoding the
// JSON first changes key order, spacing and escaping, and the digest then
// fails every single time for a reason that looks like a wrong secret.
//
// The digest is part of the message on purpose. Signing only the task ID and
// the timestamp would let anyone who intercepted one real delivery pair those
// two values with a body of their own — and it would verify.
//
// tolerance bounds the accepted clock skew; pass 0 for WebhookTolerance.
func VerifyWebhook(secret string, header http.Header, rawBody []byte, tolerance time.Duration) (string, error) {
	if secret == "" {
		return "", fmt.Errorf("%w: no signing secret configured", ErrWebhookRejected)
	}
	version := header.Get("X-Webhook-Payload-Version")
	timestamp := header.Get("X-Webhook-Timestamp")
	signature := header.Get("X-Webhook-Signature")
	if version != "1" && version != "2" {
		return "", fmt.Errorf("%w: unsupported payload version %q", ErrWebhookRejected, version)
	}
	if timestamp == "" || signature == "" {
		return "", fmt.Errorf("%w: missing timestamp or signature header", ErrWebhookRejected)
	}

	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return "", fmt.Errorf("%w: timestamp %q is not unix seconds", ErrWebhookRejected, timestamp)
	}

	// The taskId has to come out of an unverified payload here, but only to recompute the
	// signature: a forged id produces a signature that does not match, so it is substituted rather
	// than trusted.
	var parsed webhookEvent
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return "", fmt.Errorf("%w: body is not JSON", ErrWebhookRejected)
	}
	taskID := parsed.Data.TaskID
	if version == "1" {
		taskID = parsed.TaskID
	}
	if taskID == "" {
		return "", fmt.Errorf("%w: body carries no task ID for version %s", ErrWebhookRejected, version)
	}

	digest := sha256.Sum256(rawBody)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(taskID + "." + timestamp + "." + hex.EncodeToString(digest[:])))
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	// Constant-time comparison: an ordinary == returns at the first differing byte, and that much
	// timing difference is enough to recover the signature one byte at a time.
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return "", ErrWebhookSignature
	}

	// Freshness comes after the signature, deliberately, matching the TypeScript, Python, PHP and
	// Java clients.
	//
	// The other order would let no forgery through - both checks must pass - but it points the
	// alert the wrong way. A forger writes their own timestamp, and an old one gets reported as
	// stale; faced with a screen full of staleness, the reasonable operational response is to widen
	// the tolerance, which is exactly the knob that moves those deliveries from rejected to
	// accepted. An attack thus disguises itself as a clock problem and then gets waved through by
	// us.
	//
	// With the signature checked first, only deliveries we genuinely signed are even eligible to be
	// judged fresh, "stale" goes back to meaning nothing but clock skew, and widening the tolerance
	// becomes a safe action again.
	if tolerance <= 0 {
		tolerance = WebhookTolerance
	}
	if skew := time.Since(time.Unix(seconds, 0)); skew > tolerance || skew < -tolerance {
		return "", fmt.Errorf("%w: %s off", ErrWebhookStale, skew.Round(time.Second))
	}
	return taskID, nil
}

// request describes one call to the API surface.
type request struct {
	method  string
	path    string
	body    any
	headers map[string]string
	// replayable says whether sending these exact bytes again is safe. It
	// gates every automatic retry: a transport error does not prove the server
	// never saw the request, so anything that would create or charge twice is
	// only resent when an idempotency key makes the duplicate harmless.
	replayable bool
	out        any
	// timeout overrides the client's per-attempt deadline for this one call.
	// Zero keeps the default. It exists for requests the service is expected
	// to hold open on purpose, where the ordinary budget would have this
	// client hang up on a response that was going to arrive.
	timeout time.Duration
	// status, when set, receives the HTTP status of the successful response.
	// Two endpoints answer with two different bodies under two different
	// statuses, and the status is the discriminator the contract defines —
	// sniffing the JSON shape instead would guess.
	status *int
}

// do performs a request, decodes the envelope and retries what is worth
// retrying.
func (c *Client) do(ctx context.Context, spec request) error {
	for attempt := 0; ; attempt++ {
		status, header, body, err := c.attempt(ctx, spec)
		if err != nil {
			if ctx.Err() != nil || !spec.replayable || attempt >= c.maxRetries {
				return fmt.Errorf("spicy: %s %s: %w", spec.method, spec.path, err)
			}
			if waitErr := c.sleep(ctx, c.backoff(attempt, 0)); waitErr != nil {
				return waitErr
			}
			continue
		}

		retryAfter := parseRetryAfter(header.Get("Retry-After"), c.now())
		var env envelope
		// code is always non-zero, which makes it double as the test for "is this our envelope".
		parsed := json.Unmarshal(body, &env) == nil && env.Code != 0

		if !parsed {
			// Not an envelope - usually an HTML error page from an edge network. Only the HTTP
			// status is available to go on, because there is no business code at all.
			if spec.replayable && attempt < c.maxRetries && retryableHTTP[status] && c.canWaitFor(ctx, retryAfter) {
				if waitErr := c.sleep(ctx, c.backoff(attempt, retryAfter)); waitErr != nil {
					return waitErr
				}
				continue
			}
			return &APIError{
				Status:     status,
				Message:    fmt.Sprintf("response was not the SpicyAPI envelope: %s", snippet(body)),
				RetryAfter: retryAfter,
			}
		}

		if status/100 != 2 || env.Code != CodeSuccess {
			// A server that asks for a longer wait than this call can give gets its answer handed
			// back at once rather than slept through: see canWaitFor.
			if spec.replayable && attempt < c.maxRetries && retryableCode[env.Code] && c.canWaitFor(ctx, retryAfter) {
				if waitErr := c.sleep(ctx, c.backoff(attempt, retryAfter)); waitErr != nil {
					return waitErr
				}
				continue
			}
			return &APIError{
				Status:     status,
				Code:       env.Code,
				Message:    env.Msg,
				RequestID:  env.RequestID,
				RetryAfter: retryAfter,
				Reason:     failureReason(env.Data),
			}
		}

		if spec.status != nil {
			*spec.status = status
		}
		if spec.out == nil {
			return nil
		}
		if len(env.Data) == 0 {
			return &APIError{
				Status:    status,
				Code:      env.Code,
				Message:   "successful envelope carried no data",
				RequestID: env.RequestID,
			}
		}
		if err := json.Unmarshal(env.Data, spec.out); err != nil {
			return fmt.Errorf("spicy: decoding the %s response: %w", spec.path, err)
		}
		return nil
	}
}

// attempt performs exactly one HTTP round trip under its own deadline.
func (c *Client) attempt(ctx context.Context, spec request) (int, http.Header, []byte, error) {
	budget := c.requestTimeout
	if spec.timeout > 0 {
		budget = spec.timeout
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var payload io.Reader
	if spec.body != nil {
		raw, err := json.Marshal(spec.body)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("encoding the request body: %w", err)
		}
		payload = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, spec.method, c.baseURL+spec.path, payload)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", UserAgent)
	if spec.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Caller headers are written last, so they override any default above, including the
	// User-Agent - anyone embedding this client in their own SDK needs that.
	for name, value := range spec.headers {
		req.Header.Set(name, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Read ceiling: a broken intermediary can emit bytes indefinitely, and without a ceiling that
	// consumes the whole process.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return resp.StatusCode, resp.Header, nil, err
	}
	if len(body) > maxResponseBytes {
		return resp.StatusCode, resp.Header, nil, fmt.Errorf("response exceeded %d bytes", maxResponseBytes)
	}
	return resp.StatusCode, resp.Header, body, nil
}

// backoff waits as long as the server asked, and otherwise doubles with jitter
// so that a batch of failures does not come back as one thundering herd.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > maxRetryDelay {
			return maxRetryDelay
		}
		return retryAfter
	}
	delay := time.Duration(1<<uint(attempt)) * baseRetryDelay
	if delay <= 0 || delay > maxRetryDelay {
		delay = maxRetryDelay
	}
	return c.jitter(delay)
}

// canWaitFor reports whether a retry that honours the server's Retry-After fits
// this call.
//
// It does not when the server asks for more than maxRetryDelay, the longest
// this client ever waits between attempts, or for more than the context has
// left. Retrying early in the first case only asks a question the server has
// already answered, and in the second the wait would end in the context's own
// deadline error, which buries the server's code, Reason, RetryAfter and
// request ID under a timeout that never happened. The asset library's daily
// limit (429 with Retry-After until 00:00 UTC) is the case that made this
// visible. Shorter waits are retried as before.
func (c *Client) canWaitFor(ctx context.Context, retryAfter time.Duration) bool {
	if retryAfter <= 0 {
		return true
	}
	if retryAfter > maxRetryDelay {
		return false
	}
	// The context's deadline is on the real clock, so it is compared with the real clock rather
	// than with Options.Now.
	if deadline, ok := ctx.Deadline(); ok && retryAfter > time.Until(deadline) {
		return false
	}
	return true
}

// failureReason reads data.reason from a failure envelope. Any non-empty
// string is kept verbatim, so a reason the server adds later still reaches the
// caller; anything else - absent, null, empty or not a string - reads as no
// reason at all.
func failureReason(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var shape struct {
		Reason json.RawMessage `json:"reason"`
	}
	if json.Unmarshal(data, &shape) != nil || len(shape.Reason) == 0 {
		return ""
	}
	var reason string
	if json.Unmarshal(shape.Reason, &reason) != nil {
		return ""
	}
	return reason
}

// jitter spreads a delay over ±20 percent.
func (c *Client) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*c.rand()))
}

// parseRetryAfter reads the header in both of its documented forms: a count of
// seconds, or an HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := when.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

// sleepWithContext waits, or gives up early when the context ends.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// snippet renders the start of an unexpected body on one line, so that an edge
// network's HTML page is readable in a log without flooding it.
func snippet(body []byte) string {
	const limit = 200
	if len(body) > 4*limit {
		body = body[:4*limit]
	}
	runes := []rune(strings.Join(strings.Fields(string(body)), " "))
	if len(runes) == 0 {
		return "(empty body)"
	}
	if len(runes) > limit {
		// Cut by code point; cutting by byte would split a multi-byte character down the middle.
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}

// isLoopback reports whether a host is this machine.
func isLoopback(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	return host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.")
}

// acceptedUploadTypes is the closed set the upload ticket endpoint takes.
// TestContractHasNotDrifted pins it to the published contract.
var acceptedUploadTypes = []string{
	"image/jpeg", "image/png", "image/webp", "image/gif",
	"video/mp4", "video/webm", "audio/mpeg", "audio/wav",
	"application/pdf",
	"application/msword",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"application/vnd.ms-excel",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"application/vnd.ms-powerpoint",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"application/vnd.apple.keynote",
	"application/vnd.apple.pages",
	"application/vnd.apple.numbers",
	"text/plain",
	"text/markdown",
	"application/octet-stream",
}

func isAcceptedUploadType(contentType string) bool {
	for _, accepted := range acceptedUploadTypes {
		if accepted == contentType {
			return true
		}
	}
	return false
}
