package spicy

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestContractHasNotDrifted pins the contract facts hard-coded in this package to the published
// contract.
//
// Why it is needed: the enums, error codes and limits in an SDK are all constants copied from the
// contract. When the contract changes and these do not, nothing raises an error - requests still go
// out, responses still parse, it is only that some new option gets rejected locally as an illegal
// value, or some new error code is flattened into "unknown error". This kind of failure emits no
// signal.
//
// Why it compares against the published contract rather than the spicy-server repository: this
// repository is public and spicy-server is not. Giving a public repository's CI a token that can
// read a private one puts that token somewhere anyone can open a pull request against. The contract
// is public anyway, so comparing public against public needs no credentials at all.
func TestContractHasNotDrifted(t *testing.T) {
	local, err := os.ReadFile("contracts/openapi.yaml")
	if err != nil {
		t.Fatalf("read the pinned contract: %v", err)
	}

	if os.Getenv("SPICY_SKIP_LIVE_CONTRACT") == "" {
		live := fetchLiveContract(t)
		if string(local) != live {
			t.Fatalf("contracts/openapi.yaml is stale against the published contract.\n" +
				"refresh it:  curl -sH 'User-Agent: spicyapi-contract-check' " +
				"https://docs.spicyapi.ai/openapi.yaml -o contracts/openapi.yaml\n" +
				"then re-check every constant this package hard-codes against that diff")
		}
	}

	text := string(local)
	// Only the handful of facts whose drift actually causes damage. A short list gets maintained.
	assertEnum(t, text, "    TaskRecord:", "state:", []string{
		StateQueued, StateRunning, StateSucceeded, StateFailed, StateCanceled, StateExpired,
	})
	assertEnum(t, text, "    UploadURLRequest:", "contentType:", acceptedUploadTypes)
	// The asset library's reasons, sorts and sources: a reason missing here is still passed through,
	// but a sort or source missing here is refused locally before the server ever sees it.
	assertEnum(t, text, "    ErrorEnvelope:", "reason:", assetErrorReasons)
	assertEnum(t, text, "      operationId: listAssets", "name: sort,", assetSorts)
	assertEnum(t, text, "    AssetCreateRequest:", "source:", assetSources)
}

// fetchLiveContract retrieves the published contract.
//
// A User-Agent is mandatory: the docs site's edge protection answers 403 to requests with no UA or
// a default one, while the api host does not - both were tested on 2026-09-20 and they behave
// differently. Without that one header this test goes red in the name of "contract drift" for a
// reason that has nothing to do with the contract.
func fetchLiveContract(t *testing.T) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://docs.spicyapi.ai/openapi.yaml", nil)
	if err != nil {
		t.Fatalf("build the contract request: %v", err)
	}
	request.Header.Set("User-Agent", "spicyapi-contract-check")

	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Skipf("the published contract is unreachable (%v); run with network access to verify drift", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the published contract answered %d", response.StatusCode)
	}
	body := make([]byte, 0, 128*1024)
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := response.Body.Read(buffer)
		body = append(body, buffer[:n]...)
		if readErr != nil {
			break
		}
	}
	return string(body)
}

var enumPattern = regexp.MustCompile(`enum:\s*\[([^\]]+)\]`)

func assertEnum(t *testing.T, contract, anchor, field string, ours ...[]string) {
	t.Helper()
	start := strings.Index(contract, anchor)
	if start < 0 {
		t.Fatalf("the contract has no %s schema", anchor)
	}
	window := contract[start:min(start+4000, len(contract))]
	at := strings.Index(window, field)
	if at < 0 {
		t.Fatalf("%s has no %s property", anchor, field)
	}
	match := enumPattern.FindStringSubmatch(window[at:min(at+600, len(window))])
	if match == nil {
		t.Fatalf("%s.%s has no enum", anchor, field)
	}

	theirs := make([]string, 0, 8)
	for _, value := range strings.Split(match[1], ",") {
		theirs = append(theirs, strings.Trim(strings.TrimSpace(value), `'"`))
	}

	var flat []string
	for _, group := range ours {
		flat = append(flat, group...)
	}
	sort.Strings(flat)
	sort.Strings(theirs)
	if strings.Join(flat, ",") != strings.Join(theirs, ",") {
		t.Errorf("%s.%s drifted:\n  package:  %v\n  contract: %v", anchor, field, flat, theirs)
	}
}
