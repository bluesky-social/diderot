package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/bluesky-social/diderot/internal/clientcmd"
)

// TestServerBinaryCommands: the server's binary has the client commands too,
// and its own commands are the ones the client-only binary names as being
// in the server's image.
func TestServerBinaryCommands(t *testing.T) {
	var client, server []string
	for _, cmd := range buildRootCmd().Commands() {
		switch {
		case cmd.GroupID == clientcmd.GroupID:
			client = append(client, cmd.Name())
		case cmd.Name() != "help" && cmd.Name() != "completion":
			server = append(server, cmd.Name())
		}
	}
	assert.ElementsMatch(t, []string{"setup", "doctor", "ask", "sessions"}, client)
	assert.ElementsMatch(t, clientcmd.ServerCommands, server)
}
