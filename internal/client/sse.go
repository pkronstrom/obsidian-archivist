package client

import (
	"bufio"
	"bytes"
	"io"
)

// sseDecoder reads Server-Sent Events, returning the payload of each `data:`
// block. Comments (": keep-alive") and blank lines are skipped.
//
// Hand-rolled rather than pulled in: the subset we need is twenty lines, and
// the server only ever emits single-line data blocks. Multi-line data is
// handled anyway because the spec allows it and a future event could grow.
type sseDecoder struct {
	r   *bufio.Reader
	buf bytes.Buffer
}

func newSSEDecoder(r io.Reader) *sseDecoder {
	// A generous line limit: one event can carry MaxInlineChanges entries.
	return &sseDecoder{r: bufio.NewReaderSize(r, 1<<20)}
}

// next blocks until a complete event arrives, then returns its data payload.
func (d *sseDecoder) next() ([]byte, error) {
	d.buf.Reset()
	for {
		line, err := d.r.ReadBytes('\n')
		if err != nil {
			// A trailing fragment without a newline is not a usable event.
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")

		// A blank line terminates the event.
		if len(line) == 0 {
			if d.buf.Len() > 0 {
				out := make([]byte, d.buf.Len())
				copy(out, d.buf.Bytes())
				return out, nil
			}
			continue // blank between events, or after a comment
		}
		if line[0] == ':' {
			continue // comment, e.g. the keep-alive
		}
		if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			d.buf.Write(bytes.TrimPrefix(after, []byte(" ")))
			continue
		}
		// Other fields (event:, id:, retry:) are not used by this protocol.
	}
}
