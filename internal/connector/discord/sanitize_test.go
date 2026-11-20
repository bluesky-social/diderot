package discord

import "testing"

// zwsp is the zero-width space Sanitize inserts to defang "@everyone"
// and "@here", written as its raw UTF-8 bytes (not a \u escape) so the
// literal character can't end up baked into this source file by a
// find-and-replace or an editor's Unicode auto-conversion.
const zwsp = "\xe2\x80\x8b"

// TestSanitizeMatchesEveryoneAndHereCaseInsensitively covers a gap found
// while checking surface.Sanitize for the same defects as
// sanitizeSlackMarkdown (internal/connector/slack/sanitize.go): the old
// implementation matched "@everyone"/"@here" with an exact-case
// strings.Replacer, so a mixed-case form like "@Everyone" or "@HERE"
// passed through untouched. Trigger: Sanitize is called on every answer
// and label this surface posts (PostReply, PostChunk, Upload's title,
// the asker display name and question text in askcommands.go), and
// model output or an asker's display name is not guaranteed to be
// lowercase.
//
// Note this is a cosmetic gap, not a notification bypass: every post
// through this surface also sets AllowedMentions to an empty parse list
// (noMentions in askapi.go), Discord's own server-side control that
// disables mention resolution independent of message text or case, so
// no casing of "@everyone"/"@here" was ever capable of actually pinging
// anyone here. It is still worth closing so the text doesn't even look
// like a live mention, matching the function's stated purpose.
func TestSanitizeMatchesEveryoneAndHereCaseInsensitively(t *testing.T) {
	s := &surface{}
	cases := []struct{ in, want string }{
		{"@everyone", "@" + zwsp + "everyone"},
		{"@Everyone", "@" + zwsp + "Everyone"},
		{"@EVERYONE", "@" + zwsp + "EVERYONE"},
		{"@here", "@" + zwsp + "here"},
		{"@Here", "@" + zwsp + "Here"},
		{"@HERE", "@" + zwsp + "HERE"},
		{"hey @EveryOne check this", "hey @" + zwsp + "EveryOne check this"},
		{"no mention here at all", "no mention here at all"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := s.Sanitize(tc.in); got != tc.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
