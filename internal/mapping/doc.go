// Package mapping traces a DriftProposal back to the Git file that defines
// the drifted resource and computes the edited file. It works on an fs.FS and
// plain bytes: no Kubernetes client and no Git access.
//
// The steps are Locate (find the single YAML document), Apply (edit that
// document), Verify (prove the edit reproduces the proposal exactly) and
// UnifiedDiff (show the result). Map runs all of them.
//
// Edits are made so that a reviewer sees only the lines that matter: the
// document is parsed with the YAML Node API for positions, and only the
// affected "key: value" entry is re-rendered in place, keeping its comments
// and quoting style. Everything else in the file, including other documents,
// stays byte for byte as it was.
package mapping

import "errors"

var (
	// ErrNotFound means no document in the source defines the resource.
	ErrNotFound = errors.New("mapping: resource not found in the source")
	// ErrAmbiguous means more than one document defines the resource.
	ErrAmbiguous = errors.New("mapping: resource is defined more than once in the source")
	// ErrCannotApply means a change could not be applied to the document.
	ErrCannotApply = errors.New("mapping: change cannot be applied")
	// ErrUnsupportedFileFormat means the file that defines the resource is in
	// a format that cannot be edited yet (JSON).
	ErrUnsupportedFileFormat = errors.New("mapping: unsupported file format")
	// ErrInvalidGlob means a directory include/exclude pattern does not compile.
	ErrInvalidGlob = errors.New("mapping: invalid include/exclude pattern")
	// ErrVerificationFailed means the edited document does not reproduce the
	// proposal exactly.
	ErrVerificationFailed = errors.New("mapping: verification failed")
)
