package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const pmdbBaseURL = "https://publicmetadb.com"

// pmdbItem is one entry of a PublicMetaDB list: just enough to find the
// title on TMDB. Metadata and artwork come from TMDB so the plugin does not
// depend on PMDB's item payload shape beyond these two fields.
type pmdbItem struct {
	TMDBID    int
	MediaType string // "movie" or "tv"
}

type pmdbClient struct {
	http   *http.Client
	apiKey string
}

func newPMDBClient(apiKey string) *pmdbClient {
	return &pmdbClient{
		http:   &http.Client{Timeout: 30 * time.Second},
		apiKey: apiKey,
	}
}

func (c *pmdbClient) get(path string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, pmdbBaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pmdb GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pmdb GET %s: unexpected status %d", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("pmdb GET %s: decode: %w", path, err)
	}
	return out, nil
}

// listItems fetches every item of a PMDB list, following pagination.
func (c *pmdbClient) listItems(listID string) ([]pmdbItem, error) {
	var items []pmdbItem
	page := 1
	for {
		body, err := c.get(fmt.Sprintf("/api/external/lists/%s/items?page=%d&perPage=100", listID, page))
		if err != nil {
			return nil, err
		}
		rawItems, _ := body["items"].([]any)
		for _, raw := range rawItems {
			m, _ := raw.(map[string]any)
			if m == nil {
				continue
			}
			it := pmdbItem{
				TMDBID:    intFromAny(firstPresent(m, "tmdb_id", "tmdbId")),
				MediaType: strings.ToLower(strings.TrimSpace(strFromAny(firstPresent(m, "media_type", "mediaType")))),
			}
			// Some payloads nest the identifiers under "media".
			if it.TMDBID == 0 || it.MediaType == "" {
				if nested, _ := m["media"].(map[string]any); nested != nil {
					if it.TMDBID == 0 {
						it.TMDBID = intFromAny(firstPresent(nested, "tmdb_id", "tmdbId"))
					}
					if it.MediaType == "" {
						it.MediaType = strings.ToLower(strings.TrimSpace(strFromAny(firstPresent(nested, "media_type", "mediaType"))))
					}
				}
			}
			if it.MediaType == "tv" || it.MediaType == "show" || it.MediaType == "series" {
				it.MediaType = "tv"
			}
			if it.TMDBID <= 0 || (it.MediaType != "movie" && it.MediaType != "tv") {
				continue
			}
			items = append(items, it)
		}
		totalPages := intFromAny(body["totalPages"])
		if totalPages < 1 {
			totalPages = 1
		}
		if page >= totalPages {
			break
		}
		page++
	}
	return items, nil
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func intFromAny(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	}
	return 0
}

func strFromAny(v any) string {
	s, _ := v.(string)
	return s
}
