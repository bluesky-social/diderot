package slack

import (
	"time"

	"github.com/bluesky-social/diderot/internal/ask"
)

// askerInfoFromUserInfo is the single Slack-profile projection used by both
// direct Slack asks and authenticated HTTP asks enriched by email. Keeping the
// conversion here prevents the two transports from making different choices
// about display names, handles, canonical email, or timezone fields.
func askerInfoFromUserInfo(ui UserInfo) *ask.AskerInfo {
	display := pickName(ui.RealName, pickName(ui.DisplayName, ui.Name))
	if display == "" && ui.Email == "" && ui.UserID == "" {
		return nil
	}
	info := &ask.AskerInfo{
		DisplayName: display,
		Handle:      ui.DisplayName,
		Email:       ui.Email,
		SlackUserID: ui.UserID,
		TZ:          ui.TZ,
		TZLabel:     ui.TZLabel,
	}
	if ui.TZ != "" {
		if loc, err := time.LoadLocation(ui.TZ); err == nil {
			info.LocalTime = time.Now().In(loc)
		}
	}
	return info
}
