package accountexport

import (
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
)

// Protect wraps dst so that what is written to it is encrypted with age in
// passphrase (scrypt) mode. A person opens the result with the stock tool:
// `age --decrypt -o <stem>.tar <stem>.tar.age`, which prompts for the
// passphrase. Close the returned writer to finish the file; an unclosed one
// is missing its last chunk and does not open.
func Protect(dst io.Writer, passphrase string) (io.WriteCloser, error) {
	if passphrase == "" {
		return nil, errors.New("accountexport: a protected file needs a passphrase")
	}
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, fmt.Errorf("accountexport: protect: %w", err)
	}
	w, err := age.Encrypt(dst, r)
	if err != nil {
		return nil, fmt.Errorf("accountexport: protect: %w", err)
	}
	return w, nil
}

// Unprotect opens a file Protect wrote. A wrong passphrase fails here, before
// any byte is returned. age authenticates every chunk, so a changed byte later
// in the file fails the Read that reaches it.
func Unprotect(src io.Reader, passphrase string) (io.Reader, error) {
	if passphrase == "" {
		return nil, errors.New("accountexport: opening a protected file needs its passphrase")
	}
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, fmt.Errorf("accountexport: unprotect: %w", err)
	}
	r, err := age.Decrypt(src, id)
	if err != nil {
		return nil, fmt.Errorf("accountexport: unprotect: %w", err)
	}
	return r, nil
}
