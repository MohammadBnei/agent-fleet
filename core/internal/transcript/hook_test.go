package transcript

import "testing"

// A store without the decisions hook cannot be built (docs/adr/0082 §5): it
// would write no decisions, so nothing would ever be pending.
func TestNewPostgresStore_RefusesNoHook(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewPostgresStore(pool, nil) returned a store")
		}
	}()
	NewPostgresStore(nil, nil)
}
