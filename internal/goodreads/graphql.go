package goodreads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Goodreads' book pages load their data from an AppSync GraphQL API using an
// API key that every page ships in __NEXT_DATA__ (props.pageProps.apiKey).
// Asking it directly for just the fields we need costs ~800 bytes per book in
// batches of 25, against 641 KB (uncompressed) per book page. It is
// undocumented, so anything other than a transient failure makes the enricher
// fall back to the book pages.

const (
	DefaultGraphQLEndpoint = "https://kxbwmqov6jgg3daaamb744ycu4.appsync-api.us-east-1.amazonaws.com/graphql"
	// BatchSize books are looked up per request.
	BatchSize = 25
	// MaxQueryBody caps the request body. AppSync's WAF rejected an 11.7 KB
	// query body (consistent with AWS WAF's 8 KB body rule); 6 KB leaves room.
	MaxQueryBody = 6 << 10
	// SettingGraphQLKey caches the API key in the store's settings table.
	SettingGraphQLKey = "graphql_api_key"
)

// ErrGraphQLUnavailable marks a GraphQL failure that waiting will not fix
// (schema change, key still rejected after a refresh, unexpected response).
var ErrGraphQLUnavailable = errors.New("graphql unavailable")

// gqlBookFragment lists the fields we read; a fragment keeps each aliased
// lookup in the query to ~45 bytes.
const gqlBookFragment = `fragment F on Book{description imageUrl details{numPages publicationTime isbn isbn13} work{stats{averageRating} details{publicationTime}}}`

// bookQueryBody builds the JSON request body looking up ids as aliases b0, b1, ….
func bookQueryBody(ids []string) ([]byte, error) {
	var sb strings.Builder
	sb.WriteString("query{")
	for i, id := range ids {
		if _, err := strconv.ParseInt(id, 10, 64); err != nil {
			return nil, fmt.Errorf("book id %q is not numeric", id)
		}
		fmt.Fprintf(&sb, "b%d:getBookByLegacyId(legacyId:%s){...F}", i, id)
	}
	sb.WriteString("}")
	sb.WriteString(gqlBookFragment)
	return json.Marshal(map[string]string{"query": sb.String()})
}

type gqlBook struct {
	Description string `json:"description"`
	ImageURL    string `json:"imageUrl"`
	Details     struct {
		NumPages        int     `json:"numPages"`
		PublicationTime float64 `json:"publicationTime"`
		ISBN            string  `json:"isbn"`
		ISBN13          string  `json:"isbn13"`
	} `json:"details"`
	Work *struct {
		Stats struct {
			AverageRating float64 `json:"averageRating"`
		} `json:"stats"`
		Details struct {
			PublicationTime float64 `json:"publicationTime"`
		} `json:"details"`
	} `json:"work"`
}

func (b *gqlBook) fields() bookFields {
	f := bookFields{
		Description: b.Description,
		ImageURL:    b.ImageURL,
		NumPages:    b.Details.NumPages,
		EditionTime: b.Details.PublicationTime,
		ISBN13:      b.Details.ISBN13,
		ISBN:        b.Details.ISBN,
	}
	if b.Work != nil {
		f.AverageRating = b.Work.Stats.AverageRating
		f.WorkTime = b.Work.Details.PublicationTime
	}
	return f
}

type gqlError struct {
	ErrorType string `json:"errorType"`
	Message   string `json:"message"`
	Path      []any  `json:"path"`
}

type gqlResponse struct {
	Data   map[string]*gqlBook `json:"data"`
	Errors []gqlError          `json:"errors"`
}

// GraphQL queries the GraphQL API. It shares the enrichment Client for
// pacing, retry waits and fetching a book page when the key must be (re)read.
type GraphQL struct {
	http     *http.Client
	endpoint string
	gr       *Client
	keys     KeyStore
	key      string
}

// KeyStore persists the API key between runs (the store's settings table).
type KeyStore interface {
	Setting(key string) (string, error)
	SetSetting(key, value string) error
}

// NewGraphQL builds a GraphQL client. gr is the enrichment Client: GraphQL
// requests share its pacing and retry waits, and it fetches the book page the
// API key is read from. httpClient nil means one with a 30 s timeout.
func NewGraphQL(httpClient *http.Client, endpoint string, gr *Client, keys KeyStore) *GraphQL {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &GraphQL{http: httpClient, endpoint: endpoint, gr: gr, keys: keys}
}

