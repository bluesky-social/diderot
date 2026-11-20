package slack

import (
	"context"
	"testing"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
)

// A reference's optional "|label" ends at its own '>'. An unterminated one
// must not take the text up to a '>' on a later line as its label: that
// text would be dropped from what is indexed.
func TestResolveKeepsTextAfterAnUnterminatedLabel(t *testing.T) {
	pool := getDirDB(t)
	truncateDir(t, pool)
	d := newDirectoryWithAPI(pool, &fakeAPI{
		users:    map[string]*slack.User{"U0EXAMPLE02": makeUser("U0EXAMPLE02", "alex", "alex")},
		channels: map[string]*slack.Channel{},
	})

	for _, text := range []string{
		"<@U0EXAMPLE02|alex\nthe limit is 5\nand 5 > 3",
		"<#C0EXAMPLE01|general\nthe limit is 5\nand 5 > 3",
		"<!subteam^S0EXAMPLE01|team\nthe limit is 5\nand 5 > 3",
		"<https://example.com/a|docs\nthe limit is 5\nand 5 > 3",
	} {
		assert.Equal(t, text, d.Resolve(context.Background(), text))
	}

	// A terminated label still resolves.
	assert.Equal(t, "cc @alex and docs (https://example.com/a)",
		d.Resolve(context.Background(), "cc <@U0EXAMPLE02|someone> and <https://example.com/a|docs>"))
}
