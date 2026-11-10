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
	"unicode"

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

// teamKey is a team's name for matching across seasons: letters and digits,
// lower case ("Nova Cains" and "Novacains" are one team).
func teamKey(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// borrowLogos finds logos for st's teams that have none in GameSheet's data,
// from a team of the same name in the league's other seasons, latest first.
// Each season's logos (from its standings) are cached for a day, and fetched
// only while a team lacks one.
func (s *Server) borrowLogos(ctx context.Context, st stats) {
	missing := map[int]string{} // team ID → teamKey
	s.mu.Lock()
	note := func(t gamesheet.Team) {
		if _, have := s.logoURLs[t.ID]; !have && t.ID != 0 && teamKey(t.Title) != "" {
			missing[t.ID] = teamKey(t.Title)
		}
	}
	for _, d := range st.Divisions {
		for _, t := range d.Teams {
			note(t.Team)
		}
	}
	for _, g := range st.Games {
		note(g.Home.Team)
		note(g.Visitor.Team)
	}
	s.mu.Unlock()

	for _, other := range st.Seasons { // latest first
		if len(missing) == 0 {
			return
		}
		if other.ID == st.Season.ID {
			continue
		}
		id := other.ID
		r := s.seasonLogos.Get(ctx, strconv.Itoa(id), func(ctx context.Context) (map[string]string, error) {
			divs, err := s.cfg.Source.Standings(ctx, id)
			if err != nil {
				return nil, err
			}
			out := map[string]string{}
			for _, d := range divs {
				for _, t := range d.Teams {
					if u, ok := logoURL(t.Team.Logo); ok && teamKey(t.Team.Title) != "" {
						out[teamKey(t.Team.Title)] = u
					}
				}
			}
			return out, nil
		})
		if r.Err != nil {
			s.cfg.Log.Warn("other season's logos unavailable", "season", id, "err", r.Err.Error())
			continue
		}
		s.mu.Lock()
		for team, key := range missing {
			if u, ok := r.Value[key]; ok {
				s.logoURLs[team] = u
				delete(missing, team)
			}
		}
		s.mu.Unlock()
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
