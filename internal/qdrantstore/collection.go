package qdrantstore

import (
	"fmt"
	"regexp"
	"strings"
)

var collectionNameUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// CollectionName is the collection an embedding model's vectors go in: named
// after the model and the dimension, so choosing another model or dimension
// starts a new collection instead of writing incompatible vectors into an
// existing one.
func CollectionName(model string, dimensions int) string {
	name := strings.Trim(collectionNameUnsafe.ReplaceAllString(strings.ToLower(model), "_"), "_")
	return fmt.Sprintf("diderot_chunks_%s_%d", name, dimensions)
}

var collectionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)

// ValidCollectionName reports whether name can be an existing collection's
// name to adopt: letters, digits, _ or -.
func ValidCollectionName(name string) bool { return collectionNamePattern.MatchString(name) }
