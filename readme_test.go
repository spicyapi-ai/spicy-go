package spicy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README is this package's front page. The Go in it is not illustration; it is the first call
// most people will write, and they will copy it wholesale.
//
// Before this check nothing compiled it, and at the time it did not compile: the type
// `spicy.TaskRequest` did not exist (its real name is CreateTaskInput), `NewIdempotencyKey()`
// returns two values and was used as one, `CreateTask` was missing an argument and
// `WaitForTerminal` was missing one too. Five errors.
//
// This kind of failure emits no signal: the README renders fine, CI stays green, and only someone
// actually copying from it finds out - at the precise moment that forms their first impression of
// this SDK. Nor does anyone changing a signature get warned; the compiler cannot see Markdown.
//
// It is a *_test.go rather than a standalone program under scripts/ because this repository's CI
// runs exactly three commands - gofmt, go vet and go test (.github/workflows/ci.yml). Nothing would
// execute something placed in scripts/, which amounts to writing a check down and then switching it
// off.

// readmeSnippet is the compilation context for one Go block in the README.
type readmeSnippet struct {
	// name appears in failure messages.
	name string
	// anchor is a short piece of text unique to that block. It ties this table entry to that block
	// in the README: if someone reorders the blocks or inserts one in the middle, the check says
	// exactly which block no longer lines up instead of quietly compiling the wrong code against
	// the wrong context.
	anchor string
	// prelude supplies whatever the snippet assumes already exists. The README reads
	// continuously, each section carrying on with the previous one's variables; listing those here
	// is closer to how a reader actually copies than making every snippet build a client from
	// scratch.
	prelude string
	// sink consumes variables the snippet itself does not go on to use. In Go a declared-and-unused
	// variable is a compile error, whereas a documentation snippet is perfectly entitled to stop
	// partway.
	sink string
}

// readmeSnippets registers every Go block in the README, in the order they appear in the file. The
// count is fixed by this table: adding a block without registering it turns the check red rather
// than silently leaving that block unchecked.
var readmeSnippets = []readmeSnippet{
	{
		name:   "Generate something",
		anchor: "client.WaitForTerminal",
	},
	{
		name:   "Skipping the first poll",
		anchor: "WaitSeconds: &seconds",
		// request and key come from the previous block; this one demonstrates wait alone.
		prelude: "request := spicy.CreateTaskInput{Model: \"m\", Input: map[string]any{\"prompt\": \"p\"}}\n" +
			"key, _ := spicy.NewIdempotencyKey()\n",
	},
	{
		name:   "Start from a local file",
		anchor: "client.UploadFile",
		sink:   "_, _ = uploaded, err",
	},
	{
		name:   "Asset library",
		anchor: "client.SaveAsset",
		sink:   "_ = input",
	},
	{
		name:    "Favorites, folders and characters",
		anchor:  "client.GetCharacter",
		prelude: "hero := &spicy.LibraryAsset{}\n",
		sink:    "_, _ = page, references",
	},
	{
		name:   "Balance, usage and history",
		anchor: "client.EachTask",
	},
	{
		name:   "Errors",
		anchor: "var apiErr *spicy.APIError",
	},
}

var readmeGoFence = regexp.MustCompile("(?ms)^```go[^\n]*\n(.*?)^```$")

// snippetHarness gives a snippet a shell it can compile inside.
//
// The code in the README is part of a function body rather than a whole program, so it needs a
// context. The snippet goes inside an inner { } block so that a short declaration such as
// `client, err := spicy.New(...)` holds as written - otherwise it collides in one scope with the
// identically named outer parameter and the compiler says "no new variables on left side of :=",
// which is this harness's problem rather than the README's.
//
// Parameters may go unused, so a snippet that only demonstrates half a flow does not go red merely
// for lack of context.
const snippetHarness = `package main

import (
	"context"
	"errors"
	"log"

	spicy "github.com/spicyapi-ai/spicy-go"
)

// Imports a snippet does not use are consumed here: otherwise "imported and not used" would
// drown out the type errors this check actually exists to surface.
var (
	_ = context.Background
	_ = errors.As
	_ = log.Println
	_ = spicy.APIBaseURL
)

func snippet(ctx context.Context, client *spicy.Client, err error) error {
	{
%s
%s
%s
	}
	return nil
}

func main() { _ = snippet }
`

// TestREADMEGoBlocksCompile compiles every Go block in the README.
func TestREADMEGoBlocksCompile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no Go toolchain on PATH; this check needs one to compile the README")
	}

	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate the repository: %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	matches := readmeGoFence.FindAllStringSubmatch(string(readme), -1)
	if len(matches) != len(readmeSnippets) {
		t.Fatalf("README.md has %d Go code block(s) but readmeSnippets declares %d.\n"+
			"every Go block must be registered there, or it goes uncompiled and nothing says so",
			len(matches), len(readmeSnippets))
	}

	for i, match := range matches {
		declared := readmeSnippets[i]
		block := match[1]
		if !strings.Contains(block, declared.anchor) {
			t.Errorf("Go block #%d does not contain the anchor %q declared for %q; "+
				"the blocks and readmeSnippets are out of step",
				i+1, declared.anchor, declared.name)
			continue
		}
		t.Run(declared.name, func(t *testing.T) {
			compileSnippet(t, root, declared.prelude, block, declared.sink)
		})
	}
}

// compileSnippet compiles one snippet inside a throwaway module that replaces back to this
// repository.
func compileSnippet(t *testing.T, root, prelude, block, sink string) {
	t.Helper()

	dir := t.TempDir()

	write := func(name, contents string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("go.mod", "module spicyreadmecheck\n\ngo 1.23\n\n"+
		"require github.com/spicyapi-ai/spicy-go v0.0.0\n\n"+
		"replace github.com/spicyapi-ai/spicy-go => "+root+"\n")
	write("main.go", sprintfHarness(indent(prelude), indent(block), indent(sink)))

	build := exec.Command("go", "build", "./...")
	build.Dir = dir
	// GOFLAGS=-mod=mod lets it fill in the require itself; GOPROXY=off and GOWORK=off keep this
	// check off the network and out of reach of any enclosing go.work that would rewrite module
	// resolution - otherwise it fails on some machines for reasons unrelated to the README, and a
	// check that cries wolf is no check at all.
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOPROXY=off", "GOWORK=off")

	if output, err := build.CombinedOutput(); err != nil {
		t.Errorf("this README code block does not compile:\n%s\n"+
			"fix the README against the real signatures in client.go — not the other way round",
			strings.TrimSpace(string(output)))
	}
}

func sprintfHarness(prelude, body, sink string) string {
	return strings.Replace(snippetHarness, "%s\n%s\n%s", prelude+"\n"+body+"\n"+sink, 1)
}

// indent shifts a snippet to the harness's nesting level while keeping blank lines genuinely
// blank.
func indent(block string) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ""
			continue
		}
		lines[i] = "\t\t" + line
	}
	return strings.Join(lines, "\n")
}
