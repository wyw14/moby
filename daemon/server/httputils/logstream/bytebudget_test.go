package logstream

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestByteBudget(t *testing.T) {
	t.Run("zero is unlimited", func(t *testing.T) {
		b := byteBudget{limit: 0}
		assert.Assert(t, b.take(1<<30))
		assert.Assert(t, b.take(1<<30))
	})
	t.Run("negative is unlimited", func(t *testing.T) {
		b := byteBudget{limit: -1}
		assert.Assert(t, b.take(1<<30))
	})
	t.Run("exact fit", func(t *testing.T) {
		b := byteBudget{limit: 5}
		assert.Assert(t, b.take(2))
		assert.Assert(t, b.take(3))
	})
	t.Run("next record does not fit", func(t *testing.T) {
		b := byteBudget{limit: 4}
		assert.Assert(t, b.take(2))
		// A record that does not fit is rejected without consuming budget.
		assert.Assert(t, !b.take(3))
		assert.Assert(t, !b.take(100))
	})
	t.Run("empty record always fits", func(t *testing.T) {
		b := byteBudget{limit: 0}
		assert.Assert(t, b.take(0))
	})
}
