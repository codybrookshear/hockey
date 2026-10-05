package gamesheet

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// GameSheet stats widgets link to their season, on either domain.
var seasonLinkRe = regexp.MustCompile(`(?:gamesheet\.app|gamesheetstats\.com)/seasons/(\d{1,9})\b`)

// SeasonLinks returns the GameSheet season IDs a page links to (the rink's
// standings page embeds one widget per season), in page order, without
// repeats.
func SeasonLinks(page []byte) []int {
	var ids []int
	for _, m := range seasonLinkRe.FindAllSubmatch(page, -1) {
		id, err := strconv.Atoi(string(m[1]))
		if err == nil && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// Current picks the season to show on date (YYYY-MM-DD) from a league's
// seasons:
//
//  1. an active one (GameSheet's flag) that has started, latest start first;
//  2. else the latest that has started: between seasons, last season's
//     final standings;
//  3. else the earliest yet to start.
//
// Seasons from other leagues and private ones are skipped. Dates are as
// entered in GameSheet, mistakes included (one season ends before it starts),
// which is why "active" comes first.
func Current(seasons []Season, leagueID int, date string) (Season, bool) {
	var started, upcoming []Season
	for _, s := range seasons {
		if s.LeagueID != leagueID || !s.Public {
			continue
		}
		if s.Start <= date {
			started = append(started, s)
		} else {
			upcoming = append(upcoming, s)
		}
	}
	slices.SortStableFunc(started, func(a, b Season) int { return cmp.Compare(b.Start, a.Start) })
	for _, s := range started {
		if s.Active {
			return s, true
		}
	}
	if len(started) > 0 {
		return started[0], true
	}
	slices.SortStableFunc(upcoming, func(a, b Season) int { return cmp.Compare(a.Start, b.Start) })
	if len(upcoming) > 0 {
		return upcoming[0], true
	}
	return Season{}, false
}

// Today is date's YYYY-MM-DD in loc, for Current.
func Today(now time.Time, loc *time.Location) string { return now.In(loc).Format(time.DateOnly) }
