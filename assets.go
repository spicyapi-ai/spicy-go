package spicy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The asset library keeps files until they are deleted, each with a stable
// spicy://a/<id> reference that works in any media field of model inputs sent
// with keys of the same workspace. An upload's spicy://f/ URI lasts one day; a
// saved asset does not expire.
//
// The key decides the library. A key created in a personal workspace reaches
// the personal library; a key created in a team workspace, by any member,
// reaches that team's shared library and never anyone's personal one. With a
// team key, what may change follows the current role of the member who created
// it (see AssetUsage.Scope).
//
// Nothing here returns a public or permanent link: PreviewURL is a private
// preview signed for at most 20 minutes, so store URI, never PreviewURL.

// AssetURIPrefix starts a saved asset's stable reference, as in
// spicy://a/ast_.... Every method that takes an asset ID accepts either the
// bare ID or this reference.
const AssetURIPrefix = "spicy://a/"

// Sources a saved asset can come from: the output of one of your successful
// tasks, a committed upload, or a public URL the server downloads once.
const (
	AssetSourceTaskOutput = "task_output"
	AssetSourceUpload     = "upload"
	AssetSourceURL        = "url"
)

// Orders of an asset listing. AssetSortRecent, newest first, is the server's
// default. A cursor only works with the sort it was issued for.
const (
	AssetSortRecent = "recent"
	AssetSortOldest = "oldest"
	AssetSortName   = "name"
	AssetSortSize   = "size"
)

// Reasons the asset library attaches to a failed response as data.reason,
// surfaced as APIError.Reason:
//
//	asset_library_locked       400  the library opens after the owner's first top-up.
//	asset_library_full         400  the allowance's storage or item count is used up.
//	asset_file_too_large       400  the file is above the plan's per-file limit.
//	asset_daily_limit          429  today's (UTC) allowance of new assets is used up;
//	                                RetryAfter runs until 00:00 UTC.
//	asset_storage_full         503  storage is temporarily full across the platform.
//	asset_permission_denied    403  the key's team role does not allow this change.
//	asset_workspace_forbidden  403  this key or workspace cannot reach that library,
//	                                for example because its creator left the team.
//	asset_entity_name_taken    409  the library already has a character with that name.
//
// The server may add reasons later. APIError.Reason carries any value
// unchanged, so keep a default branch and treat an unknown value like an empty
// one.
const (
	ReasonAssetLibraryLocked      = "asset_library_locked"
	ReasonAssetLibraryFull        = "asset_library_full"
	ReasonAssetFileTooLarge       = "asset_file_too_large"
	ReasonAssetDailyLimit         = "asset_daily_limit"
	ReasonAssetStorageFull        = "asset_storage_full"
	ReasonAssetPermissionDenied   = "asset_permission_denied"
	ReasonAssetWorkspaceForbidden = "asset_workspace_forbidden"
	ReasonAssetEntityNameTaken    = "asset_entity_name_taken"
)

// assetErrorReasons is the set above. TestContractHasNotDrifted pins it to the
// published contract.
var assetErrorReasons = []string{
	ReasonAssetLibraryLocked,
	ReasonAssetLibraryFull,
	ReasonAssetFileTooLarge,
	ReasonAssetDailyLimit,
	ReasonAssetStorageFull,
	ReasonAssetPermissionDenied,
	ReasonAssetWorkspaceForbidden,
	ReasonAssetEntityNameTaken,
}

// assetSorts and assetSources are pinned to the contract the same way.
var (
	assetSorts   = []string{AssetSortRecent, AssetSortOldest, AssetSortName, AssetSortSize}
	assetSources = []string{AssetSourceTaskOutput, AssetSourceUpload, AssetSourceURL}
)

// AssetErrorReasons returns the asset library reasons the contract lists
// today, as a fresh slice the caller may keep.
func AssetErrorReasons() []string {
	return append([]string(nil), assetErrorReasons...)
}

