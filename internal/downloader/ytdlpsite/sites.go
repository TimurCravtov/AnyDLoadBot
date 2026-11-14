package ytdlpsite

import (
	"net/url"
	"regexp"
	"strings"
)

var (
	digits = regexp.MustCompile(`^[0-9]{5,25}$`)
	// Short codes and post IDs are alphanumeric (TikTok share codes, Reddit IDs).
	code = regexp.MustCompile(`^[A-Za-z0-9_-]{3,40}$`)
)

// segments splits a URL path into its non-empty parts.
func segments(u *url.URL) []string {
	return strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
}

// TikTok matches videos on tiktok.com, including the vm./vt. short links and
// /t/ share links. Profiles and other pages are not matched.
func TikTok() Site {
	return Site{
		Name:        "TikTok",
		Description: "videos",
		NoCaption:   true, // TikTok's title is the whole video description
		Match: func(u *url.URL) bool {
			host, parts := strings.ToLower(u.Hostname()), segments(u)
			switch host {
			case "vm.tiktok.com", "vt.tiktok.com":
				return len(parts) == 1 && code.MatchString(parts[0])
			case "tiktok.com", "www.tiktok.com", "m.tiktok.com":
				switch {
				case len(parts) >= 3 && strings.HasPrefix(parts[0], "@") && parts[1] == "video":
					return digits.MatchString(parts[2])
				case len(parts) >= 2 && parts[0] == "t":
					return code.MatchString(parts[1])
				}
			}
			return false
		},
	}
}

// Reddit matches video posts on reddit.com, plus v.redd.it and redd.it links.
// Reddit refuses anonymous requests, so this needs a logged-in cookies.txt.
func Reddit() Site {
	return Site{
		Name:        "Reddit",
		Description: "video posts",
		Match: func(u *url.URL) bool {
			host, parts := strings.ToLower(u.Hostname()), segments(u)
			switch {
			case host == "v.redd.it" || host == "redd.it":
				return len(parts) == 1 && code.MatchString(parts[0])
			case host == "reddit.com" || strings.HasSuffix(host, ".reddit.com"):
				// /r/<sub>/comments/<id>/..., /comments/<id>, /user/<name>/comments/<id>
				// and the /r/<sub>/s/<code> share links.
				for i := 0; i+1 < len(parts); i++ {
					switch parts[i] {
					case "comments", "s":
						return code.MatchString(parts[i+1])
					}
				}
			}
			return false
		},
	}
}
