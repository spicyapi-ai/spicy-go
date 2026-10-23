package spicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every request in this file goes to a local httptest server; nothing reaches the network. Most
// behaviours have a counter-case next to them, because the failure modes of a list filter or an ID
// check are silent: a dropped favorite=false returns the unfiltered library, and an empty ID lists
// the collection instead of reading one asset.

// scripted is one canned answer of scriptedAPI.
type scripted struct {
	status int
	header map[string]string
	body   string
}

func okData(data string) scripted {
	return scripted{status: 200, body: `{"code":200,"msg":"success","request_id":"req_ok","data":` + data + `}`}
}

func failed(status, code int, data string, header map[string]string) scripted {
	body := fmt.Sprintf(`{"code":%d,"msg":"refused by the asset library","request_id":"req_fail"`, code)
	if data != "" {
		body += `,"data":` + data
	}
	return scripted{status: status, header: header, body: body + "}"}
}

// seenRequest is what scriptedAPI recorded about one request.
type seenRequest struct {
	method string
	uri    string
	header http.Header
	body   string
}

func (r seenRequest) query() url.Values {
	parsed, _ := url.ParseRequestURI(r.uri)
	return parsed.Query()
}

func (r seenRequest) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatalf("request body is not JSON: %q", r.body)
	}
	return out
}

// scriptedAPI replays its answers in order and records every request. Running out of answers is a
// test failure, which is how "nothing more was sent" is asserted.
type scriptedAPI struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	answers []scripted
	seen    []seenRequest
	sleeps  []time.Duration
}

func newScriptedAPI(t *testing.T, answers ...scripted) *scriptedAPI {
	t.Helper()
	api := &scriptedAPI{t: t, answers: answers}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		defer api.mu.Unlock()
		api.seen = append(api.seen, seenRequest{method: r.Method, uri: r.URL.RequestURI(), header: r.Header.Clone(), body: string(raw)})
		if len(api.answers) == 0 {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.RequestURI())
			w.WriteHeader(http.StatusTeapot)
			return
		}
		answer := api.answers[0]
		api.answers = api.answers[1:]
		for name, value := range answer.header {
			w.Header().Set(name, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		_, _ = w.Write([]byte(answer.body))
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *scriptedAPI) client(t *testing.T) *Client {
	t.Helper()
	client, err := New(Options{
		APIKey:  "sk-test",
		BaseURL: a.server.URL,
		Sleep: func(ctx context.Context, d time.Duration) error {
			a.mu.Lock()
			a.sleeps = append(a.sleeps, d)
			a.mu.Unlock()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	return client
}

func (a *scriptedAPI) requests() []seenRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]seenRequest(nil), a.seen...)
}

func (a *scriptedAPI) sleepCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sleeps)
}

const assetJSON = `{"id":"ast_1","uri":"spicy://a/ast_1","name":"Hero portrait","kind":"image",
	"contentType":"image/png","bytes":2048,"width":1024,"height":768,"source":"task_output",
	"sourceId":"job_1","createdAt":"2026-10-12T10:00:00Z","updatedAt":"2026-10-12T10:00:00Z",
	"previewUrl":"https://preview.example/ast_1","previewUrlExpiresAt":"2026-10-12T10:20:00Z",
	"favorite":false,"model":"example/model/text-to-image","modelName":"Example Model"}`

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// -- Saving ----------------------------------------------------------------

