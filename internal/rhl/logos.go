package rhl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"hockey/internal/gamesheet"
)

// Logo is a team's logo image, fetched from GameSheet's CDN and served from
// this site (the CSP allows images from 'self' only, and visitors' browsers
// never contact the CDN).
type Logo struct {
	Type string // image/png, image/jpeg, image/webp or image/gif
	Data []byte
}

const (
	logoHost    = "imagedelivery.net" // Cloudflare Images, where GameSheet keeps logos
	logoVariant = "256"               // the size GameSheet's games data links to
	maxLogo     = 512 << 10
)

var logoTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// logoURL checks a logo URL from GameSheet's data: https on GameSheet's image
// CDN only. Standings give a bare image URL; games add the size (variant).
func logoURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != logoHost || u.User != nil || u.RawQuery != "" {
		return "", false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch len(parts) {
	case 2: // account/image
		u.Path += "/" + logoVariant
	case 3: // account/image/variant
	default:
		return "", false
	}
	return u.String(), true
}

// rememberLogos records the logo URLs in GameSheet's data. Games' URLs win:
// they name a size that exists.
func (s *Server) rememberLogos(st stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	add := func(t gamesheet.Team, override bool) {
		if t.ID == 0 || t.Logo == "" {
			return
		}
		if u, ok := logoURL(t.Logo); ok {
			if _, have := s.logoURLs[t.ID]; override || !have {
				s.logoURLs[t.ID] = u
			}
		}
	}
	for _, d := range st.Divisions {
		for _, t := range d.Teams {
			add(t.Team, false)
		}
	}
	for _, g := range st.Games {
		add(g.Home.Team, true)
		add(g.Visitor.Team, true)
	}
}

func (s *Server) hasLogo(id int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.logoURLs[id]
	return ok
}

func (s *Server) logo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	u, ok := s.logoURLs[id]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	res := s.logos.Get(r.Context(), u, func(ctx context.Context) (Logo, error) { return s.cfg.Logo(ctx, u) })
	if res.Err != nil {
		s.cfg.Log.Warn("logo unavailable", "team", id, "err", res.Err.Error())
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", res.Value.Type)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(res.Value.Data)
}

// FetchLogo returns a Config.Logo that downloads with client.
func FetchLogo(client *http.Client, userAgent string) func(ctx context.Context, url string) (Logo, error) {
	return func(ctx context.Context, u string) (Logo, error) {
		if _, ok := logoURL(u); !ok {
			return Logo{}, fmt.Errorf("not a GameSheet logo URL: %q", u)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return Logo{}, err
		}
		req.Header.Set("User-Agent", userAgent)
		res, err := client.Do(req)
		if err != nil {
			return Logo{}, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return Logo{}, fmt.Errorf("logo: HTTP %d", res.StatusCode)
		}
		mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
		if !logoTypes[mt] {
			return Logo{}, fmt.Errorf("logo: unexpected content type %q", mt)
		}
		b, err := io.ReadAll(io.LimitReader(res.Body, maxLogo+1))
		if err != nil {
			return Logo{}, err
		}
		if len(b) > maxLogo {
			return Logo{}, errors.New("logo: too large")
		}
		return Logo{Type: mt, Data: b}, nil
	}
}
