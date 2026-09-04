package ops

import "bytes"

// CountLines counts lines the way a line scanner does: a trailing newline ends
// the last line rather than starting an empty one.
//
// It has to agree with Search, which uses bufio.Scanner. The same payload
// reporting 1000 lines from one tool and 1001 from another is worse than
// either number being slightly off — the reader cannot tell which to believe.
//
// It lives here rather than in record because two packages need it: record
// reports it from read_result and search_result, and the gateway reports it
// for a payload it withheld. A second implementation is how the two drifted
// apart the first time.
func CountLines(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	n := bytes.Count(buf, []byte{'\n'})
	if buf[len(buf)-1] != '\n' {
		n++
	}
	return n
}
