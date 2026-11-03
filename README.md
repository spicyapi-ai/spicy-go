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

This README describes v0.4.0; see the [changelog](CHANGELOG.md) for what changed.

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

Not every result is a file. Speech-to-text models answer in `Output.Text`, with `Output.Transcript`
(words, timed `Segments`, language) when the model reports more; embedding models answer in
`Output.Embeddings`, one vector per input in input order. Image-to-3D and text-to-3D models return
one asset with role `mesh`, a `.glb` file.

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
Pages, Numbers, plain text, Markdown) are accepted, and so are LoRA weights: a `.safetensors` file,
sent as `application/octet-stream` and up to 2 GiB, whose URI goes into `loras[].path`. The file is
streamed from disk, but a file that large can outlast the default ten-minute upload timeout, so
raise `Options.UploadTimeout` for big ones.

The content type is inferred from the extension, except for Keynote: `.key` is also how private
keys are named, so pass `"application/vnd.apple.keynote"` yourself rather than have a key file
uploaded on a guess.

An upload's URI lasts one day. To keep a file, save it to the asset library.

## Asset library

Files you use again and again — a character portrait, a style reference, a voice sample — belong in
your asset library: each saved file gets a stable `URI` of the form `spicy://a/<id>` that works in
any media field of model inputs sent with keys of the same workspace, until it is deleted. The
library never hands out a public or permanent link.

The key decides the library. A key created in your personal workspace reaches your personal library;
a key created in a team workspace, by any member, reaches that team's shared library and never
anyone's personal one. With a team key, what may change follows the current role of the member who
created it: owner and admin can save, rename, favorite and delete anything; developer can save, and
rename, favorite or delete what they saved; billing and readonly can only read and use assets.

```go
// Save the first output of one of your successful tasks (with a team key, one of the team's).
// AssetSourceURL with URL, and AssetSourceUpload with FileID, work the same way. The required
// Idempotency-Key is minted for you; pass your own as the last argument to replay a save.
index := 0
hero, err := client.SaveAsset(ctx, spicy.SaveAssetInput{
    Source: spicy.AssetSourceTaskOutput,
    TaskID: "job_...",
    Index:  &index,
    Name:   "Hero portrait",
}, "")
if err != nil {
    return err
}

// Or upload a local file and save it in one call; the name defaults to the file name.
voice, err := client.UploadAssetFile(ctx, "./voice-sample.wav", spicy.UploadAssetOptions{})
if err != nil {
    return err
}
log.Println(voice.URI)

// Later, in any request: the URI goes where the model's schema expects a file.
input := map[string]any{"prompt": "Slow push-in, light rain", "image_url": hero.URI}

// Every saved image, newest first, across all pages.
err = client.EachAsset(ctx, spicy.ListAssetsOptions{Kind: "image", Q: "portrait"},
    func(asset spicy.LibraryAsset) bool {
        log.Println(asset.URI, asset.Name, asset.Bytes)
        return true // return false to stop early
    })
if err != nil {
    return err
}
```

| Method | Returns |
| --- | --- |
| `SaveAsset(ctx, SaveAssetInput{...}, idempotencyKey)` | The saved `*LibraryAsset`: `ID`, `URI`, `Name`, `Kind`, `Bytes`, `Favorite`, `Model`, `PreviewURL`, … |
| `UploadAssetFile(ctx, path, UploadAssetOptions{Name, ContentType})` | Uploads with `UploadFile`, then saves; the saved asset |
| `ListAssets(ctx, ListAssetsOptions{...})` | One page: `Items`, `NextCursor` (empty on the last page) |
| `EachAsset(ctx, ListAssetsOptions{...}, fn)` | Calls `fn` for every matching asset, page by page |
| `GetAsset(ctx, assetID)` | One asset with a fresh `PreviewURL` |
| `UpdateAsset(ctx, assetID, UpdateAssetInput{Name, Favorite})` | The updated asset; only what you set changes, the `URI` stays the same |
| `DeleteAsset(ctx, assetID)` | `*DeletedAsset`; the `URI` stops working at once |
| `GetAssetUsage(ctx)` | `Scope` (which library, role, `CanWrite`, `CanTopUp`), plan, usage and limits |
| `ListCharacters(ctx, ListCharactersOptions{AssetID, Cursor, Limit})` / `EachCharacter(...)` | Characters, newest first, each with its images in order |
| `GetCharacter(ctx, characterID)` | One character with its images in order; `URIs()` lists their references |

