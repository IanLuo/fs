package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// envelope is what every fs invocation prints on stdout. A refusal is a normal
// outcome rather than a crash, so it is JSON too, and the exit code says which
// kind of answer it was.
type envelope struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// usageErr is a malformed command line: the caller asked for something fs does
// not understand, which is a different answer from "fs understood and refuses".
// It exits 2 so a script can tell the two apart.
type usageErr struct{ msg string }

func (e usageErr) Error() string { return e.msg }

// helpErr is a verb asked to describe itself: `fs <verb> --help`. It is not a
// refusal — the caller asked a question and fs answers it — so it exits 0.
// A wrapper that reads the exit code must not see help as a failure.
type helpErr struct{ verb string }

func (e helpErr) Error() string { return e.verb + ": help requested" }

// emit prints the envelope and returns the exit code: 0 for a result, 2 for a
// malformed command line, 1 for everything else. Every failure path is a
// refusal — there is no fallback store, no cached answer, and no partial success.
func emit(stdout, stderr io.Writer, data any, err error, usage string) int {
	if err == nil {
		write(stdout, envelope{OK: true, Data: data})
		return 0
	}
	var u usageErr
	if errors.As(err, &u) {
		if usage != "" {
			fmt.Fprint(stderr, usage)
		}
		write(stdout, envelope{OK: false, Error: err.Error()})
		return 2
	}
	write(stdout, envelope{OK: false, Error: err.Error()})
	return 1
}

func write(w io.Writer, e envelope) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(e)
}
