package tlscertificates_test

import (
	"testing"

	"cdr.dev/coder-cli/wsnet/tlscertificates"
	"github.com/stretchr/testify/require"
)

func TestLoadDirectory(t *testing.T) {
	t.Parallel()

	t.Run("ValidDirectory", func(t *testing.T) {
		// Load the testdata certs
		certs, err := tlscertificates.LoadCertsFromDirectory("testdata")
		require.NoError(t, err)
		// ca-certificates.crt is 6 certs
		// Comodo is 1
		// VeriSign is 1
		require.Len(t, certs, 6+1+1)
	})
}