- `assetID` may be the bare `ast_…` ID or the `spicy://a/…` reference. An empty ID, a blank filter,
  an unknown `Sort` and a save that mixes two sources fail before anything is sent.
- `Source` takes several values (`AssetSourceTaskOutput`, `AssetSourceUpload`, `AssetSourceURL`).
  `Favorite` is a `*bool`: `true` lists only favorites, `false` only the rest, `nil` both.
- `Sort` is `AssetSortRecent` (the default, newest first), `AssetSortOldest`, `AssetSortName` or
  `AssetSortSize`. A `NextCursor` only works with the `Sort` it came from; sending it with another
  one fails with `400`, which is returned as-is and not retried. `EachAsset` keeps your filters and
  `Sort` on every page.
- Saving the same task output, upload or URL again returns the asset you already have. Pass your
  own idempotency key to replay a save safely after an uncertain response, for example across a
  restart.
- `PreviewURL` is a private preview signed for at most 20 minutes. Show it; store `URI`.
- Storage unlocks with the owner's lifetime top-ups, and the owner's personal and team libraries
  share one allowance. `GetAssetUsage` reports the whole allowance; with a team key whose creator is
  not the owner, `Scope.CanTopUp` is false and `LifetimeTopupUSD` and `TopupToNextTierUSD` are
  empty.
- A refused call returns `*spicy.APIError` with a `Reason`: `ReasonAssetLibraryLocked`,
  `ReasonAssetLibraryFull` and `ReasonAssetFileTooLarge` (400, plan limits — read `GetAssetUsage`
  for the numbers), `ReasonAssetDailyLimit` (429, resets at 00:00 UTC, with `RetryAfter`),
  `ReasonAssetStorageFull` (503, temporary), `ReasonAssetPermissionDenied` (403, the team role does
  not allow this change), `ReasonAssetWorkspaceForbidden` (403, this key cannot reach that library,
  for example because its creator left the team) and `ReasonAssetEntityNameTaken` (409, a character
  with that name already exists). `IsAssetErrorReason` tells them apart; `Reason` carries any other
  value the server sends unchanged, so keep a default branch.

### Favorites, folders and characters

Every asset has a `Favorite` flag; in a team library everyone sees the same favorites. Folders
(collections in the API) and characters are created and arranged on the Assets page of the web app;
the API reads them. Each asset also carries `Model` and `ModelName`, the model that generated it
(empty for uploads and links).

```go
// Mark a favorite, then list only favorites in one folder, A to Z.
favorite := true
if _, err := client.UpdateAsset(ctx, hero.URI, spicy.UpdateAssetInput{Favorite: &favorite}); err != nil {
    return err
}
page, err := client.ListAssets(ctx, spicy.ListAssetsOptions{
    Favorite:     &favorite,
    CollectionID: "col_...",
    Sort:         spicy.AssetSortName,
})
if err != nil {
    return err
}

// A character is a named, ordered set of images. Put its images, in order, into the model's
// reference image field (GetModel shows which field takes a list of images).
mia, err := client.GetCharacter(ctx, "ety_...")
if err != nil {
    return err
}
references := mia.URIs() // ["spicy://a/ast_...", ...]
```

An unknown folder, and an unknown character, both fail with `404`. Characters used to be called
personas: existing `spicy://p/<id>` references keep resolving to the same images, in your personal
library only; new code should use the `spicy://a/` references a character lists.

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
    log.Println(apiErr.Status, apiErr.Code, apiErr.Reason, apiErr.RequestID, apiErr.RetryAfter)
}
```

Branch on `Code`, or on `Reason` when the server sends one (see [Asset library](#asset-library)),
never on the message — messages are translated, codes are not. A retryable failure whose
`RetryAfter` is longer than this client waits between attempts, or than your context has left, is
returned at once with that `RetryAfter`, rather than slept through into a timeout. **HTTP 503 is
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
The same goes for the OpenAI-compatible `POST /v1/embeddings` and `POST /v1/audio/speech` routes and
the realtime WebSocket at `wss://api.spicyapi.ai/v1/realtime`; see the
[realtime guide](https://docs.spicyapi.ai/docs/realtime-audio).

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
