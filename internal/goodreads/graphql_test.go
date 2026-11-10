package goodreads

import (
	"testing"

	"goodreads-nextread/internal/goodreads/goodreadstest"
)

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
