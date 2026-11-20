package ask

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestValidateArtifactSelectionShapeStripsEmbeddedNulBytes is a regression
// test: an embedded NUL byte (U+0000) in an artifact selection's title or
// alt text passed validateArtifactSelectionShape untouched, even though
// ask_result_artifacts.title and .alt_text are plain Postgres text columns
// that reject a literal NUL byte outright (SQLSTATE 22021, "invalid byte
// sequence for encoding"). These strings come from the same decoded
// structured final answer as the assistant's text (respond.go's
// decodeFinalResponse), so they can carry a NUL for the same reason.
//
// Trigger: an ArtifactSelection whose Title or AltText contains a literal
// NUL byte.
func TestValidateArtifactSelectionShapeStripsEmbeddedNulBytes(t *testing.T) {
	id := uuid.New()
	selections := []ArtifactSelection{
		{ArtifactID: id, Title: "API error \x00 rate", AltText: "Panel showing the \x00 error rate."},
	}

	err := validateArtifactSelectionShape(selections)
	require.NoError(t, err)

	require.Equal(t, "API error  rate", selections[0].Title,
		"the NUL byte must be stripped, not left in place to break the text-column insert")
	require.Equal(t, "Panel showing the  error rate.", selections[0].AltText,
		"the NUL byte must be stripped, not left in place to break the text-column insert")
}

// TestAppendAgenticMessagesWithArtifactsPersistsTitleAndAltTextWithEmbeddedNulBytes
// is a regression test against the real database: committing a result
// artifact whose title or alt text contains an embedded NUL byte used to
// fail the INSERT into ask_result_artifacts, a plain text column, with
// Postgres rejecting the raw NUL byte (SQLSTATE 22021). Because the
// artifact insert runs in the same transaction as the terminal assistant
// message, the whole turn (answer included) was discarded.
func TestAppendAgenticMessagesWithArtifactsPersistsTitleAndAltTextWithEmbeddedNulBytes(t *testing.T) {
	pool := testPool(t)
	mgr := NewManager(pool, nil)
	store := NewArtifactStore(pool, &memoryArtifactBlob{})
	sessionID := newAgenticSession(t, mgr)
	record, err := store.PersistPanelImage(context.Background(), sessionID, "call-panel", testPanelPNG(t))
	require.NoError(t, err)

	selection := ArtifactSelection{
		ArtifactID: record.ID,
		Title:      "API error \x00 rate",
		AltText:    "Panel showing the \x00 error rate.",
	}
	err = mgr.AppendAgenticMessagesWithArtifacts(context.Background(), sessionID, []StoredMessage{
		{Turn: 0, Message: NewAgenticUserText("show the panel")},
		{Turn: 1, Message: NewAgenticAssistantText("Here it is")},
	}, 1, []ArtifactSelection{selection})
	require.NoError(t, err, "a title/alt_text ValidateSelections accepts must be durably persistable; "+
		"instead the whole turn was discarded on an unvalidated embedded NUL byte")

	descriptors, err := store.LoadResultArtifacts(context.Background(), sessionID, 1)
	require.NoError(t, err)
	require.Len(t, descriptors, 1)
	require.Equal(t, "API error  rate", descriptors[0].Title,
		"the NUL byte must be stripped, not left in place to break the text-column insert")
	require.Equal(t, "Panel showing the  error rate.", descriptors[0].AltText,
		"the NUL byte must be stripped, not left in place to break the text-column insert")
}
