<div align="center">

# spicy-go

**Official Go SDK for [SpicyAPI](https://spicyapi.ai)** — image, video, audio and text models behind one API.

[Get a key](https://spicyapi.ai) · [Models](https://spicyapi.ai/models) · [Docs](https://docs.spicyapi.ai) · [Status](https://status.spicyapi.ai)

</div>

---

One endpoint in front of 142 model families across 214 callable endpoints, billed in USD per request
rather than in credits. Media generation is asynchronous and quotable before you spend; text models
speak the OpenAI, Anthropic and Gemini wire formats.

```bash
go get github.com/spicyapi-ai/spicy-go
```

Requires Go 1.23+. **Standard library only** — this package goes into your dependency graph, and
every module it adds is one more thing for you to audit.

## Generate something

```go
client, err := spicy.New(spicy.Options{}) // reads SPICY_API_KEY from the environment
if err != nil {
    return err
}

request := spicy.CreateTaskInput{
    Model: "MODEL_ID_FROM_CATALOG", // copy a real id from ListModels
    Input: map[string]any{"prompt": "a lantern in fog"},
}

quote, err := client.Quote(ctx, request)
if err != nil {
    return err
}
log.Printf("this will cost at most %s", quote.MaxCharge)

// Store this next to your own record of the job before sending it: its whole
// purpose is to still be there after the crash that lost the response.
key, err := spicy.NewIdempotencyKey()
if err != nil {
    return err
}

request.Model = quote.Model
request.QuoteID = quote.QuoteID
request.ExpectedCost = quote.EstimatedCost

task, err := client.CreateTask(ctx, request, key, spicy.CreateTaskOptions{})
if err != nil {
    return err
}

final, err := client.WaitForTerminal(ctx, task.TaskID, spicy.WaitOptions{})
if err != nil {
    return err
}
for _, asset := range final.ReadyAssets() {
    log.Println(asset.URL)
}
```

Build `Input` from that model's own `inputSchema` — every model has different fields, and
`ListModels` returns them.

## Skipping the first poll

`WaitSeconds` asks the service to hold the connection open until the task finishes, which on a
quick model means the result arrives with the submission.

```go
seconds := 30
task, err := client.CreateTask(ctx, request, key, spicy.CreateTaskOptions{WaitSeconds: &seconds})
if err != nil {
    return err
}

// Task is nil whenever the wait ran out. That is the ordinary outcome, not an
// error — the generation is still running, so poll from here.
final := task.Task
if final == nil {
    final, err = client.WaitForTerminal(ctx, task.TaskID, spicy.WaitOptions{})
    if err != nil {
        return err
    }
}
log.Println(final.State, final.Cost)
```

**It is an optimisation, never a guarantee.** Code that reads `task.Task` without the nil check
works right up until the first generation slower than the budget, and then panics in production.

The value is sent exactly as given: the service clamps anything above its ceiling and *ignores*
— rather than rejects — a value that is not a positive integer, so a typo here cannot fail a
generation that would otherwise succeed. This client enforces no ceiling of its own, and lifts
that one request's deadline to cover the wait.

A terminal state is not a successful one. `failed`, `canceled` and `expired` all end the wait;
check `final.State`.

## Start from a local file

Image-to-video, face swap and image editing need your material on our side first. Upload returns a
`spicy://` URI; that is what goes into `Input`.

```go
uploaded, err := client.UploadFile(ctx, "/path/to/reference.png", "")
// uploaded.URI -> "spicy://f/fil_..."
```

Images, MP4/WebM video, MP3/WAV audio and reference documents (PDF, Word, Excel, PowerPoint,
Pages, Numbers, plain text, Markdown) are accepted. The content type is inferred from the extension,
except for Keynote: `.key` is also how private keys are named, so pass
`"application/vnd.apple.keynote"` yourself rather than have a key file uploaded on a guess.

## Balance, usage and history

All three are scoped to **this API key**: sibling keys on the same account, and
generations started in the web console, are not included, and their absence is not a fault.

```go
balance, err := client.GetBalance(ctx)
if err != nil {
    return err
}
log.Printf("%s available, %s held by accepted tasks", balance.Available, balance.Held)

usage, err := client.GetUsage(ctx, spicy.UsageOptions{From: "2026-09-01", To: "2026-09-08"})
if err != nil {
    return err
}
log.Printf("%d calls, %s settled over [%s,%s)", usage.TotalCalls, usage.TotalSpend, usage.From, usage.To)

// EachTask repeats every filter on every page and moves only the cursor.
if err := client.EachTask(ctx, spicy.ListTasksOptions{From: "2026-09-01", To: "2026-09-08"},
    func(task spicy.TaskListItem) bool {
        log.Println(task.TaskID, task.State, task.Cost)
        return true // return false to stop early
    }); err != nil {
    return err
}
```

Amounts are decimal USD strings, never floats. Dates are UTC calendar dates in a half-open
interval `[from, to)` of at most 92 days.

**Pin `From` and `To` on any walk that can cross midnight UTC.** Left empty the server
recomputes its own window — `to` is tomorrow in UTC — on every page, so a walk that starts at
23:59 and continues at 00:01 asks two different questions and splices the answers together.
Nothing reports it.

`GetUsage` is for reconciliation, not for progress. It sits behind an account-wide budget of
thirty requests a minute shared by every key, and that limiter fails closed — polling it can
lock every other key out of its own reporting. Follow a task with `WaitForTerminal`. Settlement
can also land late, so a figure read today can still change tomorrow.

## Errors

Failures come back as `*spicy.APIError`, reachable with `errors.As`:

```go
var apiErr *spicy.APIError
if errors.As(err, &apiErr) {
    log.Println(apiErr.Status, apiErr.Code, apiErr.RequestID, apiErr.RetryAfter)
}
```

Branch on `Code`, never on the message — messages are translated, codes are not. **HTTP 503 is
shared by three different business codes**, so the status alone is not enough to decide what to do:

| Code | Meaning | What to do |
| --- | --- | --- |
| `40003` | uploaded bytes do not match their ticket | upload again |
| `40004` | no deployment serves that parameter combination | change the named parameter |
| `40310` | the account's email address is not verified yet | open the verification link; the same key then works |
| `40901` | the price moved before the task was created | quote again, keep the same idempotency key |
| `503` | a dependency is briefly unavailable | back off by `RetryAfter` |
| `50301` | no usable deployment or price right now | refresh the catalogue, do not hammer |
| `50302` | a synchronous generation failed upstream and was refunded | retry with a **new** idempotency key |

## Two things that will save you money

**Keep one idempotency key per submission.** Reuse it for every resend, including after a timeout.
A lost response does not prove the task was not created — a fresh key turns an unknown outcome into
a second paid task. `CreateTask` requires the key for exactly this reason.

**A task that succeeds is charged, even if the result disappoints.** `Quote` reserves nothing, so
quote first when the price matters. Failures are normally free, with one exception: on a model whose
page says content-policy refusals are billed, a task that fails with `content_rejected` settles at
the estimate it was accepted at. Read `Cost` once `Settled` is true rather than assuming.

## What this package does not do

**Text models.** They speak the OpenAI, Anthropic and Gemini wire formats — the established Go
clients for those already work against `https://api.spicyapi.ai/v1` with the same key. One part of
this package still helps there: chat models that read images, video or audio take the `spicy://f/`
URI from `UploadFile` in an `image_url`, `video_url` or `audio_url` message part. Which parts a
model accepts is in its input schema; see
[sending media to a text model](https://docs.spicyapi.ai/docs/text-and-streaming#media-input).

**Client-side applications.** Never ship this key inside a binary you distribute. Call from your
server.

## Links

- [Documentation](https://docs.spicyapi.ai)
- [API reference](https://docs.spicyapi.ai/docs/api-reference)

---

<div align="center">
<sub>

Also available in [TypeScript](https://github.com/spicyapi-ai/spicy-sdk) · [Python](https://github.com/spicyapi-ai/spicy-python) · **Go** · [PHP](https://github.com/spicyapi-ai/spicy-php) · [Java](https://github.com/spicyapi-ai/spicy-java)

</sub>
</div>
