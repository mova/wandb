package shared_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/wandb/wandb/nexus/internal/shared"
)

func TestShortID(t *testing.T) {
	t.Run("shortid", func(t *testing.T) {
		item := shared.ShortID(8)
		assert.Equal(t, len(item), 8)
	})
}
