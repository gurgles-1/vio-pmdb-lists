package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const tmdbBaseURL = "https://api.themoviedb.org/3"
const tmdbImageBase = "https://image.tmdb.org/t/p"

// tmdbTitle is the normalized metadata the sync needs for one TMDB record.
type tmdbTitle struct {
	TMDBID      int
	MediaType   string // "movie" or "tv"
	IMDbID      string
	Title       string
	Year        int
	Overview    string
	PosterURL   string
	BackdropURL string
	Genres      []string
	Runtime     int
	Episodes    []tmdbEpisode
}

type tmdbEpisode struct {
	Season   int
	Episode  int
	Title    string
	Overview string
	AirDate  string
	Runtime  int
	StillURL string
}

type tmdbClient struct {
	http   *http.Client
	apiKey string
	jwt    bool
}

func newTMDBClient(apiKey string) *tmdbClient {
	// v4 read tokens are JWTs; v3 keys go in the query string.
	jwt := strings.HasPrefix(strings.TrimSpace(apiKey), "eyJ")
	return &tmdbClient{
		http:   &http.Client{Timeout: 30 * time.Second},
		apiKey: strings.TrimSpace(apiKey),
		jwt:    jwt,
	}
}

func (c *tmdbClient) get(path string) (map[string]any, error) {
	url := tmdbBaseURL + path
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	if !c.jwt {
		url += sep + "api_key=" + c.apiKey
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.jwt {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tmdb GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb GET %s: unexpected status %d", path, resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tmdb GET %s: decode: %w", path, err)
	}
	return out, nil
}

// fetchTitle resolves one TMDB movie or TV record, including its IMDb id,
// metadata, and (for series) the full episode list of regular seasons.
func (c *tmdbClient) fetchTitle(tmdbID int, mediaType string) (*tmdbTitle, error) {
	kind := "movie"
	if mediaType == "tv" {
		kind = "tv"
	}
	body, err := c.get(fmt.Sprintf("/%s/%d?append_to_response=external_ids", kind, tmdbID))
	if err != nil {
		return nil, err
	}
	t := &tmdbTitle{TMDBID: tmdbID, MediaType: mediaType}
	if ext, _ := body["external_ids"].(map[string]any); ext != nil {
		t.IMDbID = strings.TrimSpace(strFromAny(ext["imdb_id"]))
	}
	if kind == "movie" {
		t.Title = strFromAny(body["title"])
		t.Year = yearFromDate(strFromAny(body["release_date"]))
		if r, ok := body["runtime"].(float64); ok {
			t.Runtime = int(r)
		}
	} else {
		t.Title = strFromAny(body["name"])
		t.Year = yearFromDate(strFromAny(body["first_air_date"]))
		if rt, ok := body["episode_run_time"].([]any); ok && len(rt) > 0 {
			if n, ok := rt[0].(float64); ok {
				t.Runtime = int(n)
			}
		}
	}
	t.Overview = strFromAny(body["overview"])
	if p := strFromAny(body["poster_path"]); p != "" {
		t.PosterURL = tmdbImageBase + "/w500" + p
	}
	if b := strFromAny(body["backdrop_path"]); b != "" {
		t.BackdropURL = tmdbImageBase + "/w780" + b
	}
	if genres, ok := body["genres"].([]any); ok {
		for _, g := range genres {
			if gm, ok := g.(map[string]any); ok {
				if name := strings.TrimSpace(strFromAny(gm["name"])); name != "" {
					t.Genres = append(t.Genres, name)
				}
			}
		}
	}
	if kind == "tv" {
		eps, err := c.fetchEpisodes(tmdbID, body)
		if err != nil {
			return nil, err
		}
		t.Episodes = eps
	}
	return t, nil
}

// fetchEpisodes pulls every regular season (season_number >= 1) of a series.
func (c *tmdbClient) fetchEpisodes(tmdbID int, tvBody map[string]any) ([]tmdbEpisode, error) {
	var seasons []int
	if raw, ok := tvBody["seasons"].([]any); ok {
		for _, s := range raw {
			if sm, ok := s.(map[string]any); ok {
				if n, ok := sm["season_number"].(float64); ok && int(n) >= 1 {
					seasons = append(seasons, int(n))
				}
			}
		}
	}
	if len(seasons) == 0 {
		if n, ok := tvBody["number_of_seasons"].(float64); ok {
			for s := 1; s <= int(n); s++ {
				seasons = append(seasons, s)
			}
		}
	}
	var out []tmdbEpisode
	for _, s := range seasons {
		body, err := c.get(fmt.Sprintf("/tv/%d/season/%d", tmdbID, s))
		if err != nil {
			return nil, fmt.Errorf("season %d: %w", s, err)
		}
		raw, _ := body["episodes"].([]any)
		for _, e := range raw {
			em, _ := e.(map[string]any)
			if em == nil {
				continue
			}
			ep := tmdbEpisode{
				Season:   intFromAny(em["season_number"]),
				Episode:  intFromAny(em["episode_number"]),
				Title:    strFromAny(em["name"]),
				Overview: strFromAny(em["overview"]),
				AirDate:  strFromAny(em["air_date"]),
				Runtime:  intFromAny(em["runtime"]),
			}
			if st := strFromAny(em["still_path"]); st != "" {
				ep.StillURL = tmdbImageBase + "/w300" + st
			}
			if ep.Season >= 1 && ep.Episode >= 1 {
				out = append(out, ep)
			}
		}
		// Be polite to the TMDB rate limiter on long series.
		time.Sleep(150 * time.Millisecond)
	}
	return out, nil
}

func yearFromDate(date string) int {
	if len(date) >= 4 {
		y, err := strconv.Atoi(date[:4])
		if err == nil && y > 1800 && y < 2200 {
			return y
		}
	}
	return 0
}
