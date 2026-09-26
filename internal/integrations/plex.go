package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Plex
// -----------------------------------------------------------------------------

// ErrNotInPlex means no Plex library covers the file; nothing to refresh.
var ErrNotInPlex = errors.New("file is not in a Plex library")

// Plex tells Plex Media Server that a file was replaced. Plex keeps the codec
// and stream details it read when the file was first added; a file replaced
// under the same name keeps the old details, and clients that trust them
// (direct play of the old codec) fail to play it. Refresh re-scans the folder
// and asks Plex to analyze the item again.
type Plex struct {
	base    string
	token   string
	pathMap map[string]string
	hc      *http.Client
	log     *slog.Logger

	// retries and retryDelay bound the wait for Plex to list a file whose
	// name changed (new container extension) after the folder scan.
	retries    int
	retryDelay time.Duration
}

// NewPlex builds a client. pathMap translates Codecsmith paths into the paths
// Plex sees (longest matching prefix wins); leave it empty when both see the
// media at the same paths.
func NewPlex(baseURL, token string, pathMap map[string]string, log *slog.Logger) *Plex {
	return &Plex{
		base:       strings.TrimRight(baseURL, "/"),
		token:      token,
		pathMap:    pathMap,
		hc:         &http.Client{Timeout: 30 * time.Second},
		log:        log,
		retries:    6,
		retryDelay: 10 * time.Second,
	}
}

// Refresh updates Plex after oldPath was replaced by newPath (the same path
// unless the container changed).
func (p *Plex) Refresh(ctx context.Context, oldPath, newPath string) error {
	oldPath, newPath = p.mapPath(oldPath), p.mapPath(newPath)
	secs, err := p.sections(ctx)
	if err != nil {
		return err
	}
	sec, ok := sectionFor(secs, newPath)
	if !ok {
		return ErrNotInPlex
	}
	itemType := map[string]string{"movie": "1", "show": "4"}[sec.Type]
	if itemType == "" {
		return ErrNotInPlex
	}

	// Partial scan of the folder so a renamed file is picked up.
	if err := p.do(ctx, http.MethodGet, "/library/sections/"+sec.Key+"/refresh",
		url.Values{"path": {path.Dir(newPath)}}, nil); err != nil {
		return fmt.Errorf("plex scan: %w", err)
	}

	for attempt := 0; ; attempt++ {
		key, err := p.find(ctx, sec.Key, itemType, newPath)
		if err == nil && key == "" && oldPath != newPath {
			key, err = p.find(ctx, sec.Key, itemType, oldPath)
		}
		if err != nil {
			return err
		}
		if key != "" {
			if err := p.do(ctx, http.MethodPut, "/library/metadata/"+key+"/analyze", nil, nil); err != nil {
				return fmt.Errorf("plex analyze: %w", err)
			}
			p.log.Info("plex re-analyzed", "rating_key", key, "file", newPath)
			return nil
		}
		if attempt >= p.retries {
			return fmt.Errorf("plex does not list %s", newPath)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.retryDelay):
		}
	}
}

// mapPath applies the longest matching path_map prefix.
func (p *Plex) mapPath(file string) string {
	prefixes := make([]string, 0, len(p.pathMap))
	for from := range p.pathMap {
		prefixes = append(prefixes, from)
	}
	sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
	for _, from := range prefixes {
		if under(file, from) {
			return strings.TrimRight(p.pathMap[from], "/") + strings.TrimPrefix(file, strings.TrimRight(from, "/"))
		}
	}
	return file
}

// under reports whether file lies inside dir (not merely sharing a prefix:
// /media/movies-4k is not under /media/movies).
func under(file, dir string) bool {
	dir = strings.TrimRight(dir, "/")
	return dir != "" && strings.HasPrefix(file, dir+"/")
}

type plexSection struct {
	Key       string
	Type      string
	Locations []string
}

func sectionFor(secs []plexSection, file string) (plexSection, bool) {
	best, bestLen := plexSection{}, -1
	for _, s := range secs {
		for _, loc := range s.Locations {
			if under(file, loc) && len(loc) > bestLen {
				best, bestLen = s, len(loc)
			}
		}
	}
	return best, bestLen >= 0
}

func (p *Plex) sections(ctx context.Context) ([]plexSection, error) {
	var resp struct {
		MediaContainer struct {
			Directory []struct {
				Key      string `json:"key"`
				Type     string `json:"type"`
				Location []struct {
					Path string `json:"path"`
				} `json:"Location"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := p.do(ctx, http.MethodGet, "/library/sections", nil, &resp); err != nil {
		return nil, fmt.Errorf("plex sections: %w", err)
	}
	var out []plexSection
	for _, d := range resp.MediaContainer.Directory {
		s := plexSection{Key: d.Key, Type: d.Type}
		for _, l := range d.Location {
			s.Locations = append(s.Locations, l.Path)
		}
		out = append(out, s)
	}
	return out, nil
}

// find returns the ratingKey of the item whose file is exactly file. Plex's
// file filter matches substrings, so every returned part is checked.
func (p *Plex) find(ctx context.Context, section, itemType, file string) (string, error) {
	var resp struct {
		MediaContainer struct {
			Metadata []struct {
				RatingKey string `json:"ratingKey"`
				Media     []struct {
					Part []struct {
						File string `json:"file"`
					} `json:"Part"`
				} `json:"Media"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	q := url.Values{"type": {itemType}, "file": {file}}
	if err := p.do(ctx, http.MethodGet, "/library/sections/"+section+"/all", q, &resp); err != nil {
		return "", fmt.Errorf("plex lookup: %w", err)
	}
	for _, m := range resp.MediaContainer.Metadata {
		for _, media := range m.Media {
			for _, part := range media.Part {
				if part.File == file {
					return m.RatingKey, nil
				}
			}
		}
	}
	return "", nil
}

func (p *Plex) do(ctx context.Context, method, endpoint string, q url.Values, out any) error {
	u := p.base + endpoint
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Token", p.token)
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d", method, endpoint, resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
