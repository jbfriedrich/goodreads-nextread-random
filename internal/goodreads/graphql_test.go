package goodreads

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"goodreads-nextread/internal/goodreads/goodreadstest"
)

// memKeys is an in-memory KeyStore.
type memKeys map[string]string

func (m memKeys) Setting(k string) (string, error) { return m[k], nil }
func (m memKeys) SetSetting(k, v string) error     { m[k] = v; return nil }

// captureLog redirects the standard logger for the rest of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

func TestLookupLogsPerBookErrorBeforeFallingBack(t *testing.T) {
	f := goodreadstest.New(t, 0)
	f.Set(func(f *goodreadstest.Fake) {
		f.Books["1"] = goodreadstest.BookData{Desc: "one"}
		f.Books["2"] = goodreadstest.BookData{Desc: "two"}
		f.GQLBookErrors = map[string]string{"2": "InternalFailure"}
	})
	logs := captureLog(t)
	g := NewGraphQL(f.Server.Client(), f.Server.URL+"/graphql", newClient(f), memKeys{SettingGraphQLKey: goodreadstest.APIKey})

	got, err := g.Lookup(context.Background(), []string{"1", "2"})
	if err != nil {
		t.Fatal(err)
	}
	if got["1"] == nil || got["1"].Description != "one" {
		t.Errorf("book 1 = %+v, want its details", got["1"])
	}
	if _, ok := got["2"]; ok {
		t.Errorf("book 2 is in the result; it should be left to the book-page fallback")
	}
	if line := logs.String(); !strings.Contains(line, "book 2") || !strings.Contains(line, "InternalFailure") {
		t.Errorf("log %q should name book 2 and GraphQL's error type", line)
	}
}

func TestAPIKeyFromBookPage(t *testing.T) {
	key, err := apiKeyFromBookPage(readFixture(t, "book_page.html"))
	if err != nil || key != "da2-fixturekey" {
		t.Errorf("apiKeyFromBookPage = %q, %v; want da2-fixturekey", key, err)
	}
	if _, err := apiKeyFromBookPage([]byte(goodreadstest.BookPageHTML("1", goodreadstest.BookData{}, ""))); err == nil {
		t.Error("expected an error for a page without an apiKey")
	}
}

func TestBookQueryStaysUnderWAFBodyLimit(t *testing.T) {
	ids := make([]string, BatchSize)
	for i := range ids {
		ids[i] = "999999999999" // longest realistic legacy ids
	}
	body, err := bookQueryBody(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxQueryBody {
		t.Errorf("a %d-book query body is %d bytes, over the %d-byte limit", BatchSize, len(body), MaxQueryBody)
	}
	if _, err := bookQueryBody([]string{"12a"}); err == nil {
		t.Error("expected an error for a non-numeric book id")
	}
}