// lookup fetches details for ids. In the result, an id mapped to nil does not
// exist (permanent); an id missing from the result failed on its own and
// should be tried through its book page. A transient error is returned as is
// (retries are already used up); anything else wraps ErrGraphQLUnavailable.
func (g *GraphQL) Lookup(ctx context.Context, ids []string) (map[string]*Book, error) {
	body, err := bookQueryBody(ids)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGraphQLUnavailable, err)
	}
	if len(body) > MaxQueryBody && len(ids) > 1 {
		return g.lookupSplit(ctx, ids)
	}

	key, err := g.apiKey(ctx, ids[0], false)
	if err != nil {
		return nil, g.unavailable("reading API key", err)
	}
	raw, err := g.post(ctx, key, body)
	if Classify(err) == ErrAuth {
		// The key rotated: read the current one from a book page, once.
		if key, err = g.apiKey(ctx, ids[0], true); err != nil {
			return nil, g.unavailable("refreshing API key", err)
		}
		raw, err = g.post(ctx, key, body)
	}
	if err != nil {
		return nil, g.unavailable("request", err)
	}

	var resp gqlResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("%w: unexpected response: %v", ErrGraphQLUnavailable, err)
	}

	// Errors scoped to one alias are answers about that book; an error
	// without a path is about the whole query (schema change, auth, ...).
	aliasErr := map[string]string{} // alias -> errorType
	for _, e := range resp.Errors {
		alias := ""
		if len(e.Path) > 0 {
			alias, _ = e.Path[0].(string)
		}
		if alias == "" {
			return nil, fmt.Errorf("%w: %s: %s", ErrGraphQLUnavailable, e.ErrorType, e.Message)
		}
		aliasErr[alias] = e.ErrorType
	}
	if resp.Data == nil && len(aliasErr) == 0 {
		return nil, fmt.Errorf("%w: response has no data", ErrGraphQLUnavailable)
	}

	out := make(map[string]*Book, len(ids))
	for i, id := range ids {
		alias := "b" + strconv.Itoa(i)
		b, present := resp.Data[alias] // AppSync sends data:null when every alias failed
		switch errType := aliasErr[alias]; {
		case errType == "RESOURCE_NOT_FOUND":
			out[id] = nil // Goodreads: "Book is invalid" (removed or merged)
		case errType != "" || !present:
			// an error of its own, or no answer: the caller tries the book page
		case b == nil:
			out[id] = nil // no such book
		default:
			book := b.fields().book(id)
			out[id] = &book
		}
	}
	return out, nil
}

// lookupSplit halves a batch whose query body is too large.
func (g *GraphQL) lookupSplit(ctx context.Context, ids []string) (map[string]*Book, error) {
	mid := len(ids) / 2
	left, err := g.Lookup(ctx, ids[:mid])
	if err != nil {
		return nil, err
	}
	right, err := g.Lookup(ctx, ids[mid:])
	if err != nil {
		return nil, err
	}
	for k, v := range right {
		left[k] = v
	}
	return left, nil
}

// unavailable passes transient errors through (the caller waits and retries)
// and wraps everything else as ErrGraphQLUnavailable (the caller falls back).
func (g *GraphQL) unavailable(what string, err error) error {
	if Classify(err) == ErrTransient || errors.Is(err, context.Canceled) {
		return err
	}
	return fmt.Errorf("%w: %s: %v", ErrGraphQLUnavailable, what, err)
}

// apiKey returns the cached key, or — when there is none or refresh is set —
// reads it from the book page of hintBookID and caches it in the store.
func (g *GraphQL) apiKey(ctx context.Context, hintBookID string, refresh bool) (string, error) {
	if !refresh {
		if g.key != "" {
			return g.key, nil
		}
		if v, err := g.keys.Setting(SettingGraphQLKey); err == nil && v != "" {
			g.key = v
			return v, nil
		}
	}
	page, _, err := g.gr.get(ctx, "/book/show/"+hintBookID, nil)
	if err != nil {
		return "", err
	}
	key, err := apiKeyFromBookPage(page)
	if err != nil {
		return "", err
	}
	if err := g.keys.SetSetting(SettingGraphQLKey, key); err != nil {
		return "", err
	}
	g.key = key
	return key, nil
}

// post sends one GraphQL request, paced and counted like other enrichment
// requests, retrying transient failures in place.
func (g *GraphQL) post(ctx context.Context, key string, body []byte) ([]byte, error) {
	var out []byte
	err := withRetry(ctx, g.gr.sleep, "POST graphql", func() error {
		if err := g.gr.pace(ctx); err != nil {
			return err
		}
		g.gr.requests.Add(1)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", key) // never logged: errors carry only the URL and status
		out, _, err = httpDo(g.http, req)
		return err
	})
	return out, err
}

// apiKeyFromBookPage reads the AppSync key a book page ships in __NEXT_DATA__.
func apiKeyFromBookPage(page []byte) (string, error) {
	data, err := nextData(page)
	if err != nil {
		return "", err
	}
	var next struct {
		Props struct {
			PageProps struct {
				APIKey string `json:"apiKey"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal(data, &next); err != nil {
		return "", fmt.Errorf("parsing book page data: %w", err)
	}
	if next.Props.PageProps.APIKey == "" {
		return "", errors.New("book page carries no GraphQL API key")
	}
	return next.Props.PageProps.APIKey, nil
}
