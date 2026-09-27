package main

import (
	"context"
	"regexp"
	"testing"

	"github.com/codefly-dev/core/companions/python"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The default runtime image is the Python companion of the core this agent
// links, from the registry codefly publishes to. A core bump that moves the
// companion version fails here until the pin, and its digest, move with it.
func TestRuntimeImageIsTheLinkedCoreCompanion(t *testing.T) {
	companion, err := python.CompanionImage(context.Background())
	require.NoError(t, err)
	require.Equal(t, companion.Repository, runtimeImage.Repository)
	require.Equal(t, companion.Name, runtimeImage.Name)
	require.Equal(t, companion.Tag, runtimeImage.Tag,
		"re-pin runtimeImage (tag and index digest) to core's python companion")
}

// A tag alone lets the registry move the image under a built recipe, and a
// platform manifest digest would pin every target platform to one
// architecture. The pin is a digest; that it names the multi-platform index is
// what the comment on runtimeImage records and a reviewer checks.
func TestRuntimeImageIsDigestPinned(t *testing.T) {
	require.Regexp(t, regexp.MustCompile(`^sha256:[0-9a-f]{64}$`), runtimeImage.Digest)
	require.Equal(t, resources.ImageRegistry+"/python@"+runtimeImage.Digest, runtimeImage.FullName(),
		"the recipe's FROM must resolve by digest")
}