// IsAssetErrorReason reports whether reason is one of the asset library
// reasons the contract lists today.
func IsAssetErrorReason(reason string) bool {
	return contains(assetErrorReasons, reason)
}

// assetNameMaxRunes only cuts the default name of an uploaded file; a name
// passed explicitly is the server's to judge.
const assetNameMaxRunes = 80

// LibraryAsset is one saved file of the asset library. It is a different
// record from Asset, which is one output of a task.
type LibraryAsset struct {
	ID string `json:"id"`
	// URI is the stable spicy://a/ast_... reference. It works only in
	// generation requests made with keys of the same workspace, in any media
	// field of a model input; it is never a downloadable link. Store this.
	URI  string `json:"uri"`
	Name string `json:"name"`
	// Kind is image, video or audio.
	Kind        string `json:"kind"`
	ContentType string `json:"contentType"`
	Bytes       int64  `json:"bytes"`
	// Width and Height are measured by the server, and 0 when they do not
	// apply.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
	// DurationSeconds is measured by the server for audio and video, and 0
	// otherwise.
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	// Source is one of the AssetSource* values.
	Source string `json:"source"`
	// SourceID is your task or upload ID; for a URL, an opaque digest of the
	// address, which itself is not stored.
	SourceID  string    `json:"sourceId"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// PreviewURL is a private preview signed for at most 20 minutes, and empty
	// when signing failed. Show it; never store or share it.
	PreviewURL          string     `json:"previewUrl"`
	PreviewURLExpiresAt *time.Time `json:"previewUrlExpiresAt,omitempty"`
	// Favorite belongs to the library, so everyone in a team sees the same
	// favorites.
	Favorite bool `json:"favorite"`
	// Model is the public name of the model that generated the asset, and
	// ModelName its display name. Both are empty for uploads and links.
	Model     string `json:"model"`
	ModelName string `json:"modelName"`
}

// AssetList is one page of the asset library.
type AssetList struct {
	Items []LibraryAsset `json:"items"`
	// NextCursor is absent on the last page. It is opaque, and only works with
	// the filters and sort it was issued for.
	NextCursor string `json:"nextCursor,omitempty"`
}

// AssetCharacter is a named, ordered set of images in the asset library.
// Characters are created and arranged in the console; the API only reads them.
//
// A character references assets instead of copying them, so each image carries
// its own spicy://a/ URI. To use a character in a generation request, put the
// URIs of its images, in order, into the model's reference image field.
type AssetCharacter struct {
	ID string `json:"id"`
	// Kind is "character", the only kind today.
	Kind string `json:"kind"`
	// Name is unique within the library, ignoring case.
	Name string `json:"name"`
	Note string `json:"note"`
	// CoverAssetID and Cover are empty and nil while the character has no
	// images.
	CoverAssetID string        `json:"coverAssetId"`
	Cover        *LibraryAsset `json:"cover"`
	// Assets are the character's images in order.
	Assets    []LibraryAsset `json:"assets"`
	Count     int            `json:"count"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

// URIs returns the spicy://a/ references of the character's images in order,
// ready for a model's reference image field.
func (c *AssetCharacter) URIs() []string {
	uris := make([]string, 0, len(c.Assets))
	for _, asset := range c.Assets {
		uris = append(uris, asset.URI)
	}
	return uris
}

// AssetCharacterList is one page of characters, newest first.
type AssetCharacterList struct {
	Items []AssetCharacter `json:"items"`
	// NextCursor is absent on the last page.
	NextCursor string `json:"nextCursor,omitempty"`
}

// DeletedAsset confirms a deletion.
type DeletedAsset struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// AssetPlan is one storage plan. Plans unlock with the owner's lifetime
// top-ups and never go down.
type AssetPlan struct {
	// ID is the lowercase plan key, for example free or pro.
	ID   string `json:"id"`
	Name string `json:"name"`
	// MinTopupUSD is the lifetime top-up, a decimal USD string, that unlocks
	// this plan.
	MinTopupUSD string `json:"minTopupUsd"`
	// MaxBytes is the total storage in bytes (binary units); 0 means saving
	// is closed on this plan.
	MaxBytes     int64 `json:"maxBytes"`
	MaxCount     int   `json:"maxCount"`
	MaxFileBytes int64 `json:"maxFileBytes"`
}

// AssetScope says which library the key reaches and what it may do there.
type AssetScope struct {
	// WorkspaceID is the personal workspace or the team, and empty for an
	// account with no personal workspace yet.
	WorkspaceID string `json:"workspaceId"`
	Personal    bool   `json:"personal"`
	// Role is always owner for a personal library. For a team key it is the
	// current role of the member who created the key: owner, admin,
	// developer, billing or readonly.
	Role string `json:"role"`
	// CanWrite says whether new assets can be saved here.
	CanWrite bool `json:"canWrite"`
	// CanTopUp says whether the caller can see and raise the owner's
	// allowance. When it is false, AssetUsage.LifetimeTopupUSD and
	// TopupToNextTierUSD are empty.
	CanTopUp bool `json:"canTopUp"`
}

// AssetUsageTotals is what the allowance holds.
type AssetUsageTotals struct {
	Count int   `json:"count"`
	Bytes int64 `json:"bytes"`
}

// AssetLimits are the limits of the current plan.
type AssetLimits struct {
	Tier         string `json:"tier"`
	MaxCount     int    `json:"maxCount"`
	MaxBytes     int64  `json:"maxBytes"`
	MaxFileBytes int64  `json:"maxFileBytes"`
	// GlobalMaxBytes is the platform-wide ceiling; 0 disables it.
	GlobalMaxBytes int64 `json:"globalMaxBytes"`
	NameMaxChars   int   `json:"nameMaxChars"`
}

// AssetUsage is the storage plan, what is stored, the limits and how many
// assets were saved today. Usage and limits cover the owner's whole
// allowance: the personal library plus every team library they own.
type AssetUsage struct {
	Tier   AssetPlan        `json:"tier"`
	Usage  AssetUsageTotals `json:"usage"`
	Limits AssetLimits      `json:"limits"`
	// Full is true when no new asset fits, including plans where saving is
	// closed.
	Full bool `json:"full"`
	// NextTier is the next plan with more storage or items, nil on the
	// highest plan.
	NextTier *AssetPlan `json:"nextTier"`
	// TopupToNextTierUSD and LifetimeTopupUSD are decimal USD strings, empty
	// when Scope.CanTopUp is false.
	TopupToNextTierUSD string      `json:"topupToNextTierUsd,omitempty"`
	LifetimeTopupUSD   string      `json:"lifetimeTopupUsd,omitempty"`
	Tiers              []AssetPlan `json:"tiers"`
	// DailyCreateLimit is the number of new assets allowed per allowance per
	// UTC day, and CreatedToday how many were saved so far.
	DailyCreateLimit int        `json:"dailyCreateLimit"`
	CreatedToday     int        `json:"createdToday"`
	Scope            AssetScope `json:"scope"`
}

// SaveAssetInput names exactly one source to save. Fields that belong to
// another source are refused before anything is sent.
type SaveAssetInput struct {
	// Source is one of the AssetSource* values.
	Source string `json:"source"`

	// TaskID, with Key or Index, selects an output of one of your own
	// successful tasks (with a team key, one of the team's). Index is the
	// zero-based position in Task.Output.Assets; it is a pointer because 0 is
	// a real position.
	TaskID string `json:"taskId,omitempty"`
	Key    string `json:"key,omitempty"`
	Index  *int   `json:"index,omitempty"`

	// FileID is a committed upload, as fil_... or spicy://f/fil_....
	FileID string `json:"fileId,omitempty"`

	// URL is a public http or https address on port 80 or 443 without
	// credentials. The server downloads it once and reads the type from the
	// file itself.
	URL string `json:"url,omitempty"`

	// Name is optional; the server defaults to "Saved asset".
	Name string `json:"name,omitempty"`
}

// SaveAsset copies a file into the library and returns the saved asset.
// Saving the same source again returns the asset you already have and uses no
// extra space.
//
// The server requires an Idempotency-Key on every save. Leave idempotencyKey
// empty and a fresh one is minted for this call, which also makes the
// automatic resend on a network fault safe: every attempt of the call carries
// the same key. Pass your own (see NewIdempotencyKey) to replay the same save
// safely after an uncertain response, for example across a process restart;
// the server binds the key to the exact request for 24 hours.
func (c *Client) SaveAsset(ctx context.Context, input SaveAssetInput, idempotencyKey string) (*LibraryAsset, error) {
	body, err := saveAssetBody(input)
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		if key, err = NewIdempotencyKey(); err != nil {
			return nil, err
		}
	} else if strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("spicy: idempotencyKey must not contain line breaks")
	}
	out := &LibraryAsset{}
	if err := c.do(ctx, request{
		method:     http.MethodPost,
		path:       "/assets",
		body:       body,
		headers:    map[string]string{"Idempotency-Key": key},
		replayable: true,
		out:        out,
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// saveAssetBody checks that input names exactly one source and returns what
// to send.
func saveAssetBody(input SaveAssetInput) (SaveAssetInput, error) {
	given := map[string]bool{
		"TaskID": input.TaskID != "",
		"Key":    input.Key != "",
		"Index":  input.Index != nil,
		"FileID": input.FileID != "",
		"URL":    input.URL != "",
	}
	var allowed []string
	switch input.Source {
	case AssetSourceTaskOutput:
		allowed = []string{"TaskID", "Key", "Index"}
	case AssetSourceUpload:
		allowed = []string{"FileID"}
	case AssetSourceURL:
		allowed = []string{"URL"}
	default:
		return input, fmt.Errorf("spicy: Source must be one of %s, got %q", strings.Join(assetSources, ", "), input.Source)
	}
	// A save has exactly one source, so a field that belongs to another one is a mistake: the
	// server would refuse it, and quietly dropping it here would save something the caller did
	// not ask for.
	var foreign []string
	for _, field := range []string{"TaskID", "Key", "Index", "FileID", "URL"} {
		if given[field] && !contains(allowed, field) {
			foreign = append(foreign, field)
		}
	}
	if len(foreign) > 0 {
		return input, fmt.Errorf(
			"spicy: %s does not apply to Source %q; send exactly one source per save",
			strings.Join(foreign, ", "), input.Source,
		)
	}

	out := SaveAssetInput{Source: input.Source, Name: input.Name}
	var err error
	switch input.Source {
	case AssetSourceTaskOutput:
		if out.TaskID, err = requireID(input.TaskID, "TaskID"); err != nil {
			return input, err
		}
		if input.Key != "" && strings.TrimSpace(input.Key) == "" {
			return input, errors.New("spicy: Key must not be blank")
		}
		if input.Index != nil && *input.Index < 0 {
			return input, errors.New("spicy: Index must not be negative")
		}
		out.Key, out.Index = input.Key, input.Index
	case AssetSourceUpload:
		if out.FileID, err = requireID(input.FileID, "FileID"); err != nil {
			return input, err
		}
	case AssetSourceURL:
		if strings.TrimSpace(input.URL) == "" {
			return input, errors.New("spicy: URL is required for Source \"url\"")
		}
		out.URL = input.URL
	}
	return out, nil
}

// ListAssetsOptions filters and orders an asset listing. A zero field means
// "do not filter".
type ListAssetsOptions struct {
	// Kind is image, video or audio.
	Kind string
	// Source keeps the assets from any of these sources (sent
	// comma-separated); see the AssetSource* values.
	Source []string
	// Q is a case-insensitive substring of the asset name.
	Q string
	// Favorite, when non-nil, lists only favorites (true) or only the assets
	// that are not favorites (false). It is a pointer because false is a
	// filter of its own, not "no filter".
	Favorite *bool
	// CollectionID keeps the assets of one folder of the same library.
	// Folders are made in the console; an ID outside the key's library is
	// answered with 404.
	CollectionID string
	// EntityID keeps the images of one character; an unknown ID gives an
	// empty page.
	EntityID string
	// Sort is one of the AssetSort* values; the server defaults to
	// AssetSortRecent. An unknown sort is refused before anything is sent.
	Sort string
	// Cursor is the previous page's NextCursor, unchanged and with the same
	// filters and Sort. A cursor sent with a different Sort fails with 400,
	// which is returned as-is rather than retried.
	Cursor string
	// Limit is the page size; the server defaults to 100. Its upper bound is
	// the server's to enforce, as with ListTasksOptions.Limit.
	Limit int
}

func (opts ListAssetsOptions) query() (url.Values, error) {
	if opts.Limit < 0 {
		return nil, errors.New("spicy: Limit must not be negative")
	}
	if opts.Sort != "" && !contains(assetSorts, opts.Sort) {
		return nil, fmt.Errorf("spicy: Sort must be one of %s, got %q", strings.Join(assetSorts, ", "), opts.Sort)
	}
	query := url.Values{}
	sources := make([]string, 0, len(opts.Source))
	for _, source := range opts.Source {
		// An empty entry is a filter that was computed and came out empty. Dropping it would widen
		// the listing without a word, so it is refused instead.
		if strings.TrimSpace(source) == "" {
			return nil, errors.New("spicy: Source must not contain an empty value")
		}
		sources = append(sources, source)
	}
	if len(sources) > 0 {
		query.Set("source", strings.Join(sources, ","))
	}
	for name, value := range map[string]string{"kind": opts.Kind, "q": opts.Q, "sort": opts.Sort, "cursor": opts.Cursor} {
		if value != "" {
			query.Set(name, value)
		}
	}
	for name, value := range map[string]string{"collectionId": opts.CollectionID, "entityId": opts.EntityID} {
		id, err := optionalID(value, name)
		if err != nil {
			return nil, err
		}
		if id != "" {
			query.Set(name, id)
		}
	}
	if opts.Favorite != nil {
		query.Set("favorite", strconv.FormatBool(*opts.Favorite))
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	return query, nil
}

// ListAssets returns one page of the ready assets in the key's library. Use
// EachAsset to walk every page.
func (c *Client) ListAssets(ctx context.Context, opts ListAssetsOptions) (*AssetList, error) {
	query, err := opts.query()
	if err != nil {
		return nil, err
	}
	out := &AssetList{}
	if err := c.do(ctx, request{method: http.MethodGet, path: withQuery("/assets", query), replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// EachAsset walks every page of ListAssets and calls fn once per asset,
// starting from opts.Cursor when it is set. Return false from fn to stop
// early; the error is the first page that failed.
//
// The filters and Sort go out unchanged on every page and only the cursor
// moves, because a cursor only works with the query it was issued for. A
// cursor that comes back twice is refused instead of looping. Limit sets the
// page size, not the total.
func (c *Client) EachAsset(ctx context.Context, opts ListAssetsOptions, fn func(LibraryAsset) bool) error {
	if fn == nil {
		return errors.New("spicy: fn is required")
	}
	// Check every argument before the first request, so a bad value fails where it was written.
	if _, err := opts.query(); err != nil {
		return err
	}
	return eachPage(opts.Cursor, "asset listing", func(cursor string) (string, bool, error) {
		opts.Cursor = cursor
		page, err := c.ListAssets(ctx, opts)
		if err != nil {
			return "", false, err
		}
		for _, item := range page.Items {
			if !fn(item) {
				return "", false, nil
			}
		}
		return page.NextCursor, true, nil
	})
}

// GetAsset reads one asset with a fresh private PreviewURL. assetID may be the
// bare ast_... ID or its spicy://a/ reference. An unknown asset, another
// account's asset and one in a library this key does not reach are all the
// same 404.
func (c *Client) GetAsset(ctx context.Context, assetID string) (*LibraryAsset, error) {
	path, err := assetPath(assetID)
	if err != nil {
		return nil, err
	}
	out := &LibraryAsset{}
	if err := c.do(ctx, request{method: http.MethodGet, path: path, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateAssetInput changes an asset's name, its favorite mark or both. Only
// what is set is sent.
type UpdateAssetInput struct {
	// Name renames the asset when it is not empty.
	Name string `json:"name,omitempty"`
	// Favorite marks (true) or unmarks (false) the asset when it is non-nil.
	Favorite *bool `json:"favorite,omitempty"`
}

// UpdateAsset renames an asset, marks or unmarks it as a favorite, or both,
// and returns the updated asset. The URI stays the same. Repeating the request
// is safe, so no Idempotency-Key is sent.
func (c *Client) UpdateAsset(ctx context.Context, assetID string, input UpdateAssetInput) (*LibraryAsset, error) {
	path, err := assetPath(assetID)
	if err != nil {
		return nil, err
	}
	if input.Name == "" && input.Favorite == nil {
		// The server answers an empty body with a misleading "invalid name".
		return nil, errors.New("spicy: UpdateAsset needs a Name, a Favorite or both")
	}
	out := &LibraryAsset{}
	if err := c.do(ctx, request{method: http.MethodPatch, path: path, body: input, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteAsset deletes an asset. It disappears at once and its URI stops
// working in new requests. Deleting an unknown or already deleted asset
// returns the same result, so repeating it is safe and no Idempotency-Key is
// sent.
func (c *Client) DeleteAsset(ctx context.Context, assetID string) (*DeletedAsset, error) {
	path, err := assetPath(assetID)
	if err != nil {
		return nil, err
	}
	out := &DeletedAsset{}
	if err := c.do(ctx, request{method: http.MethodDelete, path: path, replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// GetAssetUsage reads the storage plan, what is stored, the limits, how many
// assets were saved today and the Scope the key acts with.
func (c *Client) GetAssetUsage(ctx context.Context) (*AssetUsage, error) {
	out := &AssetUsage{}
	if err := c.do(ctx, request{method: http.MethodGet, path: "/assets/usage", replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// UploadAssetOptions tunes UploadAssetFile. Every zero field takes its
// default.
type UploadAssetOptions struct {
	// Name defaults to the file's base name, cut to 80 characters.
	Name string
	// ContentType is inferred from the extension when empty, as in UploadFile.
	ContentType string
}

// UploadAssetFile uploads a local file with UploadFile and saves it to the
// library in one call, returning the saved asset. The upload's type and size
// limits apply. Nothing is saved when the upload fails. Every call uploads
// again, so there is no idempotency key to pass: the save uses a fresh one for
// its own retries.
func (c *Client) UploadAssetFile(ctx context.Context, path string, opts UploadAssetOptions) (*LibraryAsset, error) {
	uploaded, err := c.UploadFile(ctx, path, opts.ContentType)
	if err != nil {
		return nil, err
	}
	name := opts.Name
	if name == "" {
		name = strings.TrimSpace(filepath.Base(path))
		if runes := []rune(name); len(runes) > assetNameMaxRunes {
			// Cut by character, not by byte, or a multi-byte name would be split mid-character.
			name = string(runes[:assetNameMaxRunes])
		}
	}
	return c.SaveAsset(ctx, SaveAssetInput{Source: AssetSourceUpload, FileID: uploaded.FileID, Name: name}, "")
}

// ListCharactersOptions filters a character listing. A zero field means "do
// not filter".
type ListCharactersOptions struct {
	// AssetID keeps only the characters that contain this asset, as ast_...
	// or spicy://a/ast_....
	AssetID string
	// Cursor is the previous page's NextCursor, unchanged.
	Cursor string
	// Limit is the page size; the server defaults to 50.
	Limit int
}

func (opts ListCharactersOptions) query() (url.Values, error) {
	if opts.Limit < 0 {
		return nil, errors.New("spicy: Limit must not be negative")
	}
	query := url.Values{}
	if opts.AssetID != "" {
		id, err := bareAssetID(opts.AssetID, "AssetID")
		if err != nil {
			return nil, err
		}
		query.Set("assetId", id)
	}
	if opts.Cursor != "" {
		query.Set("cursor", opts.Cursor)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	return query, nil
}

// ListCharacters returns one page of the library's characters, newest first,
// each with its images in order. Characters are read-only through the API.
func (c *Client) ListCharacters(ctx context.Context, opts ListCharactersOptions) (*AssetCharacterList, error) {
	query, err := opts.query()
	if err != nil {
		return nil, err
	}
	out := &AssetCharacterList{}
	if err := c.do(ctx, request{method: http.MethodGet, path: withQuery("/assets/entities", query), replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// EachCharacter walks every page of ListCharacters and calls fn once per
// character. Return false from fn to stop early. A cursor that comes back
// twice is refused instead of looping.
func (c *Client) EachCharacter(ctx context.Context, opts ListCharactersOptions, fn func(AssetCharacter) bool) error {
	if fn == nil {
		return errors.New("spicy: fn is required")
	}
	if _, err := opts.query(); err != nil {
		return err
	}
	return eachPage(opts.Cursor, "character listing", func(cursor string) (string, bool, error) {
		opts.Cursor = cursor
		page, err := c.ListCharacters(ctx, opts)
		if err != nil {
			return "", false, err
		}
		for _, item := range page.Items {
			if !fn(item) {
				return "", false, nil
			}
		}
		return page.NextCursor, true, nil
	})
}

// GetCharacter reads one character with its images in order, each with a
// fresh private PreviewURL. An unknown character and one outside the key's
// library are the same 404.
func (c *Client) GetCharacter(ctx context.Context, characterID string) (*AssetCharacter, error) {
	id, err := requireID(characterID, "characterID")
	if err != nil {
		return nil, err
	}
	out := &AssetCharacter{}
	if err := c.do(ctx, request{method: http.MethodGet, path: "/assets/entities/" + url.PathEscape(id), replayable: true, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// eachPage follows a cursor until the last page. fetch reads the page at
// cursor and returns the next cursor and whether to go on.
func eachPage(cursor, what string, fetch func(cursor string) (string, bool, error)) error {
	seen := map[string]bool{}
	for {
		next, more, err := fetch(cursor)
		if err != nil || !more || next == "" {
			return err
		}
		// A cursor that comes back twice would loop forever and spend the read limit on nothing.
		if seen[next] {
			return fmt.Errorf("spicy: the %s returned a cursor it had already returned", what)
		}
		seen[next] = true
		cursor = next
	}
}

// requireID trims an identifier and refuses values that cannot be sent as
// they are: an empty ID would address the collection instead of one item, and
// a line break has no business in a URL.
func requireID(value, label string) (string, error) {
	id := strings.TrimSpace(value)
	if id == "" {
		return "", fmt.Errorf("spicy: %s is required", label)
	}
	if strings.ContainsAny(id, "\r\n") {
		return "", fmt.Errorf("spicy: %s must not contain line breaks", label)
	}
	return id, nil
}

// optionalID is requireID for a filter that may be left empty.
func optionalID(value, label string) (string, error) {
	if value == "" {
		return "", nil
	}
	return requireID(value, label)
}

// bareAssetID accepts the bare ID (ast_...) or the reference used in model
// inputs (spicy://a/ast_...), so a value copied from either place works.
func bareAssetID(value, label string) (string, error) {
	id, err := requireID(value, label)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(id, AssetURIPrefix) {
		return requireID(strings.TrimPrefix(id, AssetURIPrefix), label)
	}
	return id, nil
}

func assetPath(assetID string) (string, error) {
	id, err := bareAssetID(assetID, "assetID")
	if err != nil {
		return "", err
	}
	return "/assets/" + url.PathEscape(id), nil
}

func withQuery(path string, query url.Values) string {
	if len(query) == 0 {
		return path
	}
	return path + "?" + query.Encode()
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
