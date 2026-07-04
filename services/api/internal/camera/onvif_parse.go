package camera

import "regexp"

// parseProfiles extracts the (token, name) pairs from a
// tptz:GetProfilesResponse. Each profile looks like:
//
//	<trt:Profiles token="Profile_1" fixed="true">
//	  <tt:Name>mainStream</tt:Name>
//	  ...
//	</trt:Profiles>
//
// Some vendors use <MediaProfile> instead of <Profiles>; some use
// attribute names with namespaces. We accept the three shapes we
// have actually seen in the wild (Hikvision, Dahua, Uniview) and
// silently ignore the rest.
var (
	profileRe = regexp.MustCompile(`<(?:trt:Profiles|Profiles|MediaProfile)\b[^>]*token="([^"]+)"[^>]*>\s*<(?:tt:Name|Name)>([^<]+)</`)
	presetRe  = regexp.MustCompile(`<(?:tptz:Preset|Preset)\b[^>]*token="([^"]+)"[^>]*>\s*<(?:tt:Name|Name)>([^<]+)</`)
)

func parseProfiles(xml string) []Profile {
	matches := profileRe.FindAllStringSubmatch(xml, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Profile, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		tok, name := m[1], m[2]
		if seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, Profile{Token: tok, Name: name})
	}
	return out
}

func parsePresets(xml string) []Preset {
	matches := presetRe.FindAllStringSubmatch(xml, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Preset, 0, len(matches))
	seen := map[string]bool{}
	for _, m := range matches {
		tok, name := m[1], m[2]
		if seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, Preset{Token: tok, Name: name})
	}
	return out
}

// clamp returns v clamped to [0,1]. Negative or NaN → 0; > 1 → 1.
func clamp(v float64) float64 {
	switch {
	case v != v: // NaN
		return 0
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}
