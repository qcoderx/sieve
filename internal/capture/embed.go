package capture

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"sync"
)

// Script defines window.__sieve in the page. It is evaluated once per document
// after load and then called once per checkpoint.
//
//go:embed capture.js
var Script string

// Bootstrap is installed with Page.addScriptToEvaluateOnNewDocument so it runs
// before any page script, in every frame, on every navigation. It only hooks
// what cannot be recovered after the fact.
//
//go:embed bootstrap.js
var Bootstrap string

var (
	hashOnce sync.Once
	hashVal  string
)

// ScriptHash identifies the extraction script that produced a capture.
//
// It belongs in every trace. The script is the single largest determinant of
// what a render sees, and an artifact recorded under one version of it is not
// comparable with one recorded under another. Without this, a golden-file
// corpus silently drifts the first time the walk changes.
func ScriptHash() string {
	hashOnce.Do(func() {
		h := sha256.New()
		h.Write([]byte(Bootstrap))
		h.Write([]byte{0})
		h.Write([]byte(Script))
		hashVal = hex.EncodeToString(h.Sum(nil))[:16]
	})
	return hashVal
}
