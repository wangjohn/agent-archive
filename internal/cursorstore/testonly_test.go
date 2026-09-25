package cursorstore

import "context"

// Helpers only tests use, kept out of the production files so deadcode
// (golang.org/x/tools/cmd/deadcode) reports only code that is really dead.

// ReadComposer reads one chat through a Reader of its own and removes any
// snapshot before returning; see Reader.ReadComposer.
func ReadComposer(ctx context.Context, dbPath, composerID string) (Composer, Signature, error) {
	r := NewReader(dbPath)
	c, sig, err := func() (Composer, Signature, error) {
		defer func() { _ = r.Close() }()
		return r.ReadComposer(ctx, composerID)
	}()
	return c, sig, err
}