func TestSaveAssetPostsTheSourceAndMintsAnIdempotencyKey(t *testing.T) {
	api := newScriptedAPI(t, okData(assetJSON))
	asset, err := api.client(t).SaveAsset(context.Background(), SaveAssetInput{
		Source: AssetSourceTaskOutput, TaskID: " job_1 ", Index: intPtr(0), Name: "Hero portrait",
	}, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if asset.URI != "spicy://a/ast_1" || asset.Width != 1024 || asset.Model != "example/model/text-to-image" {
		t.Errorf("decoded asset = %+v", asset)
	}

	seen := api.requests()
	if len(seen) != 1 || seen[0].method != http.MethodPost || seen[0].uri != "/assets" {
		t.Fatalf("requests = %+v", seen)
	}
	body := seen[0].json(t)
	want := map[string]any{"source": "task_output", "taskId": "job_1", "index": float64(0), "name": "Hero portrait"}
	if fmt.Sprint(body) != fmt.Sprint(want) {
		t.Errorf("body = %v, want %v (index 0 must be sent, not dropped as a zero value)", body, want)
	}
	if key := seen[0].header.Get("Idempotency-Key"); !strings.HasPrefix(key, "spicy-") || len(key) < 20 {
		t.Errorf("Idempotency-Key = %q, want a minted key", key)
	}
}

func TestSaveAssetSendsOnlyTheChosenSourcesFields(t *testing.T) {
	cases := []struct {
		input SaveAssetInput
		want  string
	}{
		{SaveAssetInput{Source: AssetSourceUpload, FileID: "spicy://f/fil_1"}, `map[fileId:spicy://f/fil_1 source:upload]`},
		{SaveAssetInput{Source: AssetSourceURL, URL: "https://example.com/still.png"}, `map[source:url url:https://example.com/still.png]`},
		{SaveAssetInput{Source: AssetSourceTaskOutput, TaskID: "job_1", Key: "out/0.png"}, `map[key:out/0.png source:task_output taskId:job_1]`},
	}
	for _, tc := range cases {
		api := newScriptedAPI(t, okData(assetJSON))
		if _, err := api.client(t).SaveAsset(context.Background(), tc.input, ""); err != nil {
			t.Fatalf("save %s: %v", tc.input.Source, err)
		}
		if got := fmt.Sprint(api.requests()[0].json(t)); got != tc.want {
			t.Errorf("source %s sent %s, want %s", tc.input.Source, got, tc.want)
		}
	}
}

func TestSaveAssetSendsTheCallersKeyUnchanged(t *testing.T) {
	api := newScriptedAPI(t, okData(assetJSON))
	if _, err := api.client(t).SaveAsset(context.Background(),
		SaveAssetInput{Source: AssetSourceURL, URL: "https://example.com/a.png"}, "my-save-1"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if key := api.requests()[0].header.Get("Idempotency-Key"); key != "my-save-1" {
		t.Errorf("Idempotency-Key = %q, want the caller's", key)
	}
}

// A resend after a retryable failure must carry the same key, or the server cannot fold the two
// attempts into one save.
func TestSaveAssetRetryReplaysTheSameIdempotencyKey(t *testing.T) {
	api := newScriptedAPI(t, failed(503, 503, "", nil), okData(assetJSON))
	if _, err := api.client(t).SaveAsset(context.Background(),
		SaveAssetInput{Source: AssetSourceUpload, FileID: "fil_1"}, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	seen := api.requests()
	if len(seen) != 2 {
		t.Fatalf("want one retry, got %d requests", len(seen))
	}
	first, second := seen[0].header.Get("Idempotency-Key"), seen[1].header.Get("Idempotency-Key")
	if first == "" || first != second {
		t.Errorf("keys differ between attempts: %q then %q", first, second)
	}
}

func TestMalformedSavesAreRefusedBeforeAnyRequest(t *testing.T) {
	api := newScriptedAPI(t)
	client := api.client(t)
	cases := map[string]SaveAssetInput{
		"unknown source":       {Source: "disk"},
		"no source":            {URL: "https://example.com/a.png"},
		"two sources":          {Source: AssetSourceURL, URL: "https://example.com/a.png", FileID: "fil_1"},
		"task field on upload": {Source: AssetSourceUpload, FileID: "fil_1", Index: intPtr(0)},
		"missing task":         {Source: AssetSourceTaskOutput, Index: intPtr(0)},
		"negative index":       {Source: AssetSourceTaskOutput, TaskID: "job_1", Index: intPtr(-1)},
		"blank key":            {Source: AssetSourceTaskOutput, TaskID: "job_1", Key: "  "},
		"blank file":           {Source: AssetSourceUpload, FileID: "  "},
		"blank url":            {Source: AssetSourceURL, URL: " "},
	}
	for name, input := range cases {
		if _, err := client.SaveAsset(context.Background(), input, ""); err == nil {
			t.Errorf("%s: expected a local error", name)
		}
	}
	if _, err := client.SaveAsset(context.Background(),
		SaveAssetInput{Source: AssetSourceURL, URL: "https://example.com/a.png"}, "bad\nkey"); err == nil {
		t.Error("a key with a line break was accepted")
	}
	if seen := api.requests(); len(seen) != 0 {
		t.Errorf("malformed saves reached the API: %+v", seen)
	}
}

// -- Listing ---------------------------------------------------------------

func TestListAssetsEncodesEveryFilter(t *testing.T) {
	api := newScriptedAPI(t, okData(`{"items":[`+assetJSON+`],"nextCursor":"c2"}`))
	page, err := api.client(t).ListAssets(context.Background(), ListAssetsOptions{
		Kind: "image", Source: []string{AssetSourceTaskOutput, AssetSourceURL}, Q: "portrait",
		Favorite: boolPtr(true), CollectionID: "col_1", EntityID: "ety_1", Sort: AssetSortName,
		Cursor: "c1", Limit: 25,
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 || page.NextCursor != "c2" {
		t.Errorf("page = %+v", page)
	}
	got := api.requests()[0]
	if !strings.HasPrefix(got.uri, "/assets?") || got.method != http.MethodGet {
		t.Fatalf("request = %s %s", got.method, got.uri)
	}
	want := url.Values{
		"kind": {"image"}, "source": {"task_output,url"}, "q": {"portrait"}, "favorite": {"true"},
		"collectionId": {"col_1"}, "entityId": {"ety_1"}, "sort": {"name"}, "cursor": {"c1"}, "limit": {"25"},
	}
	if got.query().Encode() != want.Encode() {
		t.Errorf("query = %s, want %s", got.query().Encode(), want.Encode())
	}
}

// favorite=false is a filter of its own ("not favorites"); only nil leaves it off.
func TestFavoriteFalseIsSentAndNilIsOmitted(t *testing.T) {
	api := newScriptedAPI(t, okData(`{"items":[]}`), okData(`{"items":[]}`))
	client := api.client(t)
	if _, err := client.ListAssets(context.Background(), ListAssetsOptions{Favorite: boolPtr(false)}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := client.ListAssets(context.Background(), ListAssetsOptions{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	seen := api.requests()
	if seen[0].uri != "/assets?favorite=false" {
		t.Errorf("favorite=false went out as %s", seen[0].uri)
	}
	if seen[1].uri != "/assets" {
		t.Errorf("an empty options struct went out as %s", seen[1].uri)
	}
}

func TestInvalidListFiltersSendNothing(t *testing.T) {
	api := newScriptedAPI(t)
	client := api.client(t)
	for name, opts := range map[string]ListAssetsOptions{
		"unknown sort":      {Sort: "newest"},
		"negative limit":    {Limit: -1},
		"empty source":      {Source: []string{AssetSourceUpload, ""}},
		"blank collection":  {CollectionID: "  "},
		"line break entity": {EntityID: "ety\n_1"},
	} {
		if _, err := client.ListAssets(context.Background(), opts); err == nil {
			t.Errorf("%s: expected a local error", name)
		}
		if err := client.EachAsset(context.Background(), opts, func(LibraryAsset) bool { return true }); err == nil {
			t.Errorf("%s: EachAsset expected a local error", name)
		}
	}
	if seen := api.requests(); len(seen) != 0 {
		t.Errorf("invalid filters reached the API: %+v", seen)
	}
}

// A cursor sent with another sort is the server's 400 to give, and it is a verdict, not a blip.
func TestARejectedCursorIsReturnedAsIsAndNotRetried(t *testing.T) {
	api := newScriptedAPI(t, failed(400, 400, "", nil))
	_, err := api.client(t).ListAssets(context.Background(), ListAssetsOptions{Sort: AssetSortSize, Cursor: "from-recent"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("want the server's 400, got %v", err)
	}
	if n := len(api.requests()); n != 1 {
		t.Errorf("a 400 was retried: %d requests", n)
	}
}

func TestEachAssetFollowsTheCursorAndRepeatsTheFilters(t *testing.T) {
	api := newScriptedAPI(t,
		okData(`{"items":[{"id":"ast_1"},{"id":"ast_2"}],"nextCursor":"c2"}`),
		okData(`{"items":[{"id":"ast_3"}],"nextCursor":"c3"}`),
		okData(`{"items":[{"id":"ast_4"}]}`),
	)
	var ids []string
	err := api.client(t).EachAsset(context.Background(),
		ListAssetsOptions{Kind: "image", Sort: AssetSortOldest, Favorite: boolPtr(false), Limit: 2},
		func(asset LibraryAsset) bool { ids = append(ids, asset.ID); return true })
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if strings.Join(ids, ",") != "ast_1,ast_2,ast_3,ast_4" {
		t.Errorf("walked %v", ids)
	}
	seen := api.requests()
	cursors := []string{"", "c2", "c3"}
	for i, request := range seen {
		query := request.query()
		if query.Get("cursor") != cursors[i] {
			t.Errorf("page %d cursor = %q, want %q", i, query.Get("cursor"), cursors[i])
		}
		query.Del("cursor")
		if query.Encode() != "favorite=false&kind=image&limit=2&sort=oldest" {
			t.Errorf("page %d dropped a filter: %s", i, query.Encode())
		}
	}
}

func TestEachAssetStartsFromAGivenCursorAndStopsWhenAsked(t *testing.T) {
	api := newScriptedAPI(t, okData(`{"items":[{"id":"ast_1"},{"id":"ast_2"}],"nextCursor":"c2"}`))
	var ids []string
	err := api.client(t).EachAsset(context.Background(), ListAssetsOptions{Cursor: "c1"},
		func(asset LibraryAsset) bool { ids = append(ids, asset.ID); return false })
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("fn returned false but the walk went on: %v", ids)
	}
	if seen := api.requests(); len(seen) != 1 || seen[0].uri != "/assets?cursor=c1" {
		t.Errorf("requests = %+v", seen)
	}
}

func TestEachAssetRefusesACursorThatRepeats(t *testing.T) {
	api := newScriptedAPI(t,
		okData(`{"items":[{"id":"ast_1"}],"nextCursor":"loop"}`),
		okData(`{"items":[{"id":"ast_2"}],"nextCursor":"loop"}`),
	)
	err := api.client(t).EachAsset(context.Background(), ListAssetsOptions{}, func(LibraryAsset) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "already returned") {
		t.Fatalf("a repeating cursor was followed: %v", err)
	}
	if n := len(api.requests()); n != 2 {
		t.Errorf("want two pages before giving up, got %d", n)
	}
}

func TestEachAssetReturnsAFailedPage(t *testing.T) {
	api := newScriptedAPI(t,
		okData(`{"items":[{"id":"ast_1"}],"nextCursor":"c2"}`),
		failed(400, 400, "", nil),
	)
	err := api.client(t).EachAsset(context.Background(), ListAssetsOptions{}, func(LibraryAsset) bool { return true })
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("want the failed page's error, got %v", err)
	}
}

// -- One asset ---------------------------------------------------------------

func TestOneAssetIsAddressedByIDOrReference(t *testing.T) {
	api := newScriptedAPI(t,
		okData(assetJSON),
		okData(assetJSON),
		okData(assetJSON),
		okData(`{"id":"ast_1","deleted":true}`),
	)
	client := api.client(t)
	ctx := context.Background()
	if _, err := client.GetAsset(ctx, "ast_1"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := client.GetAsset(ctx, " spicy://a/ast_1 "); err != nil {
		t.Fatalf("get by reference: %v", err)
	}
	if _, err := client.UpdateAsset(ctx, "spicy://a/ast_1", UpdateAssetInput{Favorite: boolPtr(false)}); err != nil {
		t.Fatalf("update: %v", err)
	}
	deleted, err := client.DeleteAsset(ctx, "ast_1")
	if err != nil || !deleted.Deleted || deleted.ID != "ast_1" {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}

	seen := api.requests()
	want := []string{"GET /assets/ast_1", "GET /assets/ast_1", "PATCH /assets/ast_1", "DELETE /assets/ast_1"}
	for i, request := range seen {
		if got := request.method + " " + request.uri; got != want[i] {
			t.Errorf("request %d = %s, want %s", i, got, want[i])
		}
	}
	// Unmarking a favorite is a change, not an empty update.
	if seen[2].body != `{"favorite":false}` {
		t.Errorf("update body = %s", seen[2].body)
	}
	if seen[3].header.Get("Idempotency-Key") != "" {
		t.Error("delete sent an Idempotency-Key it does not need")
	}
}

func TestUpdateAssetSendsOnlyWhatIsSet(t *testing.T) {
	api := newScriptedAPI(t, okData(assetJSON))
	if _, err := api.client(t).UpdateAsset(context.Background(), "ast_1", UpdateAssetInput{Name: "Renamed"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if body := api.requests()[0].body; body != `{"name":"Renamed"}` {
		t.Errorf("update body = %s", body)
	}
}

func TestEmptyIDsAndEmptyUpdatesSendNothing(t *testing.T) {
	api := newScriptedAPI(t)
	client := api.client(t)
	ctx := context.Background()
	for _, id := range []string{"", "  ", "spicy://a/", "spicy://a/ ", "ast\r\n_1"} {
		if _, err := client.GetAsset(ctx, id); err == nil {
			t.Errorf("GetAsset(%q) was sent", id)
		}
		if _, err := client.DeleteAsset(ctx, id); err == nil {
			t.Errorf("DeleteAsset(%q) was sent", id)
		}
	}
	if _, err := client.UpdateAsset(ctx, "ast_1", UpdateAssetInput{}); err == nil {
		t.Error("an update with nothing to change was sent")
	}
	if _, err := client.GetCharacter(ctx, " "); err == nil {
		t.Error("an empty character ID was sent")
	}
	if seen := api.requests(); len(seen) != 0 {
		t.Errorf("requests reached the API: %+v", seen)
	}
}

// An ID is one path segment however odd it is, so it cannot address another route.
func TestAssetIDsArePathEscaped(t *testing.T) {
	api := newScriptedAPI(t, okData(assetJSON))
	if _, err := api.client(t).GetAsset(context.Background(), "ast_1/../usage"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if uri := api.requests()[0].uri; uri != "/assets/ast_1%2F..%2Fusage" {
		t.Errorf("request URI = %s", uri)
	}
}

func TestAnUnknownAsset404IsReturnedAndNotRetried(t *testing.T) {
	api := newScriptedAPI(t, failed(404, 404, "", nil))
	_, err := api.client(t).GetAsset(context.Background(), "ast_missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 404 || apiErr.Reason != "" {
		t.Fatalf("want a plain 404, got %v", err)
	}
	if n := len(api.requests()); n != 1 {
		t.Errorf("a 404 was retried: %d requests", n)
	}
}

func TestGetAssetUsageReadsThePlanAndScope(t *testing.T) {
	api := newScriptedAPI(t, okData(`{
		"tier":{"id":"plus","name":"Plus","minTopupUsd":"25","maxBytes":524288000,"maxCount":500,"maxFileBytes":104857600},
		"usage":{"count":12,"bytes":1048576},
		"limits":{"tier":"plus","maxCount":500,"maxBytes":524288000,"maxFileBytes":104857600,"globalMaxBytes":0,"nameMaxChars":80},
		"full":false,"nextTier":null,
		"tiers":[{"id":"free","name":"Free","minTopupUsd":"0","maxBytes":0,"maxCount":0,"maxFileBytes":0}],
		"dailyCreateLimit":200,"createdToday":3,
		"scope":{"workspaceId":"ws_team","personal":false,"role":"developer","canWrite":true,"canTopUp":false}}`))
	usage, err := api.client(t).GetAssetUsage(context.Background())
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.Tier.Name != "Plus" || usage.Usage.Count != 12 || usage.Limits.NameMaxChars != 80 ||
		usage.DailyCreateLimit != 200 || usage.CreatedToday != 3 || len(usage.Tiers) != 1 {
		t.Errorf("usage = %+v", usage)
	}
	if usage.NextTier != nil {
		t.Errorf("a null nextTier decoded as %+v", usage.NextTier)
	}
	// With canTopUp false the two amounts are omitted, and must read as empty rather than "0".
	if usage.Scope.Role != "developer" || usage.Scope.CanTopUp || !usage.Scope.CanWrite ||
		usage.LifetimeTopupUSD != "" || usage.TopupToNextTierUSD != "" {
		t.Errorf("scope = %+v, lifetime %q, next %q", usage.Scope, usage.LifetimeTopupUSD, usage.TopupToNextTierUSD)
	}
	if uri := api.requests()[0].uri; uri != "/assets/usage" {
		t.Errorf("request URI = %s", uri)
	}
}

// -- Characters --------------------------------------------------------------

const characterJSON = `{"id":"ety_1","kind":"character","name":"Mia","note":"","coverAssetId":"ast_2",
	"cover":{"id":"ast_2","uri":"spicy://a/ast_2"},
	"assets":[{"id":"ast_2","uri":"spicy://a/ast_2"},{"id":"ast_1","uri":"spicy://a/ast_1"}],
	"count":2,"createdAt":"2026-10-12T10:00:00Z","updatedAt":"2026-10-12T10:00:00Z"}`

func TestCharactersAreListedAndReadWithTheirImagesInOrder(t *testing.T) {
	api := newScriptedAPI(t,
		okData(`{"items":[`+characterJSON+`]}`),
		okData(characterJSON),
	)
	client := api.client(t)
	page, err := client.ListCharacters(context.Background(), ListCharactersOptions{AssetID: "spicy://a/ast_1", Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Items) != 1 || page.NextCursor != "" {
		t.Errorf("page = %+v", page)
	}
	mia, err := client.GetCharacter(context.Background(), "ety_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := strings.Join(mia.URIs(), ","); got != "spicy://a/ast_2,spicy://a/ast_1" {
		t.Errorf("URIs = %s, want the images in their order", got)
	}
	if mia.Cover == nil || mia.Cover.ID != "ast_2" {
		t.Errorf("cover = %+v", mia.Cover)
	}

	seen := api.requests()
	if seen[0].uri != "/assets/entities?assetId=ast_1&limit=10" {
		t.Errorf("list URI = %s (the reference must be sent as the bare ID)", seen[0].uri)
	}
	if seen[1].method != http.MethodGet || seen[1].uri != "/assets/entities/ety_1" {
		t.Errorf("get = %s %s", seen[1].method, seen[1].uri)
	}
}

func TestACharacterWithoutImagesHasANilCover(t *testing.T) {
	api := newScriptedAPI(t, okData(`{"id":"ety_2","kind":"character","name":"Empty","note":"",
		"coverAssetId":"","cover":null,"assets":[],"count":0,
		"createdAt":"2026-10-12T10:00:00Z","updatedAt":"2026-10-12T10:00:00Z"}`))
	empty, err := api.client(t).GetCharacter(context.Background(), "ety_2")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if empty.Cover != nil || len(empty.URIs()) != 0 {
		t.Errorf("character = %+v", empty)
	}
}

func TestEachCharacterFollowsTheCursorAndRefusesARepeat(t *testing.T) {
	api := newScriptedAPI(t,
		okData(`{"items":[{"id":"ety_1"}],"nextCursor":"c2"}`),
		okData(`{"items":[{"id":"ety_2"}]}`),
	)
	var ids []string
	if err := api.client(t).EachCharacter(context.Background(), ListCharactersOptions{},
		func(character AssetCharacter) bool { ids = append(ids, character.ID); return true }); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if strings.Join(ids, ",") != "ety_1,ety_2" {
		t.Errorf("walked %v", ids)
	}
	if seen := api.requests(); seen[1].uri != "/assets/entities?cursor=c2" {
		t.Errorf("second page URI = %s", seen[1].uri)
	}

	loop := newScriptedAPI(t,
		okData(`{"items":[],"nextCursor":"same"}`),
		okData(`{"items":[],"nextCursor":"same"}`),
	)
	if err := loop.client(t).EachCharacter(context.Background(), ListCharactersOptions{},
		func(AssetCharacter) bool { return true }); err == nil || !strings.Contains(err.Error(), "already returned") {
		t.Errorf("a repeating cursor was followed: %v", err)
	}
}

// -- Error reasons -----------------------------------------------------------

func TestDataReasonBecomesTheErrorReasonUnknownValuesIncluded(t *testing.T) {
	cases := []struct {
		data string
		want string
	}{
		{`{"reason":"asset_library_full"}`, ReasonAssetLibraryFull},
		{`{"reason":"asset_reason_added_later"}`, "asset_reason_added_later"},
		{``, ""},
		{`null`, ""},
		{`{"reason":""}`, ""},
		{`{"reason":42}`, ""},
		{`{"other":"x"}`, ""},
	}
	for _, tc := range cases {
		api := newScriptedAPI(t, failed(400, 400, tc.data, nil))
		_, err := api.client(t).SaveAsset(context.Background(),
			SaveAssetInput{Source: AssetSourceURL, URL: "https://example.com/a.png"}, "")
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("data %s: want an APIError, got %v", tc.data, err)
		}
		if apiErr.Reason != tc.want {
			t.Errorf("data %s: Reason = %q, want %q", tc.data, apiErr.Reason, tc.want)
		}
		if tc.want != "" && !strings.Contains(apiErr.Error(), "reason "+tc.want) {
			t.Errorf("the error text does not name its reason: %s", apiErr.Error())
		}
	}
}

func TestAForbiddenLibraryIsReturnedWithItsReasonAndNeverRetried(t *testing.T) {
	api := newScriptedAPI(t, failed(403, 403, `{"reason":"asset_workspace_forbidden"}`, nil))
	_, err := api.client(t).ListAssets(context.Background(), ListAssetsOptions{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Reason != ReasonAssetWorkspaceForbidden || apiErr.Retryable() {
		t.Fatalf("got %v", err)
	}
	if n := len(api.requests()); n != 1 {
		t.Errorf("a 403 was retried: %d requests", n)
	}
}

// The daily limit answers 429 with Retry-After until 00:00 UTC. Sleeping through it would either
// retry early against a limit that has not moved, or end in a local timeout that buries the
// reason; the server's answer is handed back at once instead.
func TestTheDailyLimitIsReturnedAtOnceInsteadOfWaitedOut(t *testing.T) {
	api := newScriptedAPI(t, failed(429, 429, `{"reason":"asset_daily_limit"}`, map[string]string{"Retry-After": "36000"}))
	_, err := api.client(t).SaveAsset(context.Background(),
		SaveAssetInput{Source: AssetSourceURL, URL: "https://example.com/a.png"}, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want an APIError, got %v", err)
	}
	if apiErr.Reason != ReasonAssetDailyLimit || apiErr.RetryAfter != 10*time.Hour || apiErr.RequestID != "req_fail" {
		t.Errorf("error = %+v", apiErr)
	}
	if n, sleeps := len(api.requests()), api.sleepCount(); n != 1 || sleeps != 0 {
		t.Errorf("a ten-hour Retry-After was waited on: %d requests, %d sleeps", n, sleeps)
	}
}

// The counter-case: a short Retry-After is still honoured and retried.
func TestAShortRetryAfterIsStillHonoured(t *testing.T) {
	api := newScriptedAPI(t, failed(429, 429, "", map[string]string{"Retry-After": "1"}), okData(`{"items":[]}`))
	if _, err := api.client(t).ListAssets(context.Background(), ListAssetsOptions{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := len(api.requests()); n != 2 {
		t.Errorf("want one retry, got %d requests", n)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sleeps) != 1 || api.sleeps[0] != time.Second {
		t.Errorf("sleeps = %v, want the server's one second", api.sleeps)
	}
}

// A wait the caller's own deadline cannot cover ends with the server's answer, not with a context
// error that says nothing about why.
func TestAWaitLongerThanTheContextAllowsReturnsTheServersAnswer(t *testing.T) {
	api := newScriptedAPI(t, failed(503, 503, `{"reason":"asset_storage_full"}`, map[string]string{"Retry-After": "5"}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := api.client(t).SaveAsset(ctx, SaveAssetInput{Source: AssetSourceUpload, FileID: "fil_1"}, "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Reason != ReasonAssetStorageFull || apiErr.RetryAfter != 5*time.Second {
		t.Fatalf("want the server's 503 with its reason, got %v", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the server's answer was reported as a local timeout: %v", err)
	}
}

func TestAssetErrorReasonHelpers(t *testing.T) {
	if !IsAssetErrorReason(ReasonAssetEntityNameTaken) || IsAssetErrorReason("asset_reason_added_later") || IsAssetErrorReason("") {
		t.Error("IsAssetErrorReason disagrees with the listed reasons")
	}
	reasons := AssetErrorReasons()
	if len(reasons) != 8 {
		t.Errorf("AssetErrorReasons() = %v", reasons)
	}
	reasons[0] = "changed"
	if AssetErrorReasons()[0] == "changed" {
		t.Error("AssetErrorReasons hands out its own backing array")
	}
}

// -- Uploading into the library ---------------------------------------------

// uploadLibraryAPI serves the whole upload-then-save flow: an upload ticket pointing back at itself,
// the storage PUT, the commit and the save. failPut makes storage refuse the bytes.
type uploadLibraryAPI struct {
	server        *httptest.Server
	mu            sync.Mutex
	routes        []string
	contentType   string
	putLength     int64
	putChunked    bool
	putBytes      int
	saved         map[string]any
	failPut       bool
	savedRequests int
}

func newUploadLibraryAPI(t *testing.T, failPut bool) *uploadLibraryAPI {
	t.Helper()
	api := &uploadLibraryAPI{failPut: failPut}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		api.mu.Lock()
		defer api.mu.Unlock()
		api.routes = append(api.routes, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /common/upload-url":
			var ticket struct {
				ContentType string `json:"contentType"`
				Bytes       int64  `json:"bytes"`
			}
			_ = json.Unmarshal(raw, &ticket)
			api.contentType = ticket.ContentType
			fmt.Fprintf(w, `{"code":200,"msg":"success","request_id":"r","data":{"fileId":"fil_9",
				"key":"spicy://f/fil_9","uploadUrl":"%s/storage/fil_9?X-Amz-Signature=abc","method":"PUT",
				"headers":{"Content-Type":%q,"Content-Length":"%d"},"expiresAt":"2030-01-01T00:00:00Z",
				"maxBytes":2147483648}}`, api.server.URL, ticket.ContentType, ticket.Bytes)
		case "PUT /storage/fil_9":
			api.putLength = r.ContentLength
			api.putChunked = len(r.TransferEncoding) > 0
			api.putBytes = len(raw)
			if api.failPut {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "POST /files/fil_9/commit":
			_, _ = w.Write([]byte(`{"code":200,"msg":"success","request_id":"r","data":{"fileId":"fil_9",
				"status":"ready","bytes":5,"contentType":"image/png","sha256":"` + strings.Repeat("a", 64) + `",
				"uri":"spicy://f/fil_9","expiresAt":"2030-01-01T00:00:00Z"}}`))
		case "POST /assets":
			api.savedRequests++
			_ = json.Unmarshal(raw, &api.saved)
			_, _ = w.Write([]byte(`{"code":200,"msg":"success","request_id":"r","data":` + assetJSON + `}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *uploadLibraryAPI) client(t *testing.T) *Client {
	t.Helper()
	client, err := New(Options{APIKey: "sk-test", BaseURL: a.server.URL, MaxRetries: -1})
	if err != nil {
		t.Fatalf("build a client: %v", err)
	}
	return client
}

func writeTemp(t *testing.T, name, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestUploadAssetFileUploadsCommitsAndSavesUnderTheFileName(t *testing.T) {
	api := newUploadLibraryAPI(t, false)
	path := writeTemp(t, "portrait.png", "bytes")
	asset, err := api.client(t).UploadAssetFile(context.Background(), path, UploadAssetOptions{})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if asset.URI != "spicy://a/ast_1" {
		t.Errorf("asset = %+v", asset)
	}
	want := "POST /common/upload-url,PUT /storage/fil_9,POST /files/fil_9/commit,POST /assets"
	if got := strings.Join(api.routes, ","); got != want {
		t.Errorf("routes = %s, want %s", got, want)
	}
	if fmt.Sprint(api.saved) != "map[fileId:fil_9 name:portrait.png source:upload]" {
		t.Errorf("save body = %v", api.saved)
	}
}

func TestUploadAssetFileHonoursAnExplicitNameAndSavesNothingWhenTheUploadFails(t *testing.T) {
	named := newUploadLibraryAPI(t, false)
	path := writeTemp(t, "portrait.png", "bytes")
	if _, err := named.client(t).UploadAssetFile(context.Background(), path, UploadAssetOptions{Name: "Hero"}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if named.saved["name"] != "Hero" {
		t.Errorf("save body = %v", named.saved)
	}

	refused := newUploadLibraryAPI(t, true)
	if _, err := refused.client(t).UploadAssetFile(context.Background(), path, UploadAssetOptions{}); err == nil {
		t.Fatal("a refused upload reported success")
	}
	if refused.savedRequests != 0 {
		t.Error("an asset was saved although its upload failed")
	}
}

// The default name is cut by character, not by byte: a byte cut would split a multi-byte
// character and send invalid UTF-8.
func TestTheDefaultNameIsCutToEightyCharacters(t *testing.T) {
	api := newUploadLibraryAPI(t, false)
	stem := strings.Repeat("éα", 50) // 100 characters, 200 bytes
	path := writeTemp(t, stem+".png", "bytes")
	if _, err := api.client(t).UploadAssetFile(context.Background(), path, UploadAssetOptions{}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	name, _ := api.saved["name"].(string)
	if want := string([]rune(stem)[:80]); name != want {
		t.Errorf("name = %q (%d characters), want the first 80 characters", name, len([]rune(name)))
	}
}

// LoRA weights may be 2 GiB, so they are streamed with a declared length rather than read into
// memory or sent chunked, which presigned PUTs refuse.
func TestLoRAWeightsUploadAsOctetStreamWithADeclaredLength(t *testing.T) {
	api := newUploadLibraryAPI(t, false)
	contents := strings.Repeat("w", 4096)
	path := writeTemp(t, "style.SAFETENSORS", contents)
	if _, err := api.client(t).UploadFile(context.Background(), path, ""); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if api.contentType != "application/octet-stream" {
		t.Errorf("content type = %q", api.contentType)
	}
	if api.putLength != int64(len(contents)) || api.putChunked || api.putBytes != len(contents) {
		t.Errorf("PUT length %d, chunked %v, received %d bytes; want %d with a declared length",
			api.putLength, api.putChunked, api.putBytes, len(contents))
	}
}

func TestUploadFileRefusesAnEmptyFileAndADirectory(t *testing.T) {
	api := newUploadLibraryAPI(t, false)
	client := api.client(t)
	if _, err := client.UploadFile(context.Background(), writeTemp(t, "empty.png", ""), ""); err == nil ||
		!strings.Contains(err.Error(), "empty file") {
		t.Errorf("an empty file was not refused: %v", err)
	}
	if _, err := client.UploadFile(context.Background(), t.TempDir(), "image/png"); err == nil {
		t.Error("a directory was uploaded")
	}
	if len(api.routes) != 0 {
		t.Errorf("requests reached the API: %v", api.routes)
	}
}
