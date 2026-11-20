package qdrantstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCollectionName(t *testing.T) {
	assert.Equal(t, "diderot_chunks_local_embed_1024", CollectionName(" local-embed ", 1024))
	assert.Equal(t, "diderot_chunks_baai_bge_m3_1024", CollectionName("BAAI/bge-m3", 1024))
	assert.Equal(t, "diderot_chunks_nomic_embed_text_v1_5_768", CollectionName("nomic-embed-text:v1.5", 768))
	assert.Equal(t, "diderot_chunks_text_embedding_3_small_1536", CollectionName("text-embedding-3-small", 1536))
}

func TestValidCollectionName(t *testing.T) {
	assert.True(t, ValidCollectionName("diderot_chunks_local_embed_v2"))
	assert.False(t, ValidCollectionName(""))
	assert.False(t, ValidCollectionName("a b"))
	assert.False(t, ValidCollectionName("../x"))
}
