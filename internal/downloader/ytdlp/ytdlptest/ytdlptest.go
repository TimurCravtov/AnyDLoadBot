// Package ytdlptest helps tests fake yt-dlp's output for ytdlp.Client.Fetch.
package ytdlptest

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var limitRe = regexp.MustCompile(`duration <= (\d+)`)

// Download is what a Fetch run of yt-dlp prints before the file path: the
// metadata line for meta (a JSON object), and whether the run's
// --match-filter lets the media through, in which case the fake should
// "download" a file and print its path after this.
func Download(args []string, meta string) (out []byte, pass bool) {
	line := "anydl-meta:" + meta + "\n"

	i := slices.Index(args, "--match-filter")
	if i < 0 {
		return []byte(line), true
	}
	filter := args[i+1]
	var m struct {
		Duration float64 `json:"duration"`
		IsLive   bool    `json:"is_live"`
	}
	_ = json.Unmarshal([]byte(meta), &m)

	pass = !m.IsLive
	if lim := limitRe.FindStringSubmatch(filter); lim != nil {
		max, _ := strconv.ParseFloat(lim[1], 64)
		switch {
		case m.Duration == 0:
			pass = pass && strings.Contains(filter, "!duration |")
		case m.Duration > max:
			pass = false
		}
	}
	return []byte(line), pass
}